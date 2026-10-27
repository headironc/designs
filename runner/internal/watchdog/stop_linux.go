package watchdog

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const controllerUnit = "nucleiron.service"

type processIdentity struct {
	PID   uint32 `json:"pid"`
	Start string `json:"start"`
}

type stopRecord struct {
	Version    int             `json:"version"`
	Unit       string          `json:"unit"`
	Boot       string          `json:"boot"`
	Manager    string          `json:"manager"`
	Invocation string          `json:"invocation"`
	Group      string          `json:"group"`
	Main       processIdentity `json:"main"`
	Control    processIdentity `json:"control"`
	Job        uint32          `json:"job"`
	Result     string          `json:"result"`
	Stopped    bool            `json:"stopped"`
}

type stopManager interface {
	request(context.Context, *stopRecord, func() error) error
	recover(context.Context, *stopRecord, func() error) error
}

func jobResult(result string) bool {
	switch result {
	case "done", "canceled", "timeout", "failed", "dependency", "skipped", "invalid", "assert", "unsupported", "collected", "once", "frozen":
		return true
	}
	return false
}

func (r stopRecord) valid() bool {
	if r.Version != 2 || r.Unit != controllerUnit || (r.Result != "" && !jobResult(r.Result)) || (r.Stopped && r.Result != "done") {
		return false
	}
	// No job receipt means no permission to send or infer a replacement job.
	if r.Job == 0 {
		return r.Result == "" && !r.Stopped
	}
	invocation, err := hex.DecodeString(r.Invocation)
	if err != nil || (len(invocation) != 0 && len(invocation) != 16) || !uuid.MatchString(r.Boot) || r.Manager == "" || !validGroup(r.Group) {
		return false
	}
	for _, p := range []processIdentity{r.Main, r.Control} {
		if (p.PID == 0) != (p.Start == "") {
			return false
		}
		if p.PID != 0 {
			if _, err := strconv.ParseUint(p.Start, 10, 64); err != nil || r.Group == "" {
				return false
			}
		}
	}
	return true
}

func (o *observer) savedStop() (*stopRecord, error) {
	data, err := readProtected(filepath.Join(o.dir, "stop-request.json"), o.uid, 0600, 8192)
	if err != nil {
		return nil, err
	}
	var r stopRecord
	if json.Unmarshal(data, &r) != nil || !r.valid() {
		return nil, errRefused
	}
	return &r, nil
}

func (o *observer) saveStop(r *stopRecord) error {
	return atomicJSON(o.dir, "stop-request.json", r)
}

func (o *observer) resumeStop(ctx context.Context, r *stopRecord) int {
	if r.Job == 0 {
		o.event("previous_stop", "controller_stop_unconfirmed")
		return 1
	}
	if err := o.stop.recover(ctx, r, func() error { return o.saveStop(r) }); err != nil {
		o.event("previous_stop", "controller_stop_unconfirmed")
		return 1
	}
	o.event("previous_stop", "controller_stopped_admission_locked")
	return 1
}

func validGroup(group string) bool {
	return group == "" || (filepath.IsAbs(group) && filepath.Clean(group) == group && filepath.Base(group) == controllerUnit)
}

func hostBoot() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	value := strings.TrimSpace(string(data))
	if err != nil || !uuid.MatchString(value) {
		return "", errRefused
	}
	return value, nil
}

func absent(err error) bool { return errors.Is(err, os.ErrNotExist) }
