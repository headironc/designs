package firewall

import (
	"errors"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// Before facts retain the original kernel bytes, including legacy lookup/set
// expressions. They are provenance, never a completion or retry permission.
type kernelFact struct {
	Operation uint16 `json:"operation"`
	Data      []byte `json:"data"`
}
type beforePolicy struct {
	Generation uint32       `json:"generation"`
	Facts      []kernelFact `json:"facts"`
}

func factName(message netlink.Message, operation, key uint16) (string, error) {
	if message.Header.Type != netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|operation) || len(message.Data) < 4 || message.Data[1] != 0 || message.Header.Flags&netlink.DumpInterrupted != 0 {
		return "", errors.New("invalid before snapshot response")
	}
	attributes, err := netlink.UnmarshalAttributes(message.Data[4:])
	if err != nil {
		return "", err
	}
	var name string
	for _, attribute := range attributes {
		if attribute.Type != key {
			continue
		}
		if name != "" {
			return "", errors.New("duplicate before snapshot identity")
		}
		name, err = wireName(attribute.Data)
		if err != nil {
			return "", err
		}
	}
	if name == "" {
		return "", errors.New("missing before snapshot identity")
	}
	return name, nil
}

func (r *kernelReader) tableDump(operation uint16, table *nftables.Table, set string) ([]netlink.Message, error) {
	attributes := []netlink.Attribute{{Type: 1, Data: append([]byte(table.Name), 0)}}
	if set != "" {
		attributes = append(attributes, netlink.Attribute{Type: 2, Data: append([]byte(set), 0)})
	}
	raw, err := netlink.MarshalAttributes(attributes)
	if err != nil {
		return nil, err
	}
	return r.socket.Execute(netlink.Message{
		Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | operation), Flags: netlink.Request | netlink.Dump},
		Data:   append([]byte{byte(table.Family), 0, 0, 0}, raw...),
	})
}

func (r *kernelReader) Before() (beforePolicy, []*nftables.Table, error) {
	var before beforePolicy
	gen, err := r.Generation()
	if err != nil {
		return before, nil, err
	}
	before.Generation = gen
	tables, err := r.ListTables()
	if err != nil {
		return before, nil, err
	}
	size := 0
	retain := func(messages []netlink.Message, operation uint16, family nftables.TableFamily, tableName string) error {
		for _, message := range messages {
			name, err := factName(message, operation, 1)
			if err != nil {
				return err
			}
			if tableName == "" {
				if !ownedTable(&nftables.Table{Name: name, Family: nftables.TableFamily(message.Data[0])}) {
					continue
				}
			} else if name != tableName || message.Data[0] != byte(family) {
				return errors.New("before snapshot escaped managed table")
			}
			size += len(message.Data)
			if size > 32*1024 || len(before.Facts) >= 256 {
				return errors.New("before snapshot exceeds dedicated policy bound")
			}
			before.Facts = append(before.Facts, kernelFact{Operation: operation, Data: append([]byte(nil), message.Data...)})
		}
		return nil
	}
	for _, pair := range [][2]uint16{{unix.NFT_MSG_GETTABLE, unix.NFT_MSG_NEWTABLE}, {unix.NFT_MSG_GETCHAIN, unix.NFT_MSG_NEWCHAIN}} {
		messages, err := r.dump(pair[0])
		if err != nil {
			return before, nil, err
		}
		if err := retain(messages, pair[1], 0, ""); err != nil {
			return before, nil, err
		}
	}
	for _, table := range tables {
		if !ownedTable(table) {
			continue
		}
		for _, pair := range [][2]uint16{
			{unix.NFT_MSG_GETRULE, unix.NFT_MSG_NEWRULE},
			{unix.NFT_MSG_GETSET, unix.NFT_MSG_NEWSET},
			{unix.NFT_MSG_GETOBJ, unix.NFT_MSG_NEWOBJ},
			{unix.NFT_MSG_GETFLOWTABLE, unix.NFT_MSG_NEWFLOWTABLE},
		} {
			messages, err := r.tableDump(pair[0], table, "")
			if err != nil {
				return before, nil, err
			}
			if err := retain(messages, pair[1], table.Family, table.Name); err != nil {
				return before, nil, err
			}
			if pair[0] == unix.NFT_MSG_GETSET {
				for _, message := range messages {
					name, err := factName(message, unix.NFT_MSG_NEWSET, unix.NFTA_SET_NAME)
					if err != nil {
						return before, nil, err
					}
					elements, err := r.tableDump(unix.NFT_MSG_GETSETELEM, table, name)
					if err != nil {
						return before, nil, err
					}
					for _, element := range elements {
						set, err := factName(element, unix.NFT_MSG_NEWSETELEM, unix.NFTA_SET_ELEM_LIST_SET)
						if err != nil || set != name {
							return before, nil, errors.New("before snapshot set element identity mismatch")
						}
					}
					if err := retain(elements, unix.NFT_MSG_NEWSETELEM, table.Family, table.Name); err != nil {
						return before, nil, err
					}
				}
			}
		}
	}
	after, err := r.Generation()
	if err != nil {
		return before, nil, err
	}
	if after != gen {
		return before, nil, errors.New("firewall before snapshot changed during observation")
	}
	return before, tables, nil
}
