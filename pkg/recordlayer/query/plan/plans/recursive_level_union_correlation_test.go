package plans_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPlanTest[T any](t testing.TB, constructor func() (T, error)) T {
	t.Helper()
	result, err := constructor()
	if err != nil {
		t.Fatalf("construction failed: %v", err)
	}
	return result
}

func recursiveTestType() values.Type {
	return values.NewRecordType("recursive_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
}

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
	initialScan := mustPlanTest(t, func() (*plans.RecordQueryScanPlan, error) {
		return plans.NewRecordQueryScanPlan([]string{"T"}, recursiveTestType(), false)
	})
	initialQ := expressions.NamedPhysicalQuantifier(outer, expressions.FinalOf(initialScan))

	// Recursive leg: a filter correlated to the outer alias "X".
	recScan := mustPlanTest(t, func() (*plans.RecordQueryScanPlan, error) {
		return plans.NewRecordQueryScanPlan([]string{"T"}, recursiveTestType(), false)
	})
	outerValue := mustPlanTest(t, func() (values.QuantifiedObjectValue, error) {
		return values.NewQuantifiedObjectValue(outer, recursiveTestType())
	})
	pred := predicates.NewComparisonPredicate(
		outerValue,
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	recFilter := mustPlanTest(t, func() (*plans.RecordQueryPredicatesFilterPlan, error) {
		return plans.NewRecordQueryPredicatesFilterPlan(recScan, []predicates.QueryPredicate{pred})
	})
	recursiveQ := expressions.NewPhysicalQuantifier(expressions.FinalOf(recFilter))

	plan := mustPlanTest(t, func() (*plans.RecordQueryRecursiveLevelUnionPlan, error) {
		return plans.NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
			initialQ, recursiveQ,
			values.NamedCorrelationIdentifier("scan"),
			values.NamedCorrelationIdentifier("insert"),
			false,
		)
	})

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

// TestFlatMapPlan_WithQuantifiers_NoStalePlanSnapshot pins the item-3b fix
// (DIVERGENCES "memo costs an expression that is not the one that executes"):
// a physical plan stores its children SOLELY as quantifiers, with no separate
// plan-snapshot field. WithQuantifiers must therefore re-point the children
// entirely — GetChildren re-resolves through the new quantifiers. The old
// wrapper bug kept `plan: w.plan` and swapped only quantifiers, so GetChildren
// returned the STALE snapshot while the memo cost the new quantifier tree (the
// 472 semantically-divergent edges). If that dual storage ever returns, this
// test catches it.
func TestFlatMapPlan_WithQuantifiers_NoStalePlanSnapshot(t *testing.T) {
	t.Parallel()

	scan := func(name string) *plans.RecordQueryScanPlan {
		return mustPlanTest(t, func() (*plans.RecordQueryScanPlan, error) {
			return plans.NewRecordQueryScanPlan([]string{name}, recursiveTestType(), false)
		})
	}
	q := func(p plans.RecordQueryPlan) expressions.Quantifier {
		return expressions.NewPhysicalQuantifier(expressions.FinalOf(p))
	}

	fm := mustPlanTest(t, func() (*plans.RecordQueryFlatMapPlan, error) {
		return plans.NewRecordQueryFlatMapPlanFromQuantifiers(
			q(scan("OUTER_OLD")), q(scan("INNER_OLD")),
			values.NamedCorrelationIdentifier("o"), values.NamedCorrelationIdentifier("i"),
			&values.ConstantValue{Value: int64(0), Typ: values.NotNullLong}, false,
		)
	})

	swappedExpr, err := fm.WithQuantifiers([]expressions.Quantifier{
		q(scan("OUTER_NEW")), q(scan("INNER_NEW")),
	})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	swapped := swappedExpr.(*plans.RecordQueryFlatMapPlan)

	children := swapped.GetChildren()
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	gotOuter := children[0].(*plans.RecordQueryScanPlan).GetRecordTypes()[0]
	gotInner := children[1].(*plans.RecordQueryScanPlan).GetRecordTypes()[0]
	if gotOuter != "OUTER_NEW" || gotInner != "INNER_NEW" {
		t.Fatalf("WithQuantifiers returned a STALE plan snapshot: GetChildren = [%s, %s], "+
			"want [OUTER_NEW, INNER_NEW] — the memo would cost a different tree than executes",
			gotOuter, gotInner)
	}
}
