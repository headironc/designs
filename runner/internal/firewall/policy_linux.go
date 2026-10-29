package firewall

import (
	"encoding/binary"
	"errors"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

type chainPolicy struct {
	chain *nftables.Chain
	rules []*nftables.Rule
}

type tablePolicy struct {
	table  *nftables.Table
	chains []chainPolicy
}

// configuredPolicy constructs the complete desired state without opening a
// socket. Both sending and original-outcome readback use this policy owner.
func configuredPolicy(c Configuration, marker []byte) ([]tablePolicy, error) {
	if !c.valid() || len(marker) == 0 || len(marker) > unix.NFT_USERDATA_MAXLEN {
		return nil, errors.New("invalid dedicated firewall policy")
	}
	inet := &nftables.Table{Family: nftables.TableFamilyINet, Name: "nucleiron"}
	bridge := &nftables.Table{Family: nftables.TableFamilyBridge, Name: "nucleiron"}
	base := func(table *nftables.Table, name string, hook *nftables.ChainHook, priority nftables.ChainPriority) chainPolicy {
		accept := nftables.ChainPolicyAccept
		return chainPolicy{chain: &nftables.Chain{Table: table, Name: name, Type: nftables.ChainTypeFilter, Hooknum: hook, Priority: nftables.ChainPriorityRef(priority), Policy: &accept}}
	}
	add := func(chain *chainPolicy, matches []expr.Any, verdict expr.VerdictKind) {
		matches = append(matches, &expr.Counter{}, &expr.Verdict{Kind: verdict})
		chain.rules = append(chain.rules, &nftables.Rule{Table: chain.chain.Table, Chain: chain.chain, Exprs: matches})
	}
	input := base(inet, "guest_input", nftables.ChainHookInput, -20)
	dhcp := interfaceMatch(expr.MetaKeyIIFNAME, c.bridge)
	dhcp = append(dhcp, protocolMatch(unix.IPPROTO_UDP)...)
	dhcp = append(dhcp, portMatch(0, 68)...)
	dhcp = append(dhcp, portMatch(2, 67)...)
	add(&input, dhcp, expr.VerdictAccept)
	dns := guestIPv4(c)
	dns = append(dns, protocolMatch(unix.IPPROTO_UDP)...)
	dns = append(dns, portMatch(2, 53)...)
	add(&input, dns, expr.VerdictAccept)
	// The legacy set shares one counter. Separate fixed accept rules preserve
	// packet decisions, while reporting one counter per allowed TCP service.
	for _, port := range c.ports {
		service := guestIPv4(c)
		service = append(service, protocolMatch(unix.IPPROTO_TCP)...)
		service = append(service, portMatch(2, port)...)
		add(&input, service, expr.VerdictAccept)
	}
	add(&input, interfaceMatch(expr.MetaKeyIIFNAME, c.bridge), expr.VerdictDrop)
	forward := base(inet, "guest_forward", nftables.ChainHookForward, -20)
	add(&forward, interfaceMatch(expr.MetaKeyIIFNAME, c.bridge), expr.VerdictDrop)
	add(&forward, interfaceMatch(expr.MetaKeyOIFNAME, c.bridge), expr.VerdictDrop)
	ipv6 := base(bridge, "guest_ipv6", nftables.ChainHookInput, -200)
	ether := interfaceMatch(expr.MetaKeyBRIIIFNAME, c.bridge)
	ether = append(ether, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x86, 0xdd}})
	add(&ipv6, ether, expr.VerdictDrop)
	peers := base(bridge, "guest_peers", nftables.ChainHookForward, -200)
	add(&peers, interfaceMatch(expr.MetaKeyBRIIIFNAME, c.bridge), expr.VerdictDrop)
	add(&peers, interfaceMatch(expr.MetaKeyBRIOIFNAME, c.bridge), expr.VerdictDrop)
	// This unhooked chain carries provenance without matching any packet path.
	managed := chainPolicy{chain: &nftables.Chain{Table: inet, Name: "managed"}}
	managed.rules = []*nftables.Rule{{Table: inet, Chain: managed.chain, Exprs: []expr.Any{&expr.Counter{}}, UserData: append([]byte(nil), marker...)}}
	return []tablePolicy{{inet, []chainPolicy{input, forward, managed}}, {bridge, []chainPolicy{ipv6, peers}}}, nil
}

func interfaceMatch(key expr.MetaKey, name string) []expr.Any {
	var value [unix.IFNAMSIZ]byte
	copy(value[:], name)
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: value[:]}}
}

func protocolMatch(protocol byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protocol}}}
}

func portMatch(offset uint32, port uint16) []expr.Any {
	value := make([]byte, 2)
	binary.BigEndian.PutUint16(value, port)
	return []expr.Any{&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: offset, Len: 2}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: value}}
}

func guestIPv4(c Configuration) []expr.Any {
	source := c.subnet.Addr().As4()
	gateway := c.gateway.As4()
	result := interfaceMatch(expr.MetaKeyIIFNAME, c.bridge)
	return append(result,
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: net.CIDRMask(c.subnet.Bits(), 32), Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: source[:]},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: gateway[:]},
	)
}

// queuePolicy never flushes. Its owner persists the original intent before
// committing these deletes and creates together in exactly one transaction.
func queuePolicy(conn *nftables.Conn, policy []tablePolicy, existing []*nftables.Table) error {
	if conn == nil || len(policy) != 2 {
		return errors.New("missing dedicated firewall transaction")
	}
	seen := make(map[nftables.TableFamily]bool)
	for _, table := range existing {
		if table == nil {
			return errors.New("invalid firewall table inventory")
		}
		if table.Name != "nucleiron" || table.Family != nftables.TableFamilyINet && table.Family != nftables.TableFamilyBridge {
			continue
		}
		if seen[table.Family] {
			return errors.New("duplicate managed firewall table")
		}
		seen[table.Family] = true
	}
	for _, table := range existing {
		if table.Name == "nucleiron" && (table.Family == nftables.TableFamilyINet || table.Family == nftables.TableFamilyBridge) {
			conn.DelTable(table)
		}
	}
	for _, table := range policy {
		conn.AddTable(table.table)
		for _, chain := range table.chains {
			conn.AddChain(chain.chain)
			for _, rule := range chain.rules {
				conn.AddRule(rule)
			}
		}
	}
	return nil
}
