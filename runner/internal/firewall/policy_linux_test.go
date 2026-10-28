package firewall

import (
	"bytes"
	"io"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
	"golang.org/x/sys/unix"
)

func TestPolicyWireUsesOneBatchForOnlyTwoOwnedTables(t *testing.T) {
	configuration, err := ParseConfiguration([]byte(finalConfiguration))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := configuredPolicy(configuration, []byte("fixture-original-operation"))
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	counts := make(map[uint16]int)
	conn, err := nftables.New(nftables.WithTestDial(func(messages []netlink.Message) ([]netlink.Message, error) {
		if len(messages) == 0 {
			return nil, io.EOF
		}
		writes++
		if len(messages) < 2 || messages[0].Header.Type != unix.NFNL_MSG_BATCH_BEGIN || messages[len(messages)-1].Header.Type != unix.NFNL_MSG_BATCH_END {
			t.Fatal("replacement did not use one framed nftables transaction")
		}
		var acknowledgements []netlink.Message
		for _, message := range messages[1 : len(messages)-1] {
			operation := uint16(message.Header.Type) & 0xff
			counts[operation]++
			if len(message.Data) < 4 || message.Data[0] != unix.NFPROTO_INET && message.Data[0] != unix.NFPROTO_BRIDGE {
				t.Fatal("a mutation escaped the two owned families")
			}
			attributes, err := netlink.UnmarshalAttributes(message.Data[4:])
			if err != nil || len(attributes) == 0 || attributes[0].Type != 1 || !bytes.Equal(attributes[0].Data, []byte("nucleiron\x00")) {
				t.Fatal("a mutation escaped the owned table name")
			}
			ack, err := nltest.Error(0, []netlink.Message{message})
			if err != nil {
				t.Fatal(err)
			}
			acknowledgements = append(acknowledgements, ack...)
		}
		return acknowledgements, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	existing := []*nftables.Table{
		{Name: "unrelated", Family: nftables.TableFamilyINet},
		{Name: "nucleiron", Family: nftables.TableFamilyIPv4},
		{Name: "nucleiron", Family: nftables.TableFamilyINet},
		{Name: "nucleiron", Family: nftables.TableFamilyBridge},
	}
	if err := queuePolicy(conn, policy, existing); err != nil || writes != 0 {
		t.Fatal("queuing sent a transaction before its owner could retain intent")
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	// Four hooked chains plus one unreachable provenance chain; seven input
	// decisions, two inet forwards, three bridge drops and one marker rule.
	if writes != 1 || counts[unix.NFT_MSG_DELTABLE] != 2 || counts[unix.NFT_MSG_NEWTABLE] != 2 || counts[unix.NFT_MSG_NEWCHAIN] != 5 || counts[unix.NFT_MSG_NEWRULE] != 13 || len(counts) != 4 {
		t.Fatalf("unexpected mutation scope: sends=%d operations=%v", writes, counts)
	}
}

func TestPolicyRejectsUnvalidatedInputAndAmbiguousInventoryBeforeSend(t *testing.T) {
	if _, err := configuredPolicy(Configuration{}, []byte("fixture")); err == nil {
		t.Fatal("zero configuration constructed an accepted policy")
	}
	c, err := ParseConfiguration([]byte(finalConfiguration))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configuredPolicy(c, nil); err == nil {
		t.Fatal("transaction without original identity accepted")
	}
	policy, err := configuredPolicy(c, []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	conn, err := nftables.New(nftables.WithTestDial(func([]netlink.Message) ([]netlink.Message, error) {
		calls++
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, inventory := range [][]*nftables.Table{
		{nil},
		{{Name: "nucleiron", Family: nftables.TableFamilyINet}, {Name: "nucleiron", Family: nftables.TableFamilyINet}},
	} {
		if err := queuePolicy(conn, policy, inventory); err == nil {
			t.Fatal("ambiguous original inventory accepted")
		}
		if err := conn.Flush(); err != nil || calls != 0 {
			t.Fatal("refused inventory left a partial queued transaction")
		}
	}
}
