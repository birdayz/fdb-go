package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestPrimaryScanRule_YieldsScanPlan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	rule := NewPrimaryScanRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("PrimaryScanRule yielded %d expressions, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalScanWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalScanWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if plan == nil {
		t.Fatal("wrapper has no plan")
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	ref := expressions.InitialOf(filter)

	rule := NewPrimaryScanRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("PrimaryScanRule fired on a Filter; yielded %d", len(yielded))
	}
}

// TestRecordQueryScanPlan_EqualsAndHash pins that two scans over
// the same record types + flowedType + direction are equal AND hash
// to the same value.
func TestRecordQueryScanPlan_EqualsAndHash(t *testing.T) {
	t.Parallel()
	a := plans.NewRecordQueryScanPlan([]string{"T", "U"}, values.UnknownType, false)
	b := plans.NewRecordQueryScanPlan([]string{"U", "T"}, values.UnknownType, false) // dedup-sort means same canonical form
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("scan plans with same canonical type-set should be equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equal plans must have equal hashes")
	}

	c := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if a.EqualsWithoutChildren(c) {
		t.Fatal("plans over different type sets should NOT be equal")
	}
	d := plans.NewRecordQueryScanPlan([]string{"T", "U"}, values.UnknownType, true)
	if a.EqualsWithoutChildren(d) {
		t.Fatal("plans with different reverse flag should NOT be equal")
	}
}
