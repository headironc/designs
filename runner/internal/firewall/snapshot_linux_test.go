package firewall

import (
	"errors"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

func snapshotFixture(t *testing.T) []tablePolicy {
	t.Helper()
	c, err := ParseConfiguration([]byte(finalConfiguration))
	if err != nil {
		t.Fatal(err)
	}
	p, err := configuredPolicy(c, []byte("original-operation/configuration"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSnapshotIgnoresOnlyMutableAccountingAndEnumeration(t *testing.T) {
	p := snapshotFixture(t)
	want, err := policyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range p {
		table.table.Use = 999
		for _, chain := range table.chains {
			for _, rule := range chain.rules {
				rule.Handle = 700
				rule.Position = 800
				for _, expression := range rule.Exprs {
					if counter, ok := expression.(*expr.Counter); ok {
						counter.Bytes = 23456
						counter.Packets = 91
					}
				}
			}
		}
	}
	p[0], p[1] = p[1], p[0]
	p[1].chains[0], p[1].chains[2] = p[1].chains[2], p[1].chains[0]
	got, err := policyDigest(p)
	if err != nil || got != want {
		t.Fatalf("mutable accounting changed policy identity: %s %v", got, err)
	}
}

func TestSnapshotDoesNotAcceptMarkerWithoutCompleteOriginalPolicy(t *testing.T) {
	want, err := policyDigest(snapshotFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]tablePolicy){
		"missing table": func(p []tablePolicy) { p[1].chains = nil },
		"missing rule":  func(p []tablePolicy) { p[0].chains[0].rules = p[0].chains[0].rules[1:] },
		"reordered rules": func(p []tablePolicy) {
			r := p[0].chains[0].rules
			r[0], r[1] = r[1], r[0]
		},
		"extra chain": func(p []tablePolicy) {
			p[0].chains = append(p[0].chains, chainPolicy{chain: &nftables.Chain{Table: p[0].table, Name: "extra"}})
		},
		"changed priority": func(p []tablePolicy) { p[0].chains[0].chain.Priority = nftables.ChainPriorityRef(0) },
		"changed default": func(p []tablePolicy) {
			policy := nftables.ChainPolicyDrop
			p[0].chains[0].chain.Policy = &policy
		},
		"changed interface": func(p []tablePolicy) { p[0].chains[0].rules[0].Exprs[1].(*expr.Cmp).Data[0] = 'x' },
		"changed verdict": func(p []tablePolicy) {
			r := p[0].chains[0].rules[0]
			r.Exprs[len(r.Exprs)-1] = &expr.Verdict{Kind: expr.VerdictDrop}
		},
		"changed marker": func(p []tablePolicy) { p[0].chains[2].rules[0].UserData = []byte("another-operation") },
		"changed flags":  func(p []tablePolicy) { p[0].table.Flags = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			p := snapshotFixture(t)
			change(p)
			got, err := policyDigest(p)
			if err == nil && got == want {
				t.Fatal("accepted different policy with original marker")
			}
		})
	}
}

type fixturePolicyReader struct {
	policy  []tablePolicy
	sets    []*nftables.Set
	objects []nftables.Obj
	flows   []*nftables.Flowtable
	failure error
}

func (r fixturePolicyReader) ListTables() ([]*nftables.Table, error) {
	result := []*nftables.Table{{Name: "unrelated", Family: nftables.TableFamilyINet}, {Name: "nucleiron", Family: nftables.TableFamilyIPv4}}
	for _, p := range r.policy {
		result = append(result, p.table)
	}
	return result, r.failure
}
func (r fixturePolicyReader) ListChains() ([]*nftables.Chain, error) {
	var result []*nftables.Chain
	for _, p := range r.policy {
		for _, c := range p.chains {
			result = append(result, c.chain)
		}
	}
	return result, nil
}
func (r fixturePolicyReader) ReadRules(table *nftables.Table, chain *nftables.Chain) ([]*nftables.Rule, error) {
	for _, p := range r.policy {
		for _, c := range p.chains {
			if sameTable(table, p.table) && chain.Name == c.chain.Name {
				return c.rules, nil
			}
		}
	}
	return nil, errors.New("fixture queried unrelated rules")
}
func (r fixturePolicyReader) GetSets(*nftables.Table) ([]*nftables.Set, error) { return r.sets, nil }
func (r fixturePolicyReader) GetNamedObjects(*nftables.Table) ([]nftables.Obj, error) {
	return r.objects, nil
}
func (r fixturePolicyReader) ListFlowtables(*nftables.Table) ([]*nftables.Flowtable, error) {
	return r.flows, nil
}

func TestReadPolicyPreservesCompleteOwnedScopeAndReadFailures(t *testing.T) {
	p := snapshotFixture(t)
	want, _ := policyDigest(p)
	got, err := readPolicy(fixturePolicyReader{policy: p})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := policyDigest(got)
	if err != nil || digest != want {
		t.Fatal("readback changed owned policy")
	}
	failure := errors.New("original read interrupted")
	if _, err := readPolicy(fixturePolicyReader{policy: p, failure: failure}); !errors.Is(err, failure) {
		t.Fatal("original read error lost")
	}
	for _, r := range []fixturePolicyReader{
		{policy: p, sets: []*nftables.Set{{}}},
		{policy: p, objects: []nftables.Obj{nil}},
		{policy: p, flows: []*nftables.Flowtable{{}}},
	} {
		if _, err := readPolicy(r); err == nil {
			t.Fatal("accepted unexpected owned object")
		}
	}
}
