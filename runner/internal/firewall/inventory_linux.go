package firewall

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// These UAPI values are absent from the pinned x/sys. Source:
// Linux v6.12 include/uapi/linux/netfilter/nf_tables.h.
const (
	tableHandle   = 4
	tablePadding  = 5
	chainFlags    = 10
	baseChainFlag = 1
)

func wireFields(raw []byte, padding uint16) (map[uint16][]byte, error) {
	attributes, err := netlink.UnmarshalAttributes(raw)
	if err != nil {
		return nil, err
	}
	fields := make(map[uint16][]byte)
	for _, attribute := range attributes {
		key := attribute.Type &^ uint16(unix.NLA_F_NESTED)
		if padding != 0 && key == padding && len(attribute.Data) == 0 {
			continue
		}
		if _, ok := fields[key]; ok {
			return nil, errors.New("duplicate firewall inventory field")
		}
		fields[key] = attribute.Data
	}
	return fields, nil
}

func wireName(raw []byte) (string, error) {
	if len(raw) < 2 || len(raw) > 256 || raw[len(raw)-1] != 0 || bytes.IndexByte(raw[:len(raw)-1], 0) >= 0 {
		return "", errors.New("invalid firewall inventory name")
	}
	return string(raw[:len(raw)-1]), nil
}

func wireNumber(raw []byte) (uint32, error) {
	if len(raw) != 4 {
		return 0, errors.New("invalid firewall inventory integer")
	}
	return binary.BigEndian.Uint32(raw), nil
}

func inventoryFields(message netlink.Message, operation uint16, padding uint16) (map[uint16][]byte, error) {
	if message.Header.Type != netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|operation) || len(message.Data) < 4 || message.Data[1] != unix.NFNETLINK_V0 || message.Header.Flags&netlink.DumpInterrupted != 0 {
		return nil, errors.New("invalid or interrupted firewall inventory response")
	}
	return wireFields(message.Data[4:], padding)
}

func (r *kernelReader) dump(operation uint16) ([]netlink.Message, error) {
	if r.socket == nil {
		return nil, errors.New("missing original firewall socket")
	}
	messages, err := r.socket.Execute(netlink.Message{
		Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | operation), Flags: netlink.Request | netlink.Dump},
		Data:   []byte{unix.NFPROTO_UNSPEC, unix.NFNETLINK_V0, 0, 0},
	})
	if err != nil {
		return nil, err
	}
	if len(messages) > 4096 {
		return nil, errors.New("firewall inventory exceeds observation bound")
	}
	return messages, nil
}

func (r *kernelReader) ListTables() ([]*nftables.Table, error) {
	messages, err := r.dump(unix.NFT_MSG_GETTABLE)
	if err != nil {
		return nil, err
	}
	var tables []*nftables.Table
	for _, message := range messages {
		fields, err := inventoryFields(message, unix.NFT_MSG_NEWTABLE, tablePadding)
		if err != nil {
			return nil, err
		}
		name, err := wireName(fields[unix.NFTA_TABLE_NAME])
		if err != nil {
			return nil, err
		}
		table := &nftables.Table{Name: name, Family: nftables.TableFamily(message.Data[0])}
		if ownedTable(table) {
			for key, raw := range fields {
				switch key {
				case unix.NFTA_TABLE_NAME:
				case unix.NFTA_TABLE_FLAGS:
					table.Flags, err = wireNumber(raw)
				case unix.NFTA_TABLE_USE:
					table.Use, err = wireNumber(raw)
				case tableHandle:
					if len(raw) != 8 {
						err = errors.New("invalid firewall table handle")
					}
				default:
					err = errors.New("unsupported managed firewall table attribute")
				}
				if err != nil {
					return nil, err
				}
			}
			if _, ok := fields[unix.NFTA_TABLE_FLAGS]; !ok {
				return nil, errors.New("missing firewall table flags")
			}
		}
		tables = append(tables, table)
	}
	return tables, nil
}

func (r *kernelReader) ListChains() ([]*nftables.Chain, error) {
	messages, err := r.dump(unix.NFT_MSG_GETCHAIN)
	if err != nil {
		return nil, err
	}
	var chains []*nftables.Chain
	for _, message := range messages {
		chain, err := strictChain(message)
		if err != nil {
			return nil, err
		}
		chains = append(chains, chain)
	}
	return chains, nil
}

func strictChain(message netlink.Message) (*nftables.Chain, error) {
	fields, err := inventoryFields(message, unix.NFT_MSG_NEWCHAIN, unix.NFTA_CHAIN_PAD)
	if err != nil {
		return nil, err
	}
	tableName, err := wireName(fields[unix.NFTA_CHAIN_TABLE])
	if err != nil {
		return nil, err
	}
	name, err := wireName(fields[unix.NFTA_CHAIN_NAME])
	if err != nil {
		return nil, err
	}
	c := &nftables.Chain{Name: name, Table: &nftables.Table{Name: tableName, Family: nftables.TableFamily(message.Data[0])}}
	if !ownedTable(c.Table) {
		return c, nil
	}
	var flags uint32
	for key, raw := range fields {
		switch key {
		case unix.NFTA_CHAIN_NAME, unix.NFTA_CHAIN_TABLE:
		case unix.NFTA_CHAIN_HANDLE:
			if len(raw) != 8 {
				err = errors.New("invalid firewall chain handle")
			}
		case unix.NFTA_CHAIN_USE:
			_, err = wireNumber(raw)
		case chainFlags:
			flags, err = wireNumber(raw)
		case unix.NFTA_CHAIN_TYPE:
			var value string
			value, err = wireName(raw)
			c.Type = nftables.ChainType(value)
		case unix.NFTA_CHAIN_POLICY:
			var value uint32
			value, err = wireNumber(raw)
			policy := nftables.ChainPolicy(value)
			c.Policy = &policy
		case unix.NFTA_CHAIN_HOOK:
			var hook map[uint16][]byte
			hook, err = wireFields(raw, 0)
			if err != nil {
				break
			}
			if len(hook) != 2 {
				err = errors.New("unsupported firewall hook attributes")
				break
			}
			var number, priority uint32
			number, err = wireNumber(hook[unix.NFTA_HOOK_HOOKNUM])
			if err != nil {
				break
			}
			priority, err = wireNumber(hook[unix.NFTA_HOOK_PRIORITY])
			c.Hooknum, c.Priority = nftables.ChainHookRef(nftables.ChainHook(number)), nftables.ChainPriorityRef(nftables.ChainPriority(int32(priority)))
		case unix.NFTA_CHAIN_COUNTERS:
			counter := &expr.Counter{}
			err = decodeExpression(message.Data[0], raw, counter)
			if err != nil {
				break
			}
			var encoded []byte
			encoded, err = expr.MarshalExprData(message.Data[0], counter)
			if err == nil {
				err = matchAttributes(raw, encoded, unix.NFTA_COUNTER_PAD, 0)
			}
		default:
			err = errors.New("unsupported managed firewall chain attribute")
		}
		if err != nil {
			return nil, err
		}
	}
	if c.Hooknum != nil {
		if c.Policy == nil || c.Type == "" || flags != baseChainFlag {
			return nil, errors.New("incomplete firewall base chain")
		}
	} else if c.Policy != nil || c.Type != "" || flags != 0 {
		return nil, errors.New("unexpected firewall regular chain policy")
	}
	return c, nil
}

func (r *kernelReader) Generation() (uint32, error) {
	if r.socket == nil {
		return 0, errors.New("missing original firewall socket")
	}
	messages, err := r.socket.Execute(netlink.Message{
		Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_GETGEN), Flags: netlink.Request},
		Data:   []byte{unix.NFPROTO_UNSPEC, unix.NFNETLINK_V0, 0, 0},
	})
	if err != nil {
		return 0, fmt.Errorf("read firewall generation: %w", err)
	}
	if len(messages) != 1 {
		return 0, errors.New("ambiguous firewall generation response")
	}
	fields, err := inventoryFields(messages[0], unix.NFT_MSG_NEWGEN, 0)
	if err != nil {
		return 0, err
	}
	if messages[0].Data[0] != unix.NFPROTO_UNSPEC {
		return 0, errors.New("invalid firewall generation family")
	}
	for key, raw := range fields {
		switch key {
		case unix.NFTA_GEN_ID, unix.NFTA_GEN_PROC_PID:
			_, err = wireNumber(raw)
		case unix.NFTA_GEN_PROC_NAME:
			_, err = wireName(raw)
		default:
			err = errors.New("unsupported firewall generation attribute")
		}
		if err != nil {
			return 0, err
		}
	}
	return wireNumber(fields[unix.NFTA_GEN_ID])
}

type stablePolicyReader interface {
	policyReader
	Generation() (uint32, error)
}

func readStablePolicy(reader stablePolicyReader) ([]tablePolicy, error) {
	before, err := reader.Generation()
	if err != nil {
		return nil, err
	}
	policy, err := readPolicy(reader)
	if err != nil {
		return nil, err
	}
	after, err := reader.Generation()
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, errors.New("firewall rules changed during observation; outcome remains unknown")
	}
	return policy, nil
}
