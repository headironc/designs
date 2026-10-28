package lifecycle

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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

type pauseTestJob struct {
	ID   uint32
	Path dbus.ObjectPath
}
type pauseTestTimer struct {
	Base         string
	Offset, Next uint64
}
type pauseTestCalendar struct {
	Base, Spec string
	Next       uint64
}
type pauseTestExec struct {
	Path                           string
	Args                           []string
	Ignore                         bool
	Start, StartMono, End, EndMono uint64
	PID                            uint32
	Code, Status                   int32
}
type pauseTestManager struct {
	mu                   sync.Mutex
	conn                 *dbus.Conn
	mode                 string
	stopped, worker      bool
	sends, subscriptions int
	sent                 chan struct{}
}

func (f *pauseTestManager) Subscribe() *dbus.Error {
	f.mu.Lock()
	f.subscriptions++
	f.mu.Unlock()
	return nil
}
func (f *pauseTestManager) ListJobs() ([]systemd.JobStatus, *dbus.Error) {
	return []systemd.JobStatus{}, nil
}
func (f *pauseTestManager) StopUnit(unit, mode string) (dbus.ObjectPath, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if unit != pauseTimer || mode != "fail" || f.subscriptions == 0 {
		return "", dbus.MakeFailedError(errors.New("fixture rejected target/subscription"))
	}
	f.sends++
	f.stopped = true
	result := "done"
	if f.mode == "failed_job" {
		result = "failed"
	}
	path := dbus.ObjectPath(pauseJobPath(42))
	eventUnit := pauseTimer
	if f.mode == "wrong_unit" {
		eventUnit = pauseWorker
	}
	if f.mode != "missing_event" {
		if err := f.conn.Emit(pauseManagerPath, pauseManager+".Manager.JobRemoved", uint32(42), path, eventUnit, result); err != nil {
			return "", dbus.MakeFailedError(err)
		}
	}
	if f.sends == 1 {
		close(f.sent)
	}
	if f.mode == "lost_reply" {
		return "", dbus.MakeFailedError(errors.New("fixture-sensitive-error"))
	}
	return path, nil
}

type pauseTestProperties struct {
	f    *pauseTestManager
	unit string
}
type pauseEventHook struct {
	pauseConnection
	entered chan struct{}
}

type pauseDrainHook struct {
	pauseConnection
	busy chan struct{}
}

func (h pauseDrainHook) drained(ctx context.Context, before, current pauseUnits) error {
	err := h.pauseConnection.drained(ctx, before, current)
	if errors.Is(err, errPauseBusy) {
		select {
		case h.busy <- struct{}{}:
		default:
		}
	}
	return err
}

func (h pauseEventHook) event(ctx context.Context, j pauseJob) (pauseEvent, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	return h.pauseConnection.event(ctx, j)
}
func (p *pauseTestProperties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	f := p.f
	raw := map[string]any{}
	if iface == pauseManager+".Unit" {
		raw = map[string]any{"Id": p.unit, "LoadState": "loaded", "FragmentPath": "/etc/systemd/system/" + p.unit, "DropInPaths": []string{}, "NeedDaemonReload": false, "StopWhenUnneeded": false, "RefuseManualStop": false, "InvocationID": []byte("0123456789abcdef"), "Job": pauseTestJob{0, "/"}, "Triggers": []string{}, "TriggeredBy": []string{}}
		for _, key := range []string{"PartOf", "BindsTo", "BoundBy", "ConsistsOf", "PropagatesStopTo", "StopPropagatedFrom", "OnFailure", "OnSuccess", "UpheldBy", "Upholds"} {
			raw[key] = []string{}
		}
		if p.unit == pauseTimer {
			raw["ActiveState"], raw["SubState"] = "active", "waiting"
			raw["Triggers"] = []string{pauseWorker}
			if f.stopped {
				raw["ActiveState"], raw["SubState"] = "inactive", "dead"
			}
		} else {
			raw["ActiveState"], raw["SubState"] = "inactive", "dead"
			raw["TriggeredBy"] = []string{pauseTimer}
			if f.worker {
				raw["ActiveState"], raw["SubState"] = "activating", "start"
			}
		}
		switch f.mode {
		case "trigger":
			if p.unit == pauseWorker {
				raw["TriggeredBy"] = []string{pauseTimer, "other.timer"}
			}
		case "hook":
			raw["PropagatesStopTo"] = []string{"nucleiron.service"}
		case "reload":
			raw["NeedDaemonReload"] = true
		case "missing":
			delete(raw, "StopWhenUnneeded")
		case "bad_job_type":
			raw["Job"] = struct {
				ID   uint64
				Path dbus.ObjectPath
			}{0, "/"}
		case "replacement":
			if f.stopped && p.unit == pauseWorker {
				raw["InvocationID"] = []byte("fedcba9876543210")
			}
		}
	} else if iface == pauseManager+".Timer" {
		raw = map[string]any{"Unit": pauseWorker, "Persistent": false, "WakeSystem": false, "OnClockChange": false, "OnTimezoneChange": false, "AccuracyUSec": uint64(1000000), "RandomizedDelayUSec": uint64(0), "TimersCalendar": []pauseTestCalendar{}, "TimersMonotonic": []pauseTestTimer{{"OnBootUSec", 10000000, 33}, {"OnUnitActiveUSec", 5000000, 44}}}
	} else if iface == pauseManager+".Service" {
		command := pauseTestExec{Path: "/opt/nucleus-halo/bin/provider-watchdog", Args: []string{"/opt/nucleus-halo/bin/provider-watchdog"}}
		if f.mode == "exec" {
			command.Args = append(command.Args, "--other")
		}
		raw = map[string]any{"Type": "oneshot", "Restart": "no", "RemainAfterExit": false, "User": "", "Group": "", "Slice": "system.slice", "MainPID": uint32(0), "ControlPID": uint32(0), "ExecMainStartTimestampMonotonic": uint64(111), "ControlGroup": "", "Result": "success", "ExecStart": []pauseTestExec{command}}
		for _, key := range []string{"ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecCondition"} {
			raw[key] = []pauseTestExec{}
		}
		if f.worker {
			raw["MainPID"], raw["ControlGroup"] = uint32(4321), "/system.slice/"+pauseWorker
		}
	} else {
		return nil, dbus.MakeFailedError(errors.New("unknown fixture interface"))
	}
	out := map[string]dbus.Variant{}
	for key, value := range raw {
		out[key] = dbus.MakeVariant(value)
	}
	return out, nil
}

func privatePauseBus(t *testing.T) (*pauseTestManager, string, pauseHost) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	dir, err := os.MkdirTemp("/tmp", "pause-bus-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "bus")
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--session", "--nofork", "--print-address=1", "--address=unix:path="+socket)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dbus.Dial(strings.TrimSpace(address), dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err = conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		t.Fatal(err)
	}
	if err = conn.Hello(); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.RequestName(pauseManager, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	f := &pauseTestManager{conn: conn, sent: make(chan struct{})}
	if err = conn.Export(f, pauseManagerPath, pauseManager+".Manager"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{pauseTimer, pauseWorker} {
		path := "/org/freedesktop/systemd1/unit/" + strings.ReplaceAll(strings.ReplaceAll(unit, "-", "_2d"), ".", "_2e")
		if err = conn.Export(&pauseTestProperties{f, unit}, dbus.ObjectPath(path), "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	host := pauseHost{proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup")}
	testWrite(t, filepath.Join(host.proc, "sys/kernel/random/boot_id"), []byte(bootUUID+"\n"), 0444)
	if err = os.Mkdir(host.cgroup, 0700); err != nil {
		t.Fatal(err)
	}
	return f, socket, host
}

func TestPauseActualDBusEarlyEventAndUnknownRecovery(t *testing.T) {
	for _, mode := range []string{"ok", "failed_job", "lost_reply", "missing_event", "wrong_unit", "trigger", "hook", "reload", "exec", "missing", "bad_job_type", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			p, _, r, sha := pauseFixture(t)
			f, socket, host := privatePauseBus(t)
			f.mu.Lock()
			f.mode = mode
			f.mu.Unlock()
			entered := make(chan struct{}, 1)
			p.boot = host.boot
			p.connect = func(c context.Context) (pauseConnection, error) {
				b, e := connectPause(c, socket, host)
				return pauseEventHook{b, entered}, e
			}
			run := func() (pauseOutcome, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if mode != "missing_event" {
					return p.execute(ctx, r, sha)
				}
				type result struct {
					o pauseOutcome
					e error
				}
				done := make(chan result, 1)
				go func() { o, e := p.execute(ctx, r, sha); done <- result{o, e} }()
				select {
				case <-entered:
					cancel()
				case <-ctx.Done():
					t.Fatal("event wait stage not reached")
				}
				got := <-done
				return got.o, got.e
			}
			out, err := run()
			if mode == "ok" {
				if err != nil || out.State != "watchdog_paused_worker_drained" || out.TimerJob != 42 {
					t.Fatalf("real SDK: %+v %v", out, err)
				}
			} else if err == nil {
				t.Fatal("bad observation accepted")
			}
			f.mu.Lock()
			sent := f.sends
			f.mu.Unlock()
			if mode == "trigger" || mode == "hook" || mode == "reload" || mode == "exec" || mode == "missing" || mode == "bad_job_type" {
				if sent != 0 {
					t.Fatal("bad config sent request")
				}
				return
			}
			run()
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != sent || sent != 1 {
				t.Fatal("DBus original resent")
			}
		})
	}
}

func TestPauseActualWorkerNaturalDrainAndDescendants(t *testing.T) {
	p, _, r, sha := pauseFixture(t)
	f, socket, h := privatePauseBus(t)
	f.mu.Lock()
	f.worker = true
	f.mu.Unlock()
	fields := []string{"S", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "777"}
	testWrite(t, filepath.Join(h.proc, "4321/stat"), []byte("4321 (watchdog) "+strings.Join(fields, " ")), 0444)
	testWrite(t, filepath.Join(h.proc, "4321/cgroup"), []byte("0::/system.slice/"+pauseWorker+"\n"), 0444)
	group := filepath.Join(h.cgroup, "system.slice", pauseWorker)
	testWrite(t, filepath.Join(group, "cgroup.events"), []byte("populated 1\n"), 0444)
	p.boot = h.boot
	busy := make(chan struct{}, 1)
	p.connect = func(c context.Context) (pauseConnection, error) {
		b, err := connectPause(c, socket, h)
		return pauseDrainHook{b, busy}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.execute(ctx, r, sha); done <- err }()
	select {
	case <-f.sent:
	case <-ctx.Done():
		t.Fatal("StopUnit not reached")
	}
	f.mu.Lock()
	f.worker = false
	f.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(h.proc, "4321")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("retained descendant completed early: %v", err)
	case <-busy:
	case <-ctx.Done():
		t.Fatal("retained descendant not observed")
	}
	if err := os.Chmod(filepath.Join(group, "cgroup.events"), 0600); err != nil {
		t.Fatal(err)
	}
	events, err := os.OpenFile(filepath.Join(group, "cgroup.events"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = events.WriteAt([]byte("0"), 10)
	events.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 {
		t.Fatal("natural drain resent")
	}
}

func TestPauseHostRejectsPIDAndCgroupReuse(t *testing.T) {
	_, _, h := privatePauseBus(t)
	before := pauseUnits{Main: pausePID{4321, 777}, Group: "/system.slice/" + pauseWorker}
	fields := []string{"S", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "778"}
	testWrite(t, filepath.Join(h.proc, "4321/stat"), []byte("4321 (replacement) "+strings.Join(fields, " ")), 0444)
	testWrite(t, filepath.Join(h.proc, "4321/cgroup"), []byte("0::/system.slice/"+pauseWorker+"\n"), 0444)
	if h.drained(context.Background(), before, pauseUnits{}) == nil {
		t.Fatal("PID reuse was completion")
	}
	os.RemoveAll(filepath.Join(h.proc, "4321"))
	group := filepath.Join(h.cgroup, "system.slice", pauseWorker)
	testWrite(t, filepath.Join(group, "cgroup.events"), []byte("populated 0\n"), 0444)
	if h.drained(context.Background(), before, pauseUnits{}) == nil {
		t.Fatal("new cgroup accepted")
	}
	t.Log(fmt.Sprintf("original PID and cgroup replacement rejected"))
}
