package lifecycle

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
type pauseTestExitStatus struct {
	Codes, Signals []int32
}
type pauseTestManager struct {
	mu                   sync.Mutex
	conn                 *dbus.Conn
	mode                 string
	stopped, worker      bool
	sends, subscriptions int
	rawReads, switchAt   int
	sent                 chan struct{}
}

func (f *pauseTestManager) Subscribe() *dbus.Error {
	f.mu.Lock()
	f.subscriptions++
	f.mu.Unlock()
	return nil
}
func (f *pauseTestManager) ListJobs() ([]systemd.JobStatus, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var id uint32
	unit := pauseWorker
	switch f.mode {
	case "worker_new_list", "worker_consistent":
		id = 77
	case "worker_replaced_list":
		id = 78
	case "timer_new_list", "timer_consistent":
		unit, id = pauseTimer, 88
	case "timer_replaced_list":
		unit, id = pauseTimer, 89
	case "after_raw_foreign_job":
		if f.rawReads >= f.switchAt {
			unit, id = pauseTimer, 99
		}
	}
	if id != 0 {
		kind := "start"
		if unit == pauseTimer {
			kind = "stop"
		}
		return []systemd.JobStatus{{Id: id, Unit: unit, JobType: kind, Status: "waiting", JobPath: dbus.ObjectPath(pauseJobPath(id)), UnitPath: dbus.ObjectPath("/org/freedesktop/systemd1/unit/fixture")}}, nil
	}
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
		if p.unit == pauseTimer {
			f.rawReads++
		}
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
		case "changed_job":
			if p.unit == pauseWorker {
				raw["Job"] = pauseTestJob{77, dbus.ObjectPath(pauseJobPath(77))}
			}
		case "worker_disappeared", "worker_consistent", "worker_replaced_list":
			if p.unit == pauseWorker {
				raw["Job"] = pauseTestJob{77, dbus.ObjectPath(pauseJobPath(77))}
			}
		case "timer_disappeared", "timer_consistent", "timer_replaced_list":
			if p.unit == pauseTimer {
				raw["Job"] = pauseTestJob{88, dbus.ObjectPath(pauseJobPath(88))}
			}
		}
		if p.unit == pauseTimer && f.rawReads >= f.switchAt {
			switch f.mode {
			case "after_raw_active":
				raw["ActiveState"] = "active"
			case "after_raw_activating":
				raw["ActiveState"] = "activating"
			case "after_raw_foreign_job", "after_raw_foreign_job_busy":
				raw["Job"] = pauseTestJob{99, dbus.ObjectPath(pauseJobPath(99))}
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
		raw["SuccessExitStatus"] = pauseTestExitStatus{[]int32{}, []int32{}}
		switch f.mode {
		case "success_exit_code":
			raw["SuccessExitStatus"] = pauseTestExitStatus{[]int32{7}, []int32{}}
		case "success_signal":
			raw["SuccessExitStatus"] = pauseTestExitStatus{[]int32{}, []int32{15}}
		case "success_missing":
			delete(raw, "SuccessExitStatus")
		case "success_wrong_type":
			raw["SuccessExitStatus"] = []int32{}
		case "success_wrong_element":
			raw["SuccessExitStatus"] = struct{ Codes, Signals []uint32 }{[]uint32{}, []uint32{}}
		}
		for _, key := range []string{"ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecCondition"} {
			raw[key] = []pauseTestExec{}
		}
		if f.worker {
			raw["MainPID"], raw["ControlGroup"] = uint32(4321), "/system.slice/"+pauseWorker
		}
		switch f.mode {
		case "changed_start":
			raw["ExecMainStartTimestampMonotonic"] = uint64(112)
		case "changed_config":
			raw["User"] = "root"
		case "changed_pid":
			raw["MainPID"] = uint32(4322)
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
	for _, mode := range []string{"ok", "failed_job", "lost_reply", "missing_event", "wrong_unit", "trigger", "hook", "reload", "exec", "missing", "bad_job_type", "replacement", "success_exit_code", "success_signal", "success_missing", "success_wrong_type", "success_wrong_element"} {
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
			if mode == "trigger" || mode == "hook" || mode == "reload" || mode == "exec" || mode == "missing" || mode == "bad_job_type" || strings.HasPrefix(mode, "success_") {
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
	dev, ino, empty, err := h.group()
	if err != nil || !empty || dev == 0 || ino == 0 {
		t.Fatalf("original group: %d/%d %v", dev, ino, err)
	}
	before.GroupDevice, before.GroupInode = dev, ino
	current := before
	current.Main = pausePID{}
	if err := os.Rename(group, group+"-original"); err != nil {
		t.Fatal(err)
	}
	testWrite(t, filepath.Join(group, "cgroup.events"), []byte("populated 0\n"), 0444)
	newDev, newIno, _, err := h.group()
	if err != nil || newDev == dev && newIno == ino {
		t.Fatal("fixture failed to replace cgroup identity", err)
	}
	if err := h.drained(context.Background(), before, current); !errors.Is(err, errRefused) {
		t.Fatalf("cgroup replacement accepted: %v", err)
	}
	t.Logf("cgroup replacement refused: original=%d/%d replacement=%d/%d", dev, ino, newDev, newIno)
}

func pauseStat(state, start string) []byte {
	return []byte("4321 (watchdog) " + state + " 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 " + start + "\n")
}

func TestPauseOriginalReapingProcessNaturalDrain(t *testing.T) {
	for _, mode := range []string{"Z", "X", "x", "Z_deleted", "X_deleted", "x_deleted"} {
		t.Run(mode, func(t *testing.T) {
			state := strings.TrimSuffix(mode, "_deleted")
			p, _, r, sha := pauseFixture(t)
			f, socket, h := privatePauseBus(t)
			f.mu.Lock()
			f.worker = true
			f.mu.Unlock()
			testWrite(t, filepath.Join(h.proc, "4321/stat"), pauseStat("S", "777"), 0444)
			testWrite(t, filepath.Join(h.proc, "4321/cgroup"), []byte("0::/system.slice/"+pauseWorker+"\n"), 0444)
			testWrite(t, filepath.Join(h.cgroup, "system.slice", pauseWorker, "cgroup.events"), []byte("populated 0\n"), 0444)
			busy := make(chan struct{}, 1)
			p.boot = h.boot
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
			// Rename a complete stat fixture so concurrent reads see either valid
			// state of this same PID/starttime, never a truncated write.
			stat := filepath.Join(h.proc, "4321/stat")
			testWrite(t, stat+".next", pauseStat(state, "777"), 0444)
			if err := os.Rename(stat+".next", stat); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(mode, "_deleted") {
				path := filepath.Join(h.proc, "4321/cgroup")
				testWrite(t, path+".next", []byte("0::/system.slice/"+pauseWorker+" (deleted)\n"), 0444)
				if err := os.Rename(path+".next", path); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(h.cgroup, "system.slice", pauseWorker)); err != nil {
					t.Fatal(err)
				}
			}
			f.mu.Lock()
			f.worker = false
			f.mu.Unlock()
			select {
			case err := <-done:
				t.Fatalf("original %s not retained as busy: %v", state, err)
			case <-busy:
			case <-ctx.Done():
				t.Fatal("original reaping process not observed")
			}
			if err := os.RemoveAll(filepath.Join(h.proc, "4321")); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 1 {
				t.Fatal("reaping observation resent original")
			}
		})
	}
}

func TestPauseReapingProcessKeepsExactIdentity(t *testing.T) {
	for _, mode := range []string{"original_Z", "original_X", "original_x", "original_Z_deleted", "original_X_deleted", "original_x_deleted", "replacement_Z", "replacement_X", "replacement_x", "replacement_Z_missing_group", "replacement_Z_deleted", "live_deleted", "wrong_deleted", "malformed_start", "short_stat", "wrong_pid", "wrong_state", "wrong_group"} {
		t.Run(mode, func(t *testing.T) {
			_, _, h := privatePauseBus(t)
			before := pauseUnits{Main: pausePID{4321, 777}, Group: "/system.slice/" + pauseWorker}
			raw, group := pauseStat("Z", "777"), "0::/system.slice/"+pauseWorker+"\n"
			switch {
			case strings.HasPrefix(mode, "original_"):
				raw = pauseStat(strings.TrimSuffix(strings.TrimPrefix(mode, "original_"), "_deleted"), "777")
			case strings.HasPrefix(mode, "replacement_"):
				raw = pauseStat(strings.TrimSuffix(strings.TrimPrefix(mode, "replacement_"), "_deleted"), "778")
				if mode == "replacement_Z_missing_group" {
					raw = pauseStat("Z", "778")
				}
			case mode == "malformed_start":
				raw = pauseStat("Z", "invalid")
			case mode == "short_stat":
				raw = []byte("4321 (watchdog) Z 1 2\n")
			case mode == "wrong_pid":
				raw = bytes.Replace(raw, []byte("4321"), []byte("4322"), 1)
			case mode == "wrong_state":
				raw = pauseStat("ZZ", "777")
			case mode == "wrong_group":
				group = "0::/system.slice/other.service\n"
			case mode == "live_deleted":
				raw = pauseStat("S", "777")
			}
			if strings.HasSuffix(mode, "_deleted") {
				group = "0::/system.slice/" + pauseWorker + " (deleted)\n"
			}
			if mode == "wrong_deleted" {
				group = "0::/system.slice/other.service (deleted)\n"
			}
			testWrite(t, filepath.Join(h.proc, "4321/stat"), raw, 0444)
			if mode != "replacement_Z_missing_group" {
				testWrite(t, filepath.Join(h.proc, "4321/cgroup"), []byte(group), 0444)
			}
			err := h.drained(context.Background(), before, pauseUnits{})
			if strings.HasPrefix(mode, "original_") {
				if !errors.Is(err, errPauseBusy) {
					t.Fatalf("original reaping identity: %v", err)
				}
			} else if !errors.Is(err, errRefused) {
				t.Fatalf("replacement/malformed identity: %v", err)
			}
		})
	}
}

func TestPauseSnapshotPrioritizesOriginalIdentityOverReaping(t *testing.T) {
	for _, mode := range []string{"original", "unbound", "replacement", "changed_start", "changed_config", "changed_pid", "changed_job", "reused_start", "live_deleted"} {
		t.Run(mode, func(t *testing.T) {
			f, socket, h := privatePauseBus(t)
			f.mu.Lock()
			f.worker = true
			f.mu.Unlock()
			stat, membership := filepath.Join(h.proc, "4321/stat"), filepath.Join(h.proc, "4321/cgroup")
			testWrite(t, stat, pauseStat("S", "777"), 0444)
			testWrite(t, membership, []byte("0::/system.slice/"+pauseWorker+"\n"), 0444)
			group := filepath.Join(h.cgroup, "system.slice", pauseWorker)
			testWrite(t, filepath.Join(group, "cgroup.events"), []byte("populated 1\n"), 0444)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			b, err := connectPause(ctx, socket, h)
			if err != nil {
				t.Fatal(err)
			}
			defer b.close()
			original, err := b.snapshot(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, stat, pauseStat("Z", "777"), 0444)
			testWrite(t, membership, []byte("0::/system.slice/"+pauseWorker+" (deleted)\n"), 0444)
			if err := os.RemoveAll(group); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.mode, f.stopped = mode, true
			f.mu.Unlock()
			if mode == "reused_start" {
				testWrite(t, stat, pauseStat("Z", "778"), 0444)
			}
			if mode == "live_deleted" {
				testWrite(t, stat, pauseStat("S", "777"), 0444)
			}
			expected := &original
			if mode == "unbound" {
				expected = nil
			}
			_, err = b.snapshot(ctx, expected)
			want := errRefused
			if mode == "original" {
				want = errPauseBusy
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: got %v want %v", mode, err, want)
			}
		})
	}
}

func TestPauseJobListDistinguishesExitFromReplacement(t *testing.T) {
	for _, mode := range []string{"worker_new_list", "timer_new_list", "worker_replaced_list", "timer_replaced_list", "worker_disappeared", "timer_disappeared", "worker_consistent", "timer_consistent"} {
		t.Run(mode, func(t *testing.T) {
			f, socket, h := privatePauseBus(t)
			f.mu.Lock()
			f.mode = mode
			f.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := connectPause(ctx, socket, h)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.close()
			_, err = conn.(*pauseBus).rawSnapshot(ctx)
			switch {
			case strings.HasSuffix(mode, "_consistent"):
				if err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(mode, "_disappeared"):
				if !errors.Is(err, errPauseBusy) {
					t.Fatalf("original job disappearance: %v", err)
				}
			default:
				if !errors.Is(err, errRefused) {
					t.Fatalf("observed replacement not refused: %v", err)
				}
			}
		})
	}
}

func TestPauseSnapshotRetainsSecondRawTimerFacts(t *testing.T) {
	for _, mode := range []string{"after_raw_active", "after_raw_activating", "after_raw_foreign_job", "after_raw_foreign_job_busy"} {
		t.Run(mode, func(t *testing.T) {
			f, socket, h := privatePauseBus(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			b, err := connectPause(ctx, socket, h)
			if err != nil {
				t.Fatal(err)
			}
			defer b.close()
			original, err := b.snapshot(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			reads := f.rawReads
			f.stopped, f.mode, f.switchAt = true, mode, 4
			f.mu.Unlock()
			if reads != 2 {
				t.Fatalf("original raw observation count=%d", reads)
			}
			got, err := b.snapshot(ctx, &original)
			if !errors.Is(err, errPauseBusy) || !pauseTimerConflict(got, 42) {
				t.Fatalf("second raw conflict discarded: %+v %v", got, err)
			}
			if strings.Contains(mode, "foreign_job") {
				if got.TimerJob != 99 {
					t.Fatalf("missing observed foreign job: %+v", got)
				}
			} else if got.TimerActive != strings.TrimPrefix(mode, "after_raw_") {
				t.Fatalf("missing observed activation: %+v", got)
			}
		})
	}
}
