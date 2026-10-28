package firewall

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// kernelReader shares its lasting socket with the transaction connection. It
// never dials, reconnects, or mutates the ruleset while collecting a snapshot.
type kernelReader struct {
	*nftables.Conn
	socket *netlink.Conn
}

func (r *kernelReader) ReadRules(table *nftables.Table, chain *nftables.Chain) ([]*nftables.Rule, error) {
	if r.socket == nil || !ownedTable(table) || chain == nil || !sameTable(chain.Table, table) {
		return nil, errors.New("invalid managed firewall rule read")
	}
	attributes, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NFTA_RULE_TABLE, Data: append([]byte(table.Name), 0)},
		{Type: unix.NFTA_RULE_CHAIN, Data: append([]byte(chain.Name), 0)},
	})
	if err != nil {
		return nil, err
	}
	messages, err := r.socket.Execute(netlink.Message{
		Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_GETRULE), Flags: netlink.Request | netlink.Dump},
		Data:   append([]byte{byte(table.Family), unix.NFNETLINK_V0, 0, 0}, attributes...),
	})
	if err != nil {
		return nil, fmt.Errorf("read original firewall rules: %w", err)
	}
	if len(messages) > 512 {
		return nil, errors.New("managed firewall rule inventory exceeds policy bound")
	}
	var rules []*nftables.Rule
	for _, message := range messages {
		rule, err := strictRule(message, table, chain)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func strictRule(message netlink.Message, table *nftables.Table, chain *nftables.Chain) (*nftables.Rule, error) {
	// The response res_id contains the low generation bits, not zero. The
	// owner brackets the whole inventory with full-width generation reads.
	if !ownedTable(table) || chain == nil || !sameTable(chain.Table, table) || message.Header.Type != netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_NEWRULE) || message.Header.Flags&netlink.DumpInterrupted != 0 || len(message.Data) < 4 || message.Data[0] != byte(table.Family) || message.Data[1] != unix.NFNETLINK_V0 {
		return nil, errors.New("unexpected firewall rule response identity")
	}
	attributes, err := netlink.UnmarshalAttributes(message.Data[4:])
	if err != nil {
		return nil, err
	}
	rule := &nftables.Rule{Table: table, Chain: chain}
	seen := make(map[uint16]bool)
	for _, attribute := range attributes {
		key := attribute.Type &^ uint16(unix.NLA_F_NESTED|unix.NLA_F_NET_BYTEORDER)
		if attribute.Type == unix.NFTA_RULE_PAD && len(attribute.Data) == 0 {
			continue
		}
		if seen[key] {
			return nil, errors.New("duplicate firewall rule response attribute")
		}
		seen[key] = true
		switch attribute.Type &^ uint16(unix.NLA_F_NESTED) {
		case unix.NFTA_RULE_TABLE:
			if !bytes.Equal(attribute.Data, append([]byte(table.Name), 0)) {
				return nil, errors.New("firewall rule response table mismatch")
			}
		case unix.NFTA_RULE_CHAIN:
			if !bytes.Equal(attribute.Data, append([]byte(chain.Name), 0)) {
				return nil, errors.New("firewall rule response chain mismatch")
			}
		case unix.NFTA_RULE_HANDLE, unix.NFTA_RULE_POSITION:
			if len(attribute.Data) != 8 {
				return nil, errors.New("invalid firewall rule kernel handle")
			}
			if key == unix.NFTA_RULE_HANDLE {
				rule.Handle = binary.BigEndian.Uint64(attribute.Data)
			} else {
				rule.Position = binary.BigEndian.Uint64(attribute.Data)
			}
		case unix.NFTA_RULE_EXPRESSIONS:
			rule.Exprs, err = strictExpressions(byte(table.Family), attribute.Data)
			if err != nil {
				return nil, err
			}
		case unix.NFTA_RULE_USERDATA:
			if len(attribute.Data) > unix.NFT_USERDATA_MAXLEN {
				return nil, errors.New("oversized firewall rule provenance")
			}
			rule.UserData = append([]byte(nil), attribute.Data...)
		default:
			return nil, errors.New("unsupported firewall rule response attribute")
		}
	}
	if !seen[unix.NFTA_RULE_TABLE] || !seen[unix.NFTA_RULE_CHAIN] || !seen[unix.NFTA_RULE_EXPRESSIONS] || len(rule.Exprs) == 0 {
		return nil, errors.New("incomplete firewall rule response")
	}
	return rule, nil
}
