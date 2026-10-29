package firewall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// Rule order is significant. Table and chain enumeration order is not.
// These records contain semantics, never kernel handles or live counters.
type ruleSnapshot struct {
	Expressions [][]byte
	UserData    []byte
}

type chainSnapshot struct {
	Name     string
	Hook     *nftables.ChainHook
	Priority *nftables.ChainPriority
	Type     nftables.ChainType
	Policy   *nftables.ChainPolicy
	Device   string
	Rules    []ruleSnapshot
}

type tableSnapshot struct {
	Name   string
	Family nftables.TableFamily
	Flags  uint32
	Chains []chainSnapshot
}

func ownedTable(t *nftables.Table) bool {
	return t != nil && t.Name == "nucleiron" && (t.Family == nftables.TableFamilyINet || t.Family == nftables.TableFamilyBridge)
}

func sameTable(a, b *nftables.Table) bool {
	return a != nil && b != nil && a.Name == b.Name && a.Family == b.Family
}

// policyDigest uses the library's protocol encoding for expressions. Unknown
// expressions must remain errors, not disappear from an accepted snapshot.
func policyDigest(policy []tablePolicy) (string, error) {
	var tables []tableSnapshot
	seen := make(map[nftables.TableFamily]bool)
	for _, table := range policy {
		if !ownedTable(table.table) || seen[table.table.Family] {
			return "", errors.New("invalid managed table snapshot")
		}
		seen[table.table.Family] = true
		t := tableSnapshot{Name: table.table.Name, Family: table.table.Family, Flags: table.table.Flags}
		chains := make(map[string]bool)
		for _, chain := range table.chains {
			c := chain.chain
			if c == nil || c.Name == "" || !sameTable(c.Table, table.table) || chains[c.Name] {
				return "", errors.New("invalid managed chain snapshot")
			}
			chains[c.Name] = true
			cs := chainSnapshot{Name: c.Name, Hook: c.Hooknum, Priority: c.Priority, Type: c.Type, Policy: c.Policy, Device: c.Device}
			for _, rule := range chain.rules {
				if rule == nil || !sameTable(rule.Table, table.table) || rule.Chain == nil || !sameTable(rule.Chain.Table, table.table) || rule.Chain.Name != c.Name || rule.Flags != 0 {
					return "", errors.New("invalid managed rule snapshot")
				}
				rs := ruleSnapshot{UserData: rule.UserData}
				for _, expression := range rule.Exprs {
					if expression == nil {
						return "", errors.New("missing firewall expression")
					}
					if counter, ok := expression.(*expr.Counter); ok {
						if counter == nil {
							return "", errors.New("missing firewall counter")
						}
						expression = &expr.Counter{}
					}
					encoded, err := expr.Marshal(byte(t.Family), expression)
					if err != nil {
						return "", fmt.Errorf("encode firewall snapshot expression: %w", err)
					}
					rs.Expressions = append(rs.Expressions, encoded)
				}
				cs.Rules = append(cs.Rules, rs)
			}
			t.Chains = append(t.Chains, cs)
		}
		sort.Slice(t.Chains, func(i, j int) bool { return t.Chains[i].Name < t.Chains[j].Name })
		tables = append(tables, t)
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Family < tables[j].Family })
	raw, err := json.Marshal(tables)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

type policyReader interface {
	ListTables() ([]*nftables.Table, error)
	ListChains() ([]*nftables.Chain, error)
	// ReadRules must reject unsupported raw expressions and attributes. The
	// SDK's GetRules silently discards unsupported expressions and is unsafe
	// as a completion proof without that additional wire validation.
	ReadRules(*nftables.Table, *nftables.Chain) ([]*nftables.Rule, error)
	GetSets(*nftables.Table) ([]*nftables.Set, error)
	GetNamedObjects(*nftables.Table) ([]nftables.Obj, error)
	ListFlowtables(*nftables.Table) ([]*nftables.Flowtable, error)
}

// readPolicy makes no mutation. The connection owner must bracket all reads
// with an unchanged kernel generation before using this as completion proof.
func readPolicy(reader policyReader) ([]tablePolicy, error) {
	tables, err := reader.ListTables()
	if err != nil {
		return nil, fmt.Errorf("list firewall snapshot tables: %w", err)
	}
	chains, err := reader.ListChains()
	if err != nil {
		return nil, fmt.Errorf("list firewall snapshot chains: %w", err)
	}
	for _, chain := range chains {
		if chain == nil || chain.Table == nil {
			return nil, errors.New("missing firewall inventory chain")
		}
		if !ownedTable(chain.Table) {
			continue
		}
		found := false
		for _, table := range tables {
			if sameTable(table, chain.Table) {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("managed firewall chain has no inventory table")
		}
	}
	var result []tablePolicy
	for _, table := range tables {
		if table == nil {
			return nil, errors.New("missing firewall inventory table")
		}
		if !ownedTable(table) {
			continue
		}
		sets, err := reader.GetSets(table)
		if err != nil {
			return nil, fmt.Errorf("read managed firewall sets: %w", err)
		}
		objects, err := reader.GetNamedObjects(table)
		if err != nil {
			return nil, fmt.Errorf("read managed firewall objects: %w", err)
		}
		flows, err := reader.ListFlowtables(table)
		if err != nil {
			return nil, fmt.Errorf("read managed firewall flowtables: %w", err)
		}
		if len(sets) != 0 || len(objects) != 0 || len(flows) != 0 {
			return nil, errors.New("unexpected objects in managed firewall table")
		}
		p := tablePolicy{table: table}
		for _, chain := range chains {
			if chain == nil || chain.Table == nil {
				return nil, errors.New("missing firewall inventory chain")
			}
			if !sameTable(chain.Table, table) {
				continue
			}
			rules, err := reader.ReadRules(table, chain)
			if err != nil {
				return nil, fmt.Errorf("read managed firewall rules: %w", err)
			}
			p.chains = append(p.chains, chainPolicy{chain: chain, rules: rules})
		}
		result = append(result, p)
	}
	if _, err := policyDigest(result); err != nil {
		return nil, err
	}
	return result, nil
}
