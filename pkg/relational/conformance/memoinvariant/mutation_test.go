package memoinvariant

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The mutation-proofs. RFC-184's exit standard (per §15): a test that only ever
// passes proves nothing — each invariant check must be shown to FIRE on a
// deliberately-broken input. These build small synthetic plans (like
// plan_structural_key_test.go) that violate exactly one invariant and assert the
// checker catches it, then repair the input and assert it goes clean.
//
// Each synthetic plan embeds a real leaf scan for all the RelationalExpression
// boilerplate and overrides only the two or three methods the mutation needs.

// arityPlan reports an arbitrary child list and quantifier list independently,
// so a test can construct the child-without-quantifier shape (the
// ReasonNoQuantifier defect) that a real fully-linked plan makes unrepresentable.
type arityPlan struct {
	plans.RecordQueryPlan
	children []plans.RecordQueryPlan
	quants   []expressions.Quantifier
}

func (p *arityPlan) GetChildren() []plans.RecordQueryPlan     { return p.children }
func (p *arityPlan) GetQuantifiers() []expressions.Quantifier { return p.quants }

// brokenHashPlan is EqualsPlanWithoutChildren-equal to any other brokenHashPlan
// sharing its eqID, while reporting whatever HashCodeWithoutChildren the test
// sets — so a pair can be made equal-but-hash-differ (the memo dedup violation).
type brokenHashPlan struct {
	plans.RecordQueryPlan
	eqID int
	hash uint64
}

func (p *brokenHashPlan) EqualsPlanWithoutChildren(o plans.RecordQueryPlan) bool {
	other, ok := o.(*brokenHashPlan)
	return ok && other.eqID == p.eqID
}
func (p *brokenHashPlan) HashCodeWithoutChildren() uint64 { return p.hash }

// counterHashPlan returns a different hash on every call — a nondeterministic
// HashCodeWithoutChildren, which corrupts memo grouping within a single run.
type counterHashPlan struct {
	plans.RecordQueryPlan
	n *uint64
}

func (p *counterHashPlan) HashCodeWithoutChildren() uint64 {
	*p.n++
	return *p.n
}

func TestMutation_ArityCheckFires(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan(nil, nil, false)

	// Clean: one child, one quantifier — no violation.
	good := &arityPlan{
		RecordQueryPlan: scan,
		children:        []plans.RecordQueryPlan{scan},
		quants:          []expressions.Quantifier{plans.QuantifierOverPlan(scan)},
	}
	if v := arityViolations(good, nil); len(v) != 0 {
		t.Fatalf("arity check flagged a well-formed plan: %v", v)
	}

	// Broken: one child, zero quantifiers — the ReasonNoQuantifier defect shape.
	bad := &arityPlan{
		RecordQueryPlan: scan,
		children:        []plans.RecordQueryPlan{scan},
		quants:          nil,
	}
	if v := arityViolations(bad, nil); len(v) == 0 {
		t.Fatal("arity check did NOT fire on a child-without-quantifier plan — the check is vacuous")
	}

	// Allowlisted → suppressed, proving the no-quantifier-adapter exemption path.
	if v := arityViolations(bad, map[string]bool{planTypeName(bad): true}); len(v) != 0 {
		t.Fatalf("allowlist did not suppress a known no-quantifier adapter: %v", v)
	}
}

func TestMutation_IdentityHashCheckFires(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan(nil, nil, false)

	a := &brokenHashPlan{RecordQueryPlan: scan, eqID: 1, hash: 100}
	b := &brokenHashPlan{RecordQueryPlan: scan, eqID: 1, hash: 200}
	root := &arityPlan{
		RecordQueryPlan: scan,
		children:        []plans.RecordQueryPlan{a, b},
		quants: []expressions.Quantifier{
			plans.QuantifierOverPlan(scan), plans.QuantifierOverPlan(scan),
		},
	}

	// Setup sanity: the two nodes must actually be Equals-equal, else the test
	// proves nothing.
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("test setup broken: nodes must be EqualsPlanWithoutChildren-equal")
	}
	if v := identityHashViolations(root); len(v) == 0 {
		t.Fatal("identity/hash check did NOT fire on equal-but-hash-differ nodes — the check is vacuous")
	}

	// Repair: equal nodes now hash equally → clean.
	b.hash = 100
	if v := identityHashViolations(root); len(v) != 0 {
		t.Fatalf("identity/hash check flagged a consistent pair: %v", v)
	}
}

func TestMutation_HashDeterminismCheckFires(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan(nil, nil, false)

	var ctr uint64
	nd := &counterHashPlan{RecordQueryPlan: scan, n: &ctr}
	root := &arityPlan{
		RecordQueryPlan: scan,
		children:        []plans.RecordQueryPlan{nd},
		quants:          []expressions.Quantifier{plans.QuantifierOverPlan(scan)},
	}
	if v := identityHashViolations(root); len(v) == 0 {
		t.Fatal("determinism check did NOT fire on a nondeterministic HashCodeWithoutChildren")
	}
}
