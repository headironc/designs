package firewall

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
)

// Configuration represents only the deployed Nucleiron policy. It does not
// expose an expression language or let a caller widen access through fields.
type Configuration struct {
	bridge  string
	subnet  netip.Prefix
	gateway netip.Addr
	ports   [4]uint16
}

type configurationInput struct {
	Version  int      `json:"version"`
	Bridge   string   `json:"bridge"`
	Subnet   string   `json:"subnet"`
	Gateway  string   `json:"gateway"`
	TCPPorts []uint16 `json:"tcp_ports"`
}

func deployedConfiguration() Configuration {
	return Configuration{
		bridge:  "nucleiron0",
		subnet:  netip.MustParsePrefix("10.203.64.0/24"),
		gateway: netip.MustParseAddr("10.203.64.1"),
		ports:   [4]uint16{53, 9443, 3128, 6514},
	}
}

func (c Configuration) valid() bool { return c == deployedConfiguration() }

// RenderConfiguration produces the deployment input consumed by this version.
// It performs no filesystem, service or netlink operations.
func RenderConfiguration() ([]byte, error) {
	c := deployedConfiguration()
	raw, err := json.MarshalIndent(configurationInput{1, c.bridge, c.subnet.String(), c.gateway.String(), c.ports[:]}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ParseConfiguration requires every field and the precise current policy.
// Checking member names before decoding also rejects encoding/json aliases.
func ParseConfiguration(raw []byte) (Configuration, error) {
	refused := errors.New("invalid dedicated firewall configuration")
	if len(raw) == 0 || len(raw) > 16*1024 {
		return Configuration{}, refused
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return Configuration{}, refused
	}
	seen := make(map[string]bool)
	for d.More() {
		member, err := d.Token()
		key, ok := member.(string)
		if err != nil || !ok || seen[key] {
			return Configuration{}, refused
		}
		switch key {
		case "version", "bridge", "subnet", "gateway", "tcp_ports":
		default:
			return Configuration{}, refused
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Configuration{}, refused
		}
		seen[key] = true
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 5 || d.Decode(new(any)) != io.EOF {
		return Configuration{}, refused
	}
	var in configurationInput
	if json.Unmarshal(raw, &in) != nil || in.Version != 1 || len(in.TCPPorts) != 4 {
		return Configuration{}, refused
	}
	subnet, err := netip.ParsePrefix(in.Subnet)
	if err != nil || subnet.String() != in.Subnet {
		return Configuration{}, refused
	}
	gateway, err := netip.ParseAddr(in.Gateway)
	if err != nil || gateway.String() != in.Gateway {
		return Configuration{}, refused
	}
	c := Configuration{bridge: in.Bridge, subnet: subnet, gateway: gateway}
	copy(c.ports[:], in.TCPPorts)
	if !c.valid() {
		return Configuration{}, refused
	}
	return c, nil
}
