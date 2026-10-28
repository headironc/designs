package firewall

import (
	"bytes"
	"testing"

	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
	"golang.org/x/sys/unix"
)

func TestBeforeSnapshotRetainsLegacyLookupAndAnonymousSetWithoutDecoding(t *testing.T) {
	table := wireFixture(t, unix.NFT_MSG_NEWTABLE, unix.NFPROTO_INET,
		netlink.Attribute{Type: 1, Data: []byte("nucleiron\x00")}, netlink.Attribute{Type: 2, Data: be32(0)},
	)
	other := wireFixture(t, unix.NFT_MSG_NEWTABLE, unix.NFPROTO_INET, netlink.Attribute{Type: 1, Data: []byte("unrelated\x00")})
	marshal := func(fields ...netlink.Attribute) []byte {
		t.Helper()
		raw, err := netlink.MarshalAttributes(fields)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	lookup := marshal(netlink.Attribute{Type: unix.NFTA_EXPR_NAME, Data: []byte("lookup\x00")}, netlink.Attribute{Type: unix.NFTA_EXPR_DATA, Data: marshal(netlink.Attribute{Type: 1, Data: []byte("__set1\x00")}, netlink.Attribute{Type: 2, Data: be32(1)})})
	rule := wireFixture(t, unix.NFT_MSG_NEWRULE, unix.NFPROTO_INET,
		netlink.Attribute{Type: 1, Data: []byte("nucleiron\x00")}, netlink.Attribute{Type: 2, Data: []byte("guest_input\x00")},
		netlink.Attribute{Type: 4, Data: marshal(netlink.Attribute{Type: unix.NFTA_LIST_ELEM, Data: lookup})},
	)
	set := wireFixture(t, unix.NFT_MSG_NEWSET, unix.NFPROTO_INET,
		netlink.Attribute{Type: 1, Data: []byte("nucleiron\x00")}, netlink.Attribute{Type: 2, Data: []byte("__set1\x00")},
	)
	elements := wireFixture(t, unix.NFT_MSG_NEWSETELEM, unix.NFPROTO_INET,
		netlink.Attribute{Type: 1, Data: []byte("nucleiron\x00")}, netlink.Attribute{Type: 2, Data: []byte("__set1\x00")},
		netlink.Attribute{Type: 3, Data: marshal(netlink.Attribute{Type: unix.NFTA_LIST_ELEM, Data: marshal(netlink.Attribute{Type: unix.NFTA_SET_ELEM_KEY, Data: marshal(netlink.Attribute{Type: unix.NFTA_DATA_VALUE, Data: []byte{0, 53}})})})},
	)
	gen := wireFixture(t, unix.NFT_MSG_NEWGEN, unix.NFPROTO_UNSPEC, netlink.Attribute{Type: unix.NFTA_GEN_ID, Data: be32(7)})
	replies := map[uint16][]netlink.Message{
		unix.NFT_MSG_GETGEN: {gen}, unix.NFT_MSG_GETTABLE: {table, other}, unix.NFT_MSG_GETCHAIN: {chainFixture(t)},
		unix.NFT_MSG_GETRULE: {rule}, unix.NFT_MSG_GETSET: {set}, unix.NFT_MSG_GETSETELEM: {elements},
		unix.NFT_MSG_GETOBJ:       {{Header: netlink.Header{Type: netlink.Done, Flags: netlink.Multi}}},
		unix.NFT_MSG_GETFLOWTABLE: {{Header: netlink.Header{Type: netlink.Done, Flags: netlink.Multi}}},
	}
	setsRead := 0
	socket := nltest.Dial(func(requests []netlink.Message) ([]netlink.Message, error) {
		if len(requests) != 1 || requests[0].Header.Flags&netlink.Request == 0 {
			t.Fatal("before observation sent unexpected request")
		}
		op := uint16(requests[0].Header.Type) & 0xff
		responses, ok := replies[op]
		if !ok {
			t.Fatalf("before observation sent a mutation: %d", op)
		}
		if op == unix.NFT_MSG_GETSETELEM {
			setsRead++
			fields, err := wireFields(requests[0].Data[4:], 0)
			if err != nil || string(fields[2]) != "__set1\x00" {
				t.Fatal("lost anonymous set identity")
			}
		}
		result := append([]netlink.Message(nil), responses...)
		for i := range result {
			result[i].Header.Sequence, result[i].Header.PID = requests[0].Header.Sequence, requests[0].Header.PID
		}
		return result, nil
	})
	defer socket.Close()
	r := &kernelReader{socket: socket}
	before, tables, err := r.Before()
	if err != nil {
		t.Fatal(err)
	}
	if before.Generation != 7 || len(before.Facts) != 5 || len(tables) != 2 || setsRead != 1 || validateBefore(before) != nil {
		t.Fatal("incomplete original snapshot")
	}
	var foundRule, foundElements bool
	for _, fact := range before.Facts {
		if fact.Operation == unix.NFT_MSG_NEWRULE {
			foundRule = bytes.Equal(fact.Data, rule.Data)
		}
		if fact.Operation == unix.NFT_MSG_NEWSETELEM {
			foundElements = bytes.Equal(fact.Data, elements.Data)
		}
	}
	if !foundRule || !foundElements {
		t.Fatal("legacy rule or set elements were reinterpreted or lost")
	}
}
