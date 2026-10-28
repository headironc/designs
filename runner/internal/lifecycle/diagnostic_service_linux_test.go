package lifecycle

import (
	"bufio"
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

type diagnosticBus struct {
	mu    sync.Mutex
	unit  string
	mode  string
	calls int
}

func (b *diagnosticBus) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if iface == "org.freedesktop.systemd1.Unit" {
		b.calls++
		invocation := []byte("0123456789abcdef")
		if b.mode == "changed" && b.calls > 1 {
			invocation = []byte("fedcba9876543210")
		}
		out := map[string]dbus.Variant{"Id": dbus.MakeVariant(b.unit), "ActiveState": dbus.MakeVariant("inactive"), "InvocationID": dbus.MakeVariant(invocation), "NeedDaemonReload": dbus.MakeVariant(false)}
		if b.mode == "missing" {
			delete(out, "ActiveState")
		}
		return out, nil
	}
	if iface == "org.freedesktop.systemd1.Service" {
		return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(0)), "ControlPID": dbus.MakeVariant(uint32(0)), "User": dbus.MakeVariant("nucleiron"), "Group": dbus.MakeVariant("nucleiron")}, nil
	}
	return nil, dbus.MakeFailedError(errors.New("fixture interface refused"))
}

type diagnosticManager struct{ mode string }

func (m *diagnosticManager) ListJobs() ([]systemd.JobStatus, *dbus.Error) {
	if m.mode == "job" {
		return []systemd.JobStatus{{Id: 42, Unit: "nucleiron.service", JobType: "stop", Status: "running", JobPath: "/org/freedesktop/systemd1/job/42", UnitPath: "/org/freedesktop/systemd1/unit/nucleiron_2eservice"}}, nil
	}
	return []systemd.JobStatus{}, nil
}

func diagnosticBusFixture(t *testing.T, mode string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	dir, err := os.MkdirTemp("/tmp", "diag-bus-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "bus")
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--session", "--nofork", "--print-address=1", "--address=unix:path="+socket)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	address, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	c, err := dbus.Dial(strings.TrimSpace(address), dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err = c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		t.Fatal(err)
	}
	if err = c.Hello(); err != nil {
		t.Fatal(err)
	}
	if _, err = c.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	manager := &diagnosticManager{mode: mode}
	if err = c.Export(manager, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range diagnosticUnits {
		path := "/org/freedesktop/systemd1/unit/" + strings.ReplaceAll(strings.ReplaceAll(unit, "-", "_2d"), ".", "_2e")
		if err = c.Export(&diagnosticBus{unit: unit, mode: mode}, dbus.ObjectPath(path), "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
	}
	return socket
}

func TestDiagnosticPrivateSystemdReadOnly(t *testing.T) {
	for _, mode := range []string{"inactive", "job", "missing", "changed"} {
		t.Run(mode, func(t *testing.T) {
			got := servicesAt(context.Background(), diagnosticBusFixture(t, mode), t.TempDir(), uint32(os.Getuid()))
			if len(got) != 3 {
				t.Fatalf("incomplete unit report: %+v", got)
			}
			for _, unit := range got {
				want := "observed"
				if mode == "missing" || mode == "changed" {
					want = "unknown"
				}
				if unit.State != want || unit.Configuration != "current_controller_database_binding_unknown" {
					t.Fatalf("%+v", unit)
				}
				if mode == "job" && unit.Unit == "nucleiron.service" && (len(unit.Jobs) != 1 || unit.Jobs[0] != 42) {
					t.Fatal("current job lost")
				}
			}
		})
	}
}

func TestDiagnosticControllerProcessBinding(t *testing.T) {
	h, _ := processFixture(t)
	path := filepath.Join(h.proc, "4321")
	os.Remove(filepath.Join(path, "exe"))
	os.Symlink("/opt/nucleus-halo/bin/garm", filepath.Join(path, "exe"))
	testWrite(t, filepath.Join(path, "cmdline"), []byte("/opt/nucleus-halo/bin/garm\x00-config\x00"+controllerConfig+"\x00"), 0444)
	if err := controllerProcess(h.proc, 4321, uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	testWrite(t, filepath.Join(path, "cmdline"), []byte("/opt/nucleus-halo/bin/garm\x00-config\x00/other/garm.toml\x00"), 0444)
	if controllerProcess(h.proc, 4321, uint32(os.Getuid())) == nil {
		t.Fatal("wrong config process accepted")
	}
	if controllerProcess(h.proc, 4321, uint32(os.Getuid()+1)) == nil {
		t.Fatal("wrong process owner accepted")
	}
}
