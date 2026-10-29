package firewall

import (
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
	"golang.org/x/sys/unix"
)

func kernelRuleFixture(t *testing.T) (netlink.Message, *nftables.Table, *nftables.Chain) {
	t.Helper()
	table := &nftables.Table{Name: "nucleiron", Family: nftables.TableFamilyINet}
	chain := &nftables.Chain{Table: table, Name: "managed"}
	marshal := func(attributes ...netlink.Attribute) []byte {
		t.Helper()
		raw, err := netlink.MarshalAttributes(attributes)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	counter := marshal(netlink.Attribute{Type: unix.NFTA_EXPR_NAME, Data: []byte("counter\x00")}, netlink.Attribute{Type: unix.NFTA_EXPR_DATA, Data: marshal(
		netlink.Attribute{Type: unix.NFTA_COUNTER_BYTES, Data: make([]byte, 8)}, netlink.Attribute{Type: unix.NFTA_COUNTER_PACKETS, Data: make([]byte, 8)},
	)})
	attributes := marshal(
		netlink.Attribute{Type: unix.NFTA_RULE_TABLE, Data: []byte("nucleiron\x00")},
		netlink.Attribute{Type: unix.NFTA_RULE_CHAIN, Data: []byte("managed\x00")},
		netlink.Attribute{Type: unix.NFTA_RULE_PAD},
		netlink.Attribute{Type: unix.NFTA_RULE_HANDLE, Data: []byte{0, 0, 0, 0, 0, 0, 0, 8}},
		netlink.Attribute{Type: unix.NFTA_RULE_EXPRESSIONS, Data: marshal(netlink.Attribute{Type: unix.NFTA_LIST_ELEM, Data: counter})},
		netlink.Attribute{Type: unix.NFTA_RULE_USERDATA, Data: []byte("original-operation")},
	)
	return netlink.Message{Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_NEWRULE)}, Data: append([]byte{unix.NFPROTO_INET, 0, 0x12, 0x34}, attributes...)}, table, chain
}

func TestStrictRulePreservesProvenanceAndRejectsUnexplainedAttributes(t *testing.T) {
	message, table, chain := kernelRuleFixture(t)
	rule, err := strictRule(message, table, chain)
	if err != nil || rule.Handle != 8 || len(rule.Exprs) != 1 || string(rule.UserData) != "original-operation" {
		t.Fatalf("kernel rule rejected or changed: %v", err)
	}
	for _, attribute := range []netlink.Attribute{
		{Type: 999, Data: []byte{1}},
		{Type: unix.NFTA_RULE_TABLE, Data: []byte("other\x00")},
		{Type: unix.NFTA_RULE_PAD, Data: []byte{1}},
	} {
		extra, err := netlink.MarshalAttributes([]netlink.Attribute{attribute})
		if err != nil {
			t.Fatal(err)
		}
		bad := message
		bad.Data = append(append([]byte(nil), message.Data...), extra...)
		if _, err := strictRule(bad, table, chain); err == nil {
			t.Fatal("accepted unexplained rule bytes")
		}
	}
	message.Data[0] = unix.NFPROTO_BRIDGE
	if _, err := strictRule(message, table, chain); err == nil {
		t.Fatal("accepted wrong response family")
	}
}

func TestKernelReadRulesUsesOnlyReadRequestAndDoesNotRedial(t *testing.T) {
	message, table, chain := kernelRuleFixture(t)
	calls := 0
	socket := nltest.Dial(func(request []netlink.Message) ([]netlink.Message, error) {
		calls++
		if len(request) != 1 || request[0].Header.Type != netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_GETRULE) || request[0].Header.Flags != netlink.Request|netlink.Dump {
			t.Fatal("readback sent a mutation or changed request")
		}
		message.Header.Sequence = request[0].Header.Sequence
		message.Header.PID = request[0].Header.PID
		return []netlink.Message{message}, nil
	})
	defer socket.Close()
	reader := &kernelReader{socket: socket}
	rules, err := reader.ReadRules(table, chain)
	if err != nil || len(rules) != 1 || calls != 1 {
		t.Fatalf("unexpected readback: calls=%d err=%v", calls, err)
	}
}
