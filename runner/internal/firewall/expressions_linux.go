package firewall

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// strictExpressions handles only expressions emitted by this policy. SDK
// decoding alone drops unknown expressions and ignores unknown attributes.
func strictExpressions(family byte, raw []byte) ([]expr.Any, error) {
	list, err := netlink.UnmarshalAttributes(raw)
	if err != nil {
		return nil, err
	}
	var result []expr.Any
	for _, item := range list {
		if item.Type&^uint16(unix.NLA_F_NESTED) != unix.NFTA_LIST_ELEM {
			return nil, errors.New("invalid firewall expression list element")
		}
		attributes, err := netlink.UnmarshalAttributes(item.Data)
		if err != nil || len(attributes) != 2 {
			return nil, errors.New("incomplete firewall expression")
		}
		var name, data []byte
		for _, attribute := range attributes {
			switch attribute.Type &^ uint16(unix.NLA_F_NESTED) {
			case unix.NFTA_EXPR_NAME:
				name = attribute.Data
			case unix.NFTA_EXPR_DATA:
				data = attribute.Data
			default:
				return nil, errors.New("unsupported firewall expression attribute")
			}
		}
		var expression expr.Any
		switch string(name) {
		case "meta\x00":
			expression = &expr.Meta{}
		case "cmp\x00":
			expression = &expr.Cmp{}
		case "payload\x00":
			expression = &expr.Payload{}
		case "bitwise\x00":
			expression = &expr.Bitwise{}
			data, err = booleanBitwise(data)
			if err != nil {
				return nil, err
			}
		case "counter\x00":
			expression = &expr.Counter{}
		case "immediate\x00":
			expression = &expr.Verdict{}
		default:
			return nil, errors.New("unsupported firewall expression")
		}
		if err := decodeExpression(family, data, expression); err != nil {
			return nil, err
		}
		encoded, err := expr.MarshalExprData(family, expression)
		if err != nil {
			return nil, err
		}
		var padding uint16
		if _, ok := expression.(*expr.Counter); ok {
			padding = unix.NFTA_COUNTER_PAD
		}
		if err := matchAttributes(data, encoded, padding, 0); err != nil {
			return nil, errors.New("firewall expression could not be reconstructed completely")
		}
		result = append(result, expression)
	}
	return result, nil
}

func booleanBitwise(raw []byte) ([]byte, error) {
	// Linux nft_bitwise_{fast_,}dump emits NFTA_BITWISE_OP=0 for the
	// implicit mask/XOR operation. The pinned SDK represents only that
	// operation and omits this attribute. Preserve its exact meaning;
	// shifts, malformed values and duplicate operation fields refuse.
	const bitwiseOperation = 6 // Linux v6.12 nf_tables.h: NFTA_BITWISE_OP.
	attributes, err := netlink.UnmarshalAttributes(raw)
	if err != nil {
		return nil, err
	}
	seen := false
	var result []netlink.Attribute
	for _, attribute := range attributes {
		if attribute.Type != bitwiseOperation {
			result = append(result, attribute)
			continue
		}
		op, err := wireNumber(attribute.Data)
		if err != nil || op != 0 || seen {
			return nil, errors.New("unsupported firewall bitwise operation")
		}
		seen = true
	}
	return netlink.MarshalAttributes(result)
}

// Several SDK expression decoders assume valid kernel field lengths. Treat
// malformed remote bytes as a failed observation instead of crashing recovery.
func decodeExpression(family byte, raw []byte, expression expr.Any) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("malformed firewall expression data")
		}
	}()
	if err := expr.Unmarshal(family, raw, expression); err != nil {
		return fmt.Errorf("decode firewall expression: %w", err)
	}
	return nil
}

// Kernel dumps historically omit NLA_F_NESTED. Follow the SDK's known nested
// schema, preserving every value and rejecting extra or duplicate fields.
// Only the explicitly selected empty alignment padding is immaterial.
func matchAttributes(raw, expected []byte, padding uint16, depth int) error {
	if depth > 8 {
		return errors.New("excessively nested firewall expression")
	}
	attributes, err := netlink.UnmarshalAttributes(raw)
	if err != nil {
		return err
	}
	wanted, err := netlink.UnmarshalAttributes(expected)
	if err != nil {
		return err
	}
	seen := make(map[uint16][]byte)
	for _, a := range attributes {
		key := a.Type &^ uint16(unix.NLA_F_NESTED)
		if padding != 0 && key == padding && len(a.Data) == 0 {
			continue
		}
		if _, ok := seen[key]; ok {
			return errors.New("duplicate firewall expression attribute")
		}
		seen[key] = a.Data
	}
	if len(seen) != len(wanted) {
		return errors.New("different firewall expression attributes")
	}
	for _, a := range wanted {
		actual, ok := seen[a.Type&^uint16(unix.NLA_F_NESTED)]
		if !ok {
			return errors.New("missing firewall expression attribute")
		}
		if a.Type&unix.NLA_F_NESTED != 0 {
			if err := matchAttributes(actual, a.Data, 0, depth+1); err != nil {
				return err
			}
		} else if !bytes.Equal(actual, a.Data) {
			return errors.New("different firewall expression value")
		}
	}
	return nil
}
