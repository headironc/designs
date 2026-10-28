package lifecycle

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

var diagnosticUnits = []string{"nucleiron.service", "nucleiron-provider-watchdog.service", "nucleiron-provider-watchdog.timer"}

type serviceObservation struct {
	Unit          string   `json:"unit"`
	State         string   `json:"state"`
	Active        string   `json:"active,omitempty"`
	Invocation    string   `json:"invocation_id,omitempty"`
	MainPID       uint32   `json:"main_pid"`
	ControlPID    uint32   `json:"control_pid"`
	Jobs          []uint32 `json:"jobs"`
	Configuration string   `json:"configuration"`
}

func observeServices(ctx context.Context) []serviceObservation {
	uid, _, err := serviceIdentity()
	if err != nil {
		uid = 0
	}
	return servicesAt(ctx, "/run/dbus/system_bus_socket", "/proc", uid)
}

func servicesAt(ctx context.Context, socket, proc string, uid uint32) []serviceObservation {
	unknownAll := func() []serviceObservation {
		out := []serviceObservation{}
		for _, unit := range diagnosticUnits {
			out = append(out, serviceObservation{Unit: unit, State: "unknown", Configuration: "current_controller_database_binding_unknown", Jobs: []uint32{}})
		}
		return out
	}
	var controls []*dbus.Conn
	var stops []func() bool
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	api, err := systemd.NewConnection(func() (*dbus.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { raw.Close() })
		c, err := dbus.NewConn(raw, dbus.WithContext(ctx))
		if err != nil {
			stop()
			raw.Close()
			return nil, err
		}
		if err = c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err == nil {
			err = c.Hello()
		}
		if err != nil {
			stop()
			c.Close()
			return nil, err
		}
		controls = append(controls, c)
		stops = append(stops, stop)
		return c, nil
	})
	if err != nil {
		return unknownAll()
	}
	defer api.Close()
	identity := func() (string, error) {
		var owner, id string
		if controls[0].BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.freedesktop.systemd1").Store(&owner) != nil || controls[0].BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetId", 0).Store(&id) != nil || owner == "" || id == "" {
			return "", errRefused
		}
		return id + "/" + owner, nil
	}
	owner, err := identity()
	if err != nil {
		return unknownAll()
	}
	read := func() []serviceObservation {
		jobs, err := api.ListJobsContext(ctx)
		if err != nil || len(jobs) > 10000 {
			return unknownAll()
		}
		seen := map[uint32]bool{}
		for _, job := range jobs {
			if job.Id == 0 || seen[job.Id] || string(job.JobPath) != "/org/freedesktop/systemd1/job/"+strconv.FormatUint(uint64(job.Id), 10) {
				return unknownAll()
			}
			seen[job.Id] = true
		}
		out := []serviceObservation{}
		for _, unit := range diagnosticUnits {
			v := serviceObservation{Unit: unit, State: "unknown", Jobs: []uint32{}, Configuration: "current_controller_database_binding_unknown"}
			u, err := api.GetUnitPropertiesContext(ctx, unit)
			if err != nil {
				out = append(out, v)
				continue
			}
			id, idOK := u["Id"].(string)
			active, activeOK := u["ActiveState"].(string)
			invocation, invOK := u["InvocationID"].([]byte)
			reload, relOK := u["NeedDaemonReload"].(bool)
			if !idOK || id != unit || !activeOK || !invOK || len(invocation) != 0 && len(invocation) != 16 || !relOK {
				out = append(out, v)
				continue
			}
			switch active {
			case "active", "inactive", "failed", "activating", "deactivating", "reloading":
			default:
				out = append(out, v)
				continue
			}
			v.State, v.Active, v.Invocation = "observed", active, hex.EncodeToString(invocation)
			if strings.HasSuffix(unit, ".service") {
				s, err := api.GetUnitTypePropertiesContext(ctx, unit, "Service")
				if err != nil {
					v.State = "unknown"
				} else {
					main, m := s["MainPID"].(uint32)
					control, c := s["ControlPID"].(uint32)
					if !m || !c {
						v.State = "unknown"
					} else {
						v.MainPID, v.ControlPID = main, control
					}
					if unit == "nucleiron.service" {
						user, u := s["User"].(string)
						group, g := s["Group"].(string)
						if u && g && user == "nucleiron" && (group == "" || group == "nucleiron") && !reload && main > 0 && uid > 0 && controllerProcess(proc, main, uid) == nil {
							v.Configuration = "current_process_config_path_verified"
						}
					}
				}
			}
			for _, job := range jobs {
				if job.Unit == unit {
					v.Jobs = append(v.Jobs, job.Id)
				}
			}
			sort.Slice(v.Jobs, func(i, j int) bool { return v.Jobs[i] < v.Jobs[j] })
			out = append(out, v)
		}
		return out
	}
	first, second := read(), read()
	after, err := identity()
	if err != nil || after != owner {
		return unknownAll()
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.State != b.State || a.Active != b.Active || a.Invocation != b.Invocation || a.MainPID != b.MainPID || a.ControlPID != b.ControlPID || a.Configuration != b.Configuration || !equalJobs(a.Jobs, b.Jobs) {
			first[i].State = "unknown"
			first[i].Configuration = "current_controller_database_binding_unknown"
		}
	}
	return first
}

func equalJobs(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func controllerProcess(proc string, pid uint32, uid uint32) error {
	dir, path := (*os.File)(nil), filepath.Join(proc, strconv.FormatUint(uint64(pid), 10))
	var err error
	dir, err = openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	var st unix.Stat_t
	var fs unix.Statfs_t
	if unix.Fstat(int(dir.Fd()), &st) != nil || st.Uid != uid || unix.Fstatfs(int(dir.Fd()), &fs) != nil || proc == "/proc" && fs.Type != unix.PROC_SUPER_MAGIC {
		return errRefused
	}
	before, err := readAt(dir, "stat", 8192)
	if err != nil {
		return err
	}
	start, err := startTime(before, int64(pid))
	if err != nil {
		return err
	}
	link := make([]byte, 4096)
	n, err := unix.Readlinkat(int(dir.Fd()), "exe", link)
	if err != nil || string(link[:n]) != "/opt/nucleus-halo/bin/garm" {
		return errRefused
	}
	raw, err := readAt(dir, "cmdline", 128*1024)
	if err != nil {
		return err
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(args) != 3 || args[0] != "/opt/nucleus-halo/bin/garm" || args[1] != "-config" || args[2] != controllerConfig {
		return errRefused
	}
	after, err := readAt(dir, "stat", 8192)
	if err != nil {
		return err
	}
	end, err := startTime(after, int64(pid))
	if err != nil || start != end || !samePath(dir, path) {
		return errRefused
	}
	return nil
}
