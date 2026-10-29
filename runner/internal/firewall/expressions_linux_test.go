package firewall

import (
	"bytes"
	"testing"

	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

func expressionList(t *testing.T, expressions ...[]byte) []byte {
	t.Helper()
	var attributes []netlink.Attribute
	for _, expression := range expressions {
		attributes = append(attributes, netlink.Attribute{Type: unix.NLA_F_NESTED | unix.NFTA_LIST_ELEM, Data: expression})
	}
	raw, err := netlink.MarshalAttributes(attributes)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestStrictExpressionsRoundTripCompleteGeneratedPolicy(t *testing.T) {
	for _, table := range snapshotFixture(t) {
		for _, chain := range table.chains {
			for _, rule := range chain.rules {
				var encoded [][]byte
				for _, expression := range rule.Exprs {
					raw, err := expr.Marshal(byte(table.table.Family), expression)
					if err != nil {
						t.Fatal(err)
					}
					encoded = append(encoded, raw)
				}
				decoded, err := strictExpressions(byte(table.table.Family), expressionList(t, encoded...))
				if err != nil || len(decoded) != len(encoded) {
					t.Fatalf("incomplete decode: %v", err)
				}
				for i, expression := range decoded {
					raw, err := expr.Marshal(byte(table.table.Family), expression)
					if err != nil || !bytes.Equal(raw, encoded[i]) {
						t.Fatal("decoded expression changed protocol bytes")
					}
				}
			}
		}
	}
}

func TestStrictExpressionsRefusesSilentlyDroppedProtocolFields(t *testing.T) {
	marshal := func(attributes ...netlink.Attribute) []byte {
		t.Helper()
		raw, err := netlink.MarshalAttributes(attributes)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	counter, err := expr.Marshal(unix.NFPROTO_INET, &expr.Counter{Packets: 12, Bytes: 345})
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := netlink.UnmarshalAttributes(counter)
	if err != nil {
		t.Fatal(err)
	}
	unknownField := append([]byte(nil), attributes[1].Data...)
	unknownField = append(unknownField, marshal(netlink.Attribute{Type: 999, Data: []byte{1, 2, 3, 4}})...)
	for name, encoded := range map[string][]byte{
		"unknown expression":  marshal(netlink.Attribute{Type: unix.NFTA_EXPR_NAME, Data: []byte("unsupported\x00")}, attributes[1]),
		"unknown inner field": marshal(attributes[0], netlink.Attribute{Type: unix.NLA_F_NESTED | unix.NFTA_EXPR_DATA, Data: unknownField}),
		"duplicate field":     marshal(attributes[0], netlink.Attribute{Type: unix.NLA_F_NESTED | unix.NFTA_EXPR_DATA, Data: append(append([]byte(nil), attributes[1].Data...), attributes[1].Data...)}),
		"missing data":        marshal(attributes[0]),
		"truncated verdict":   marshal(netlink.Attribute{Type: unix.NFTA_EXPR_NAME, Data: []byte("immediate\x00")}, netlink.Attribute{Type: unix.NLA_F_NESTED | unix.NFTA_EXPR_DATA, Data: marshal(netlink.Attribute{Type: unix.NLA_F_NESTED | unix.NFTA_IMMEDIATE_DATA, Data: marshal(netlink.Attribute{Type: unix.NLA_F_NESTED | unix.NFTA_DATA_VERDICT, Data: []byte{0, 0, 0, 0}})})}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := strictExpressions(unix.NFPROTO_INET, expressionList(t, counter, encoded)); err == nil {
				t.Fatal("accepted a lossy or malformed expression decode")
			}
		})
	}
	decoded, err := strictExpressions(unix.NFPROTO_INET, expressionList(t, counter))
	if err != nil || len(decoded) != 1 {
		t.Fatalf("valid live counter refused: %v", err)
	}
	c := decoded[0].(*expr.Counter)
	if c.Packets != 12 || c.Bytes != 345 {
		t.Fatal("counter bytes changed during wire validation")
	}
}

func TestStrictExpressionsAcceptsKernelDumpContainersAndAlignment(t *testing.T) {
	// Kernel nft_counter_do_dump uses alignment padding for be64 values;
	// nf_tables_fill_rule_info/nft_expr_dump use nests without nested flags.
	data, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NFTA_COUNTER_PAD},
		{Type: unix.NFTA_COUNTER_BYTES, Data: []byte{0, 0, 0, 0, 0, 0, 1, 0}},
		{Type: unix.NFTA_COUNTER_PAD},
		{Type: unix.NFTA_COUNTER_PACKETS, Data: []byte{0, 0, 0, 0, 0, 0, 0, 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NFTA_EXPR_DATA, Data: data},
		{Type: unix.NFTA_EXPR_NAME, Data: []byte("counter\x00")},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := netlink.MarshalAttributes([]netlink.Attribute{{Type: unix.NFTA_LIST_ELEM, Data: encoded}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := strictExpressions(unix.NFPROTO_INET, raw)
	if err != nil || len(decoded) != 1 {
		t.Fatalf("valid kernel counter rejected: %v", err)
	}
	if c := decoded[0].(*expr.Counter); c.Packets != 2 || c.Bytes != 256 {
		t.Fatal("kernel accounting decoded incorrectly")
	}
}

func TestStrictExpressionsChecksExplicitKernelBitwiseOperation(t *testing.T) {
	base, err := expr.MarshalExprData(unix.NFPROTO_INET, &expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{255, 255, 255, 0}, Xor: make([]byte, 4)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		operation []byte
		duplicate bool
		accepted  bool
	}{
		{"boolean", []byte{0, 0, 0, 0}, false, true},
		{"shift", []byte{0, 0, 0, 1}, false, false},
		{"duplicate", []byte{0, 0, 0, 0}, true, false},
		{"short", []byte{0}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, err := netlink.MarshalAttributes([]netlink.Attribute{{Type: 6, Data: tc.operation}})
			if err != nil {
				t.Fatal(err)
			}
			data := append(append([]byte(nil), base...), op...)
			if tc.duplicate {
				data = append(data, op...)
			}
			encoded, err := netlink.MarshalAttributes([]netlink.Attribute{{Type: unix.NFTA_EXPR_NAME, Data: []byte("bitwise\x00")}, {Type: unix.NFTA_EXPR_DATA, Data: data}})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := strictExpressions(unix.NFPROTO_INET, expressionList(t, encoded))
			if (err == nil) != tc.accepted {
				t.Fatalf("accepted=%t want=%t error=%v", err == nil, tc.accepted, err)
			}
			if tc.accepted && decoded[0].(*expr.Bitwise).Len != 4 {
				t.Fatal("kernel bitwise meaning changed")
			}
		})
	}
}
