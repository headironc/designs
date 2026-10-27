package watchdog

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

const managerName = "org.freedesktop.systemd1"

type stopBus struct {
	api     *systemd.Conn
	control *dbus.Conn
	signals chan *dbus.Signal
	owner   string
	manager string
}

func connectStop(ctx context.Context, socket string) (*stopBus, error) {
	var connections []*dbus.Conn
	api, err := systemd.NewConnection(func() (*dbus.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		c, err := dbus.NewConn(raw, dbus.WithContext(ctx))
		if err != nil {
			raw.Close()
			return nil, err
		}
		if err = c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err == nil {
			err = c.Hello()
		}
		if err != nil {
			c.Close()
			return nil, err
		}
		connections = append(connections, c)
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	b := &stopBus{api: api, control: connections[0], signals: make(chan *dbus.Signal, 64)}
	b.owner, b.manager, err = b.identity(ctx)
	if err == nil {
		// The SDK installs JobRemoved matching during NewConnection. Listen on
		// its signal connection before Subscribe and before any StopUnit call.
		connections[1].Signal(b.signals)
		err = api.Subscribe()
	}
	if err != nil {
		api.Close()
		return nil, err
	}
	return b, nil
}

func (b *stopBus) identity(ctx context.Context) (string, string, error) {
	var owner, busID string
	if err := b.control.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetId", 0).Store(&busID); err != nil {
		return "", "", err
	}
	if err := b.control.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, managerName).Store(&owner); err != nil {
		return "", "", err
	}
	if busID == "" || owner == "" {
		return "", "", errRefused
	}
	return owner, busID + "/" + owner, nil
}

type systemdStop struct {
	connect func(context.Context) (*stopBus, error)
	boot    func() (string, error)
	process func(uint32) (string, error)
	empty   func(string) error
}

func hostStop() *systemdStop {
	return &systemdStop{
		connect: func(ctx context.Context) (*stopBus, error) {
			return connectStop(ctx, "/run/dbus/system_bus_socket")
		},
		boot: hostBoot, process: processStart, empty: emptyGroup,
	}
}

type unitState struct {
	invocation, group, active string
	main, control             uint32
}

func (b *stopBus) unit(ctx context.Context) (unitState, error) {
	u, err := b.api.GetUnitPropertiesContext(ctx, controllerUnit)
	if err != nil {
		return unitState{}, err
	}
	s, err := b.api.GetUnitTypePropertiesContext(ctx, controllerUnit, "Service")
	if err != nil {
		return unitState{}, err
	}
	id, idOK := u["Id"].(string)
	active, activeOK := u["ActiveState"].(string)
	invocation, invocationOK := u["InvocationID"].([]byte)
	group, groupOK := s["ControlGroup"].(string)
	main, mainOK := s["MainPID"].(uint32)
	control, controlOK := s["ControlPID"].(uint32)
	if !idOK || id != controllerUnit || !activeOK || active == "" || !invocationOK || (len(invocation) != 0 && len(invocation) != 16) || !groupOK || !validGroup(group) || !mainOK || !controlOK {
		return unitState{}, errRefused
	}
	if (group == "" && (main != 0 || control != 0)) || ((group == "" || len(invocation) == 0) && active != "inactive" && active != "failed") {
		return unitState{}, errRefused
	}
	return unitState{hex.EncodeToString(invocation), group, active, main, control}, nil
}

func (s *systemdStop) sameManager(ctx context.Context, b *stopBus, r *stopRecord) error {
	boot, err := s.boot()
	if err != nil || boot != r.Boot {
		return errRefused
	}
	_, manager, err := b.identity(ctx)
	if err != nil || manager != r.Manager || manager != b.manager {
		return errRefused
	}
	return nil
}

func (s *systemdStop) request(ctx context.Context, r *stopRecord, save func() error) error {
	b, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer b.api.Close()
	u, err := b.unit(ctx)
	if err != nil {
		return err
	}
	r.Boot, err = s.boot()
	if err != nil {
		return err
	}
	r.Manager, r.Invocation, r.Group = b.manager, u.invocation, u.group
	r.Main.PID, r.Control.PID = u.main, u.control
	for _, p := range []*processIdentity{&r.Main, &r.Control} {
		p.Start, err = s.process(p.PID)
		if err != nil || (p.PID != 0 && p.Start == "") {
			return errRefused
		}
	}
	if err := s.sameManager(ctx, b, r); err != nil {
		return err
	}
	if err := save(); err != nil {
		return err
	}
	if err := s.sameManager(ctx, b, r); err != nil {
		return err
	}
	// Fail rather than replace a conflicting job. A failed call may have been
	// delivered: Job remains zero and recovery never repeats this request.
	job, err := b.api.StopUnitContext(ctx, controllerUnit, "fail", nil)
	if err != nil || job <= 0 || uint64(job) > 4294967295 {
		return errRefused
	}
	r.Job = uint32(job)
	if err := save(); err != nil {
		return err
	}
	return s.finish(ctx, b, r, save)
}

func (s *systemdStop) recover(ctx context.Context, r *stopRecord, save func() error) error {
	if !r.valid() || r.Job == 0 {
		return errRefused
	}
	b, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer b.api.Close()
	return s.finish(ctx, b, r, save)
}

func (s *systemdStop) finish(ctx context.Context, b *stopBus, r *stopRecord, save func() error) error {
	if err := s.sameManager(ctx, b, r); err != nil {
		return err
	}
	if r.Result == "" {
		jobs, err := b.api.ListJobsContext(ctx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job.Id == r.Job && (job.Unit != controllerUnit || job.JobType != "stop" || string(job.JobPath) != fmt.Sprintf("/org/freedesktop/systemd1/job/%d", r.Job)) {
				return errRefused
			}
		}
		// An absent job is not a completion receipt. A signal may already be
		// queued; otherwise wait only within this observation's context.
		for r.Result == "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case signal, ok := <-b.signals:
				if !ok {
					return errRefused
				}
				if signal.Sender != b.owner || signal.Name != managerName+".Manager.JobRemoved" || len(signal.Body) != 4 {
					continue
				}
				var id uint32
				var path dbus.ObjectPath
				var unit, result string
				if dbus.Store(signal.Body, &id, &path, &unit, &result) != nil || id != r.Job {
					continue
				}
				if unit != controllerUnit || string(path) != fmt.Sprintf("/org/freedesktop/systemd1/job/%d", r.Job) || !jobResult(result) {
					return errRefused
				}
				r.Result = result
				if err := save(); err != nil {
					return err
				}
			}
		}
	}
	if r.Result != "done" {
		return errRefused
	}
	if err := s.stopped(ctx, b, r); err != nil {
		return err
	}
	r.Stopped = true
	return save()
}

func (b *stopBus) noJobs(ctx context.Context, r *stopRecord) error {
	jobs, err := b.api.ListJobsContext(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Id == r.Job || job.Unit == controllerUnit {
			return errRefused
		}
	}
	return nil
}

func (s *systemdStop) stopped(ctx context.Context, b *stopBus, r *stopRecord) error {
	if err := b.noJobs(ctx, r); err != nil {
		return err
	}
	u, err := b.unit(ctx)
	if err != nil || (u.active != "inactive" && u.active != "failed") || u.main != 0 || u.control != 0 || (u.invocation != "" && u.invocation != r.Invocation) || (u.group != "" && u.group != r.Group) {
		return errRefused
	}
	for _, p := range []processIdentity{r.Main, r.Control} {
		start, err := s.process(p.PID)
		if err != nil || (p.PID != 0 && start == p.Start) {
			return errRefused
		}
	}
	if err := s.empty(r.Group); err != nil {
		return err
	}
	// Re-read after process/cgroup inspection; a concurrent activation or bus
	// replacement cannot turn this observation into a stopped receipt.
	again, err := b.unit(ctx)
	if err != nil || again != u {
		return errRefused
	}
	if err := b.noJobs(ctx, r); err != nil {
		return err
	}
	return s.sameManager(ctx, b, r)
}
