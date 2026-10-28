package firewall

import (
	"encoding/json"
	"strings"
	"testing"
)

const finalConfiguration = `{"version":1,"bridge":"nucleiron0","subnet":"10.203.64.0/24","gateway":"10.203.64.1","tcp_ports":[53,9443,3128,6514]}`

func TestConfigurationPreservesFinalEgressAndCollectorPolicy(t *testing.T) {
	c, err := ParseConfiguration([]byte(finalConfiguration))
	if err != nil || !c.valid() {
		t.Fatal("the final deployed policy was refused")
	}
	if (Configuration{}).valid() {
		t.Fatal("a configuration bypassed validated construction")
	}
	rendered, err := RenderConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if json.Unmarshal(rendered, &got) != nil || json.Unmarshal([]byte(finalConfiguration), &want) != nil {
		t.Fatal("configuration producer did not emit JSON")
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("producer changed the final policy: %s", rendered)
	}
	if roundTrip, err := ParseConfiguration(rendered); err != nil || roundTrip != c {
		t.Fatal("the real consumer refused its producer's configuration")
	}
}

func TestConfigurationRejectsMissingNullAliasedAndWidenedInputs(t *testing.T) {
	for name, input := range map[string]string{
		"missing":         strings.Replace(finalConfiguration, `"version":1,`, "", 1),
		"null":            strings.Replace(finalConfiguration, `"version":1`, `"version":null`, 1),
		"wrong_type":      strings.Replace(finalConfiguration, `"version":1`, `"version":true`, 1),
		"fraction":        strings.Replace(finalConfiguration, `"version":1`, `"version":1.5`, 1),
		"duplicate":       strings.Replace(finalConfiguration, `"version":1`, `"version":1,"version":1`, 1),
		"case_alias":      strings.Replace(finalConfiguration, `"bridge"`, `"Bridge"`, 1),
		"unknown":         strings.Replace(finalConfiguration, `"version":1`, `"version":1,"policy":"accept"`, 1),
		"trailing":        finalConfiguration + `{}`,
		"interface":       strings.Replace(finalConfiguration, "nucleiron0", "eth0", 1),
		"source_widened":  strings.Replace(finalConfiguration, "10.203.64.0/24", "0.0.0.0/0", 1),
		"host_bits":       strings.Replace(finalConfiguration, "10.203.64.0/24", "10.203.64.1/24", 1),
		"gateway":         strings.Replace(finalConfiguration, "10.203.64.1", "10.203.64.2", 1),
		"extra_port":      strings.Replace(finalConfiguration, "6514]", "6514,443]", 1),
		"missing_egress":  strings.Replace(finalConfiguration, "3128,", "", 1),
		"missing_logs":    strings.Replace(finalConfiguration, ",6514", "", 1),
		"duplicate_port":  strings.Replace(finalConfiguration, "3128", "53", 1),
		"negative_port":   strings.Replace(finalConfiguration, "3128", "-1", 1),
		"port_overflow":   strings.Replace(finalConfiguration, "3128", "65536", 1),
		"null_port_list":  strings.Replace(finalConfiguration, "[53,9443,3128,6514]", "null", 1),
		"oversized_input": finalConfiguration + strings.Repeat(" ", 16*1024),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfiguration([]byte(input)); err == nil {
				t.Fatal("unapproved or malformed deployment input was accepted")
			}
		})
	}
}
