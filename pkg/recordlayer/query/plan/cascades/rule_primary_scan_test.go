package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestPrimaryScanRule_YieldsScanPlan(t *testing.T) {
	t.Parallel()
	scan := mustOrderedScanFull(t, []string{"Order"})
	ref := mustOrderedScanInitial(t, scan)

	rule := NewPrimaryScanRule()
	yielded := mustFireExpressionRule(t, rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("PrimaryScanRule yielded %d expressions, want 1", len(yielded))
	}
	// RFC-184 W2: PrimaryScanRule yields the BARE scan plan (its own physical
	// Cascades expression), not a wrapper adapter.
	plan, ok := yielded[0].(*plans.RecordQueryScanPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryScanPlan", yielded[0])
	}
	if plan == nil {
		t.Fatal("nil scan plan")
	}
	rts := plan.GetRecordTypes()
	if len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("plan record types = %v, want [Order]", rts)
	}
	if plan.IsReverse() {
		t.Fatal("plan is reversed; want forward scan")
	}
	// Reference should now have 2 members: original logical + physical wrapper.
	if got := len(ref.Members()); got != 2 {
		t.Fatalf("Reference has %d members, want 2 (original + physical)", got)
	}
}

func TestPrimaryScanRule_NoMatchOnNonScan(t *testing.T) {
	t.Parallel()
	// Build a Filter; PrimaryScanRule shouldn't match.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := mustOrderedScanFull(t, []string{"T"})
	filter := mustOrderedScanFilter(t,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(mustOrderedScanInitial(t, scan)),
	)
	ref := mustOrderedScanInitial(t, filter)

	rule := NewPrimaryScanRule()
	yielded := mustFireExpressionRule(t, rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("PrimaryScanRule fired on a Filter; yielded %d", len(yielded))
	}
}

// TestRecordQueryScanPlan_EqualsAndHash pins that two scans over
// the same record types + flowedType + direction are equal AND hash
// to the same value.
func TestRecordQueryScanPlan_EqualsAndHash(t *testing.T) {
	t.Parallel()
	a := mustOrderedScanPlan(t, []string{"T", "U"}, false)
	b := mustOrderedScanPlan(t, []string{"U", "T"}, false) // dedup-sort means same canonical form
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("scan plans with same canonical type-set should be equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equal plans must have equal hashes")
	}

	c := mustOrderedScanPlan(t, []string{"T"}, false)
	if a.EqualsPlanWithoutChildren(c) {
		t.Fatal("plans over different type sets should NOT be equal")
	}
	d := mustOrderedScanPlan(t, []string{"T", "U"}, true)
	if a.EqualsPlanWithoutChildren(d) {
		t.Fatal("plans with different reverse flag should NOT be equal")
	}
}
