package plans_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestRecursiveLevelUnion_CanCorrelateFalse_PropagatesOuterAlias pins the fix
// for the DIVERGENCES "UNSAFE, fix first" item: the physical
// RecordQueryRecursiveLevelUnionPlan must NOT anchor a Cascades correlation
// (Java has no canCorrelate override → false), so an OUTER alias a leg
// legitimately reads propagates OUT through the operator even when Go's
// human-readable alias reuse collides that outer alias with the union's own
// leg alias.
//
// Shape: the recursive leg is correlated to alias "X"; the initial leg's own
// quantifier is ALSO aliased "X" (the collision Go's alias reuse can produce).
// With the buggy canCorrelate()==true, Reference.GetCorrelatedTo suppresses X
// (it is bound in the plan's own quantifier aliases), stranding the outer
// binding — a wrong-rows shape. With the corrected false, X propagates.
func TestRecursiveLevelUnion_CanCorrelateFalse_PropagatesOuterAlias(t *testing.T) {
	t.Parallel()

	outer := values.NamedCorrelationIdentifier("X")

	// Initial leg: a bare scan, but its quantifier is aliased "X" — the
	// collision. (A recursion never reads a sibling leg by its quantifier
	// alias; the level binding rides the temp table, so an "X" appearing on a
	// leg's correlated-to set is an EXTERNAL correlation, not the recursion.)
	initialScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	initialQ := expressions.NamedPhysicalQuantifier(outer, expressions.FinalOf(initialScan))

	// Recursive leg: a filter correlated to the outer alias "X".
	recScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	pred := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(outer),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	recFilter := plans.NewRecordQueryPredicatesFilterPlan(recScan, []predicates.QueryPredicate{pred})
	recursiveQ := expressions.NewPhysicalQuantifier(expressions.FinalOf(recFilter))

	plan := plans.NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"),
		false,
	)

	if plan.CanCorrelate() {
		t.Fatal("RecordQueryRecursiveLevelUnionPlan.CanCorrelate() must be false (Java parity)")
	}

	ref := expressions.FinalOf(plan)
	corr := ref.GetCorrelatedTo()
	if _, ok := corr[outer]; !ok {
		t.Fatalf("outer alias %q was suppressed by the level-union operator (correlated-to = %v) — "+
			"canCorrelate()==true wrongly anchored it; the recursive leg's outer binding is stranded",
			outer.Name(), corr)
	}
}
