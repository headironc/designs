package watchdog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

const testJob = uint32(4294967295)
const testGroup = "/system.slice/nucleiron.service"

type busFixture struct {
	mu         sync.Mutex
	service    *dbus.Conn
	address    string
	record     string
	mode       string
	stops      int
	subscribe  chan struct{}
	entered    chan struct{}
	release    chan struct{}
	jobs       []systemd.JobStatus
	active     string
	pid        uint32
	live       bool
	populated  bool
	invocation []byte
	boot       string
	stopper    *systemdStop
}

func (f *busFixture) configure(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode, f.populated = mode, mode == "populated"
}

func (f *busFixture) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func newBusFixture(t *testing.T, record string) *busFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	socket := filepath.Join(t.TempDir(), "bus")
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--session", "--nofork", "--print-address=1", "--address=unix:path="+socket)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	f := &busFixture{record: record, address: socket, active: "active", pid: 1234, live: true, invocation: []byte("0123456789abcdef"), boot: testBinding.Controller,
		subscribe: make(chan struct{}, 16), entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.service, err = dbus.Dial(strings.TrimSpace(address), dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.service.Close() })
	t.Cleanup(func() { close(f.release) })
	if err := f.service.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Hello(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestName(managerName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Export(f, "/org/freedesktop/systemd1", managerName+".Manager"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Export(f, "/org/freedesktop/systemd1/unit/nucleiron_2eservice", "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	f.stopper = &systemdStop{
		connect: func(ctx context.Context) (*stopBus, error) { return connectStop(ctx, f.address) },
		boot:    func() (string, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.boot, nil },
		process: func(pid uint32) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if pid != 0 && f.live {
				return "123456", nil
			}
			if pid != 0 && f.mode == "pid-reused" {
				return "654321", nil
			}
			return "", nil
		},
		empty: func(group string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if group != testGroup || f.populated {
				return errRefused
			}
			return nil
		},
	}
	return f
}

func (f *busFixture) Subscribe() *dbus.Error {
	f.subscribe <- struct{}{}
	return nil
}

func (f *busFixture) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if iface == managerName+".Unit" {
		return map[string]dbus.Variant{"Id": dbus.MakeVariant(controllerUnit), "ActiveState": dbus.MakeVariant(f.active), "InvocationID": dbus.MakeVariant(f.invocation)}, nil
	}
	if iface == managerName+".Service" {
		return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(f.pid), "ControlPID": dbus.MakeVariant(uint32(0)), "ControlGroup": dbus.MakeVariant(testGroup)}, nil
	}
	return nil, dbus.MakeFailedError(errors.New("unexpected interface"))
}

func (f *busFixture) ListJobs() ([]systemd.JobStatus, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]systemd.JobStatus{}, f.jobs...), nil
}

func (f *busFixture) StopUnit(unit, mode string) (dbus.ObjectPath, *dbus.Error) {
	f.mu.Lock()
	f.stops++
	var r stopRecord
	data, err := os.ReadFile(f.record)
	if err != nil || json.Unmarshal(data, &r) != nil || r.Job != 0 || r.Manager == "" || r.Boot != f.boot || r.Group != testGroup || r.Main.Start != "123456" || unit != controllerUnit || mode != "fail" || len(f.subscribe) == 0 {
		f.mu.Unlock()
		return "", dbus.MakeFailedError(errors.New("stop arrived before subscribed durable intent"))
	}
	path := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/systemd1/job/%d", testJob))
	f.jobs = []systemd.JobStatus{{Id: testJob, Unit: controllerUnit, JobType: "stop", Status: "running", JobPath: path, UnitPath: "/org/freedesktop/systemd1/unit/nucleiron_2eservice"}}
	f.entered <- struct{}{}
	behavior := f.mode
	f.mu.Unlock()
	if behavior == "lost-reply" {
		<-f.release
		return path, nil
	}
	if behavior != "hold" {
		result := "done"
		if behavior == "failed" {
			result = "failed"
		}
		f.complete(result)
	}
	return path, nil
}

func (f *busFixture) complete(result string) {
	f.mu.Lock()
	f.jobs, f.active, f.pid, f.live = nil, "inactive", 0, false
	if f.mode == "another-job" {
		f.jobs = []systemd.JobStatus{{Id: 42, Unit: controllerUnit, JobType: "start", Status: "waiting", JobPath: "/org/freedesktop/systemd1/job/42", UnitPath: "/org/freedesktop/systemd1/unit/nucleiron_2eservice"}}
	}
	if f.mode == "live-process" {
		f.live = true
	}
	if f.mode == "active" {
		f.active = "active"
	}
	if f.mode == "failed-unit" {
		f.active = "failed"
	}
	if f.mode == "pid" {
		f.pid = 1234
	}
	if f.mode == "invocation" {
		f.invocation = []byte("different-run-id")
	}
	f.mu.Unlock()
	_ = f.service.Emit("/org/freedesktop/systemd1", managerName+".Manager.JobRemoved", testJob, dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/systemd1/job/%d", testJob)), controllerUnit, result)
}

func loadStop(t *testing.T, path string) stopRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	var r stopRecord
	if err != nil || json.Unmarshal(data, &r) != nil {
		t.Fatalf("record=%s err=%v", data, err)
	}
	return r
}

func TestSystemdStopAndTerminalChecks(t *testing.T) {
	for _, mode := range []string{"fast", "pid-reused", "failed-unit", "failed", "active", "pid", "live-process", "populated", "invocation", "another-job"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.mode = "wrong-controller"
			bus := newBusFixture(t, filepath.Join(f.o.dir, "stop-request.json"))
			bus.configure(mode)
			f.o.stop = bus.stopper
			if code := f.run(context.Background()); code != 1 {
				t.Fatalf("code=%d", code)
			}
			r := loadStop(t, bus.record)
			if r.Job != testJob || r.Stopped != (mode == "fast" || mode == "pid-reused" || mode == "failed-unit") || !jobResult(r.Result) || bus.stopCount() != 1 {
				t.Fatalf("record=%+v stops=%d", r, bus.stopCount())
			}
			before := len(f.requests)
			if code := f.run(context.Background()); code != 1 || len(f.requests) != before || bus.stopCount() != 1 {
				t.Fatal("recovery repeated admission or stop writes")
			}
		})
	}
}

func TestSystemdOriginalJobRecovery(t *testing.T) {
	dir := t.TempDir()
	bus := newBusFixture(t, filepath.Join(dir, "stop-request.json"))
	bus.configure("hold")
	r := &stopRecord{Version: 2, Unit: controllerUnit}
	saved := make(chan struct{}, 1)
	save := func() error {
		err := atomicJSON(dir, "stop-request.json", r)
		if err == nil && r.Job != 0 {
			select {
			case saved <- struct{}{}:
			default:
			}
		}
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bus.stopper.request(ctx, r, save) }()
	select {
	case <-saved:
	case <-time.After(3 * time.Second):
		t.Fatal("job not persisted")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("unfinished job reported stopped")
	}
	original := loadStop(t, bus.record)
	if original.Job != testJob || original.Result != "" {
		t.Fatal(original)
	}
	<-bus.subscribe
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r = &original
	go func() { done <- bus.stopper.recover(ctx, r, save) }()
	select {
	case <-bus.subscribe:
	case <-ctx.Done():
		t.Fatal("recovery did not subscribe")
	}
	bus.complete("done")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if final := loadStop(t, bus.record); !final.Stopped || final.Result != "done" || bus.stopCount() != 1 {
		t.Fatalf("record=%+v stops=%d", final, bus.stopCount())
	}
}

func TestSystemdMissingResultAndChangedBootStayUnknown(t *testing.T) {
	for _, mode := range []string{"missing-result", "changed-boot"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bus := newBusFixture(t, filepath.Join(dir, "stop-request.json"))
			r := &stopRecord{Version: 2, Unit: controllerUnit}
			save := func() error {
				if r.Result != "" && mode == "missing-result" {
					return errors.New("synthetic storage failure")
				}
				return atomicJSON(dir, "stop-request.json", r)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = bus.stopper.request(ctx, r, save)
			previous := loadStop(t, bus.record)
			if mode == "changed-boot" {
				bus.mu.Lock()
				bus.boot = testBinding.Repository
				bus.mu.Unlock()
			}
			ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := bus.stopper.recover(ctx, &previous, func() error { return nil }); err == nil || bus.stopCount() != 1 {
				t.Fatalf("unknown request accepted or repeated: %v stops=%d", err, bus.stopCount())
			}
		})
	}
}

func TestLegacyStopRemainsUntouched(t *testing.T) {
	f := setup(t)
	lock, err := f.o.lock()
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	path := filepath.Join(f.o.dir, "stop-request.json")
	old := []byte(`{"version":1,"status":"requested"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if f.run(context.Background()) != 1 || f.stops != 0 || len(f.requests) != 0 {
		t.Fatal("legacy request retried")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(old) {
		t.Fatal("legacy receipt rewritten")
	}
}

func TestStopCrashHelper(t *testing.T) {
	if os.Getenv("WATCHDOG_STOP_HELPER") != "1" {
		return
	}
	dir := os.Getenv("WATCHDOG_STOP_DIR")
	r := &stopRecord{Version: 2, Unit: controllerUnit}
	save := func() error { return atomicJSON(dir, "stop-request.json", r) }
	s := &systemdStop{
		connect: func(ctx context.Context) (*stopBus, error) { return connectStop(ctx, os.Getenv("WATCHDOG_STOP_BUS")) },
		boot:    func() (string, error) { return testBinding.Controller, nil },
		process: func(pid uint32) (string, error) {
			if pid == 0 {
				return "", nil
			}
			return "123456", nil
		},
		empty: func(string) error { return nil },
	}
	if err := s.request(context.Background(), r, save); err != nil {
		t.Fatal(err)
	}
}

func TestCrashAfterStopDeliveryNeverResends(t *testing.T) {
	f := setup(t)
	lock, err := f.o.lock()
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	bus := newBusFixture(t, filepath.Join(f.o.dir, "stop-request.json"))
	bus.configure("lost-reply")
	f.o.stop = bus.stopper
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStopCrashHelper$")
	cmd.Env = append(os.Environ(), "WATCHDOG_STOP_HELPER=1", "WATCHDOG_STOP_DIR="+f.o.dir, "WATCHDOG_STOP_BUS="+bus.address)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bus.entered:
	case <-ctx.Done():
		cmd.Wait()
		t.Fatal("stop was not delivered")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper survived kill")
	}
	r := loadStop(t, bus.record)
	if r.Job != 0 || r.Manager == "" {
		t.Fatal("unknown request identity was lost")
	}
	if f.run(context.Background()) != 1 || bus.stopCount() != 1 || len(f.requests) != 0 {
		t.Fatal("crashed request was repeated")
	}
}

func TestStopResources(t *testing.T) {
	for _, value := range []string{"populated 1\n", "frozen 0\n", "populated 0\npopulated 0\n", "populated null\n", "populated 0 extra\n"} {
		if unpopulated([]byte(value)) == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := unpopulated([]byte("populated 0\nfrozen 0\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := processStart(uint32(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"/", "/system.slice/other.service", "../nucleiron.service", "/system.slice/../nucleiron.service"} {
		if emptyGroup(group) == nil {
			t.Fatalf("accepted group %q", group)
		}
	}
}

func TestStopStorageFailureNeverGrantsRetry(t *testing.T) {
	for _, stage := range []string{"intent", "job"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			bus := newBusFixture(t, filepath.Join(dir, "stop-request.json"))
			r := &stopRecord{Version: 2, Unit: controllerUnit}
			if err := atomicJSON(dir, "stop-request.json", r); err != nil {
				t.Fatal(err)
			}
			save := func() error {
				if stage == "intent" || r.Job != 0 {
					return errors.New("synthetic storage failure")
				}
				return atomicJSON(dir, "stop-request.json", r)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := bus.stopper.request(ctx, r, save); err == nil {
				t.Fatal("storage failure ignored")
			}
			previous := loadStop(t, bus.record)
			want := 0
			if stage == "job" {
				want = 1
			}
			if previous.Job != 0 || bus.stopCount() != want {
				t.Fatalf("record=%+v calls=%d", previous, bus.stopCount())
			}
			if err := bus.stopper.recover(ctx, &previous, save); err == nil || bus.stopCount() != want {
				t.Fatal("storage failure granted a retry")
			}
		})
	}
}

func TestStopConnectionCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := listener.Accept(); accepted <- c }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := connectStop(ctx, socket); done <- err }()
	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("connection not accepted")
		}
		defer c.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("connection did not arrive")
	}
	// The peer accepts the connection but never answers authentication.
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("incomplete connection accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authentication ignored cancellation")
	}
}

func TestConnectFailureDoesNotRecordAnUnsentStop(t *testing.T) {
	f := setup(t)
	f.mode = "wrong-controller"
	bus := newBusFixture(t, filepath.Join(f.o.dir, "stop-request.json"))
	f.o.stop = bus.stopper
	connect := bus.stopper.connect
	bus.stopper.connect = func(ctx context.Context) (*stopBus, error) {
		return connectStop(ctx, filepath.Join(t.TempDir(), "absent-bus"))
	}
	if f.run(context.Background()) != 1 || bus.stopCount() != 0 {
		t.Fatal("unexpected first stop")
	}
	if _, err := os.Lstat(bus.record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsent stop recorded: %v", err)
	}
	if !strings.Contains(f.log.String(), "stop_not_sent_inspect_state") {
		t.Fatal(f.log.String())
	}
	bus.stopper.connect = connect
	if f.run(context.Background()) != 1 || bus.stopCount() != 1 {
		t.Fatal("later stop did not proceed")
	}
	if r := loadStop(t, bus.record); !r.Stopped || r.Result != "done" {
		t.Fatalf("record=%+v", r)
	}
}
