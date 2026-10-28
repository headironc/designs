package lifecycle

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDiagnosticNetworkRequiredWireEvidence(t *testing.T) {
	for _, field := range []string{"name", "type", "managed", "status"} {
		for _, mode := range []string{"missing", "null", "wrong_type"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				b := inventoryBodies(t)
				var network map[string]json.RawMessage
				json.Unmarshal([]byte(b["/1.0/networks/nucleiron0"]), &network)
				switch mode {
				case "missing":
					delete(network, field)
				case "null":
					network[field] = json.RawMessage("null")
				case "wrong_type":
					network[field] = json.RawMessage("42")
				}
				raw, _ := json.Marshal(network)
				b["/1.0/networks/nucleiron0"] = string(raw)
				socket := machineFixture(t, b)
				if got := diagnoseMachine(context.Background(), socket, originalFixture(t)); got.Instances.State != "unknown" {
					t.Fatalf("missing evidence became %+v", got.Instances)
				}
				if _, err := lxdMachine(context.Background(), socket, binding()); err == nil {
					t.Fatal("capture accepted missing network evidence")
				}
			})
		}
	}
	for _, body := range []string{`{}`, `{"name":"nucleiron0","type":"bridge","managed":false,"status":"Created"}`, `{"name":"nucleiron0","type":"physical","managed":true,"status":"Created"}`} {
		b := inventoryBodies(t)
		b["/1.0/networks/nucleiron0"] = body
		got := diagnoseMachine(context.Background(), machineFixture(t, b), originalFixture(t))
		want := "mismatch"
		if body == "{}" {
			want = "unknown"
		}
		if got.Instances.State != want {
			t.Fatalf("%s: %+v", body, got.Instances)
		}
	}
}
