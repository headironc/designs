package watchdog

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestIdleStatePreservesOriginalPendingRequests(t *testing.T) {
	policy, _ := json.Marshal(testBinding)
	for _, mode := range []string{"absent", "idle", "login_pending", "disable_pending", "foreign_binding", "missing_version", "duplicate", "null_pending", "partial"} {
		t.Run(mode, func(t *testing.T) {
			state := receipt{Version: 1, Binding: testBinding}
			switch mode {
			case "login_pending":
				state.LoginPending = true
			case "disable_pending":
				state.DisablePending = 4294967295
			case "foreign_binding":
				state.Binding.Repository = "33333333-3333-4333-8333-333333333333"
			}
			raw, _ := json.Marshal(state)
			switch mode {
			case "absent":
				raw = nil
			case "missing_version":
				raw = bytes.Replace(raw, []byte(`"version":1,`), nil, 1)
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`"login_pending":false`), []byte(`"login_pending":true,"login_pending":false`), 1)
			case "null_pending":
				raw = bytes.Replace(raw, []byte(`"login_pending":false`), []byte(`"login_pending":null`), 1)
			case "partial":
				raw = raw[:len(raw)-1]
			}
			before := bytes.Clone(raw)
			if err := CheckIdleState(raw, policy, testBinding.Controller); (err == nil) != (mode == "absent" || mode == "idle") {
				t.Fatal("idle state admission", err)
			}
			if !bytes.Equal(before, raw) {
				t.Fatal("modified original receipt")
			}
		})
	}
	if CheckIdleState(nil, policy, "33333333-3333-4333-8333-333333333333") == nil || CheckIdleState(nil, []byte(`{}`), testBinding.Controller) == nil {
		t.Fatal("policy identity was not required")
	}
}
