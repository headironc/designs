package firewall

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
	"golang.org/x/sys/unix"
)

func wireFixture(t *testing.T, operation uint16, family byte, attributes ...netlink.Attribute) netlink.Message {
	t.Helper()
	raw, err := netlink.MarshalAttributes(attributes)
	if err != nil {
		t.Fatal(err)
	}
	return netlink.Message{Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | operation)}, Data: append([]byte{family, 0, 0x12, 0x34}, raw...)}
}

func be32(value uint32) []byte {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], value)
	return raw[:]
}

func chainFixture(t *testing.T) netlink.Message {
	t.Helper()
	hook, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NFTA_HOOK_HOOKNUM, Data: be32(unix.NF_INET_LOCAL_IN)},
		{Type: unix.NFTA_HOOK_PRIORITY, Data: []byte{0xff, 0xff, 0xff, 0xec}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wireFixture(t, unix.NFT_MSG_NEWCHAIN, unix.NFPROTO_INET,
		netlink.Attribute{Type: unix.NFTA_CHAIN_TABLE, Data: []byte("nucleiron\x00")},
		netlink.Attribute{Type: unix.NFTA_CHAIN_NAME, Data: []byte("guest_input\x00")},
		netlink.Attribute{Type: unix.NFTA_CHAIN_HOOK, Data: hook},
		netlink.Attribute{Type: unix.NFTA_CHAIN_POLICY, Data: be32(1)},
		netlink.Attribute{Type: unix.NFTA_CHAIN_TYPE, Data: []byte("filter\x00")},
		netlink.Attribute{Type: chainFlags, Data: be32(1)},
		netlink.Attribute{Type: unix.NFTA_CHAIN_USE, Data: be32(7)},
		netlink.Attribute{Type: unix.NFTA_CHAIN_PAD},
		netlink.Attribute{Type: unix.NFTA_CHAIN_HANDLE, Data: make([]byte, 8)},
	)
}

func appendField(t *testing.T, message netlink.Message, field netlink.Attribute) netlink.Message {
	t.Helper()
	raw, err := netlink.MarshalAttributes([]netlink.Attribute{field})
	if err != nil {
		t.Fatal(err)
	}
	message.Data = append(append([]byte(nil), message.Data...), raw...)
	return message
}

func TestStrictChainPreservesPolicyAndRefusesUnexplainedFields(t *testing.T) {
	message := chainFixture(t)
	c, err := strictChain(message)
	if err != nil || c.Priority == nil || *c.Priority != -20 || c.Hooknum == nil || *c.Hooknum != nftables.ChainHook(unix.NF_INET_LOCAL_IN) || c.Policy == nil || *c.Policy != nftables.ChainPolicyAccept {
		t.Fatalf("kernel base chain changed: %+v %v", c, err)
	}
	for _, field := range []netlink.Attribute{
		{Type: 999, Data: []byte{1}},
		{Type: 12, Data: []byte("unexplained userdata")},
		{Type: unix.NFTA_CHAIN_POLICY, Data: be32(0)},
		{Type: unix.NFTA_CHAIN_PAD, Data: []byte{1}},
	} {
		if _, err := strictChain(appendField(t, message, field)); err == nil {
			t.Fatal("accepted unsupported or duplicate chain field")
		}
	}
	fields, err := netlink.UnmarshalAttributes(message.Data[4:])
	if err != nil {
		t.Fatal(err)
	}
	for i := range fields {
		if fields[i].Type == chainFlags {
			fields[i].Data = be32(3)
		}
	}
	raw, err := netlink.MarshalAttributes(fields)
	if err != nil {
		t.Fatal(err)
	}
	message.Data = append(message.Data[:4:4], raw...)
	if _, err := strictChain(message); err == nil {
		t.Fatal("accepted hardware offload flags absent from policy")
	}
}

func TestKernelInventoryUsesReadRequestsAndNetworkByteOrder(t *testing.T) {
	table := wireFixture(t, unix.NFT_MSG_NEWTABLE, unix.NFPROTO_INET,
		netlink.Attribute{Type: unix.NFTA_TABLE_NAME, Data: []byte("nucleiron\x00")},
		netlink.Attribute{Type: unix.NFTA_TABLE_FLAGS, Data: be32(1)},
		netlink.Attribute{Type: unix.NFTA_TABLE_USE, Data: be32(5)},
		netlink.Attribute{Type: tablePadding},
		netlink.Attribute{Type: tableHandle, Data: make([]byte, 8)},
	)
	generation := wireFixture(t, unix.NFT_MSG_NEWGEN, unix.NFPROTO_UNSPEC,
		netlink.Attribute{Type: unix.NFTA_GEN_ID, Data: be32(0x12345678)},
		netlink.Attribute{Type: unix.NFTA_GEN_PROC_PID, Data: be32(123)},
		netlink.Attribute{Type: unix.NFTA_GEN_PROC_NAME, Data: []byte("fixture\x00")},
	)
	replies := map[uint16]netlink.Message{unix.NFT_MSG_GETTABLE: table, unix.NFT_MSG_GETCHAIN: chainFixture(t), unix.NFT_MSG_GETGEN: generation}
	calls := 0
	socket := nltest.Dial(func(requests []netlink.Message) ([]netlink.Message, error) {
		calls++
		if len(requests) != 1 {
			t.Fatal("unexpected inventory batch")
		}
		op := uint16(requests[0].Header.Type) & 0xff
		response, ok := replies[op]
		if !ok || requests[0].Header.Flags&netlink.Request == 0 {
			t.Fatal("inventory sent mutation")
		}
		response.Header.Sequence, response.Header.PID = requests[0].Header.Sequence, requests[0].Header.PID
		return []netlink.Message{response}, nil
	})
	defer socket.Close()
	r := &kernelReader{socket: socket}
	tables, err := r.ListTables()
	if err != nil || len(tables) != 1 || tables[0].Flags != 1 || tables[0].Use != 5 {
		t.Fatalf("table inventory changed: %v", err)
	}
	chains, err := r.ListChains()
	if err != nil || len(chains) != 1 {
		t.Fatalf("chain inventory: %v", err)
	}
	gen, err := r.Generation()
	if err != nil || gen != 0x12345678 || calls != 3 {
		t.Fatalf("generation truncated: %x %v", gen, err)
	}
	replies[unix.NFT_MSG_GETTABLE] = appendField(t, table, netlink.Attribute{Type: 6, Data: []byte("unknown userdata")})
	if _, err := r.ListTables(); err == nil {
		t.Fatal("accepted unexplained managed table attribute")
	}
	replies[unix.NFT_MSG_GETGEN] = appendField(t, generation, netlink.Attribute{Type: unix.NFTA_GEN_ID, Data: be32(9)})
	if _, err := r.Generation(); err == nil {
		t.Fatal("accepted ambiguous generation")
	}
}

type stableFixture struct {
	fixturePolicyReader
	values  []uint32
	calls   int
	failure error
	failAt  int
}

func (r *stableFixture) Generation() (uint32, error) {
	r.calls++
	if r.failure != nil && (r.failAt == 0 || r.calls == r.failAt) {
		return 0, r.failure
	}
	return r.values[r.calls-1], nil
}

func TestStableReadRequiresFullGenerationAndRetainsOriginalFailure(t *testing.T) {
	for _, values := range [][]uint32{{0x10001, 0x10001}, {0x10001, 0x20001}} {
		r := &stableFixture{fixturePolicyReader: fixturePolicyReader{policy: snapshotFixture(t)}, values: values}
		got, err := readStablePolicy(r)
		if values[0] == values[1] {
			if err != nil || len(got) != 2 {
				t.Fatalf("stable policy rejected: %v", err)
			}
		} else if err == nil || got != nil {
			t.Fatal("accepted changed full generation with equal low 16 bits")
		}
		if r.calls != 2 {
			t.Fatal("read retried or omitted generation checks")
		}
	}
	failure := errors.New("original generation unavailable")
	r := &stableFixture{failure: failure}
	if _, err := readStablePolicy(r); !errors.Is(err, failure) || r.calls != 1 {
		t.Fatal("generation failure lost or retried")
	}
	r = &stableFixture{fixturePolicyReader: fixturePolicyReader{policy: snapshotFixture(t)}, values: []uint32{7}, failure: failure, failAt: 2}
	if got, err := readStablePolicy(r); !errors.Is(err, failure) || got != nil || r.calls != 2 {
		t.Fatal("accepted snapshot after final generation read failed")
	}
}
