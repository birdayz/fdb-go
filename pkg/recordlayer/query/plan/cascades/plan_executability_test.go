package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func docsRowType() *values.RecordType {
	return values.NewRecordType("DOCS", false, []values.Field{
		{Name: "ZONE", FieldType: values.NotNullString, Ordinal: 0},
		{Name: "EMBEDDING", FieldType: values.NewArrayType(false, values.NotNullDouble), Ordinal: 1},
	})
}

func docsField(t testing.TB, root values.Value, ordinal int) values.Value {
	t.Helper()
	fieldValue, fieldErr := values.ResolveFieldOrdinals(root, []int{ordinal})
	return mustConstruct(t, fieldValue, fieldErr)
}

func newDistanceRankResidual(t testing.TB, root values.Value) predicates.QueryPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		values.NewEuclideanDistanceRowNumberValue(
			[]values.Value{docsField(t, root, 0)},
			[]values.Value{docsField(t, root, 1)},
		),
		predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(3)),
	)
}

func newScanExpr(t testing.TB) expressions.RelationalExpression {
	t.Helper()
	return mustFullUnorderedScan(t, []string{"DOCS"}, docsRowType())
}

func newScanPlan(t testing.TB) plans.RecordQueryPlan {
	t.Helper()
	planValue, planErr := plans.NewRecordQueryScanPlan([]string{"DOCS"}, docsRowType(), false)
	return mustConstruct(t, planValue, planErr)
}

// TestFindIndexOnlyResidual_NestedUnderUnionArm pins that the PHYSICAL catch-all
// backstop recurses past the root (the "leaks at depth > 0" case): an index-only
// DistanceRank residual nested inside a union arm — not at the root — must still be
// found, so validateNoIndexOnlyResidual rejects a plan that hides the unevaluable
// filter beneath a UNION/INTERSECTION. This is the path the ImplementFilterRule
// gate does NOT cover (a physical filter built by another producer).
func TestFindIndexOnlyResidual_NestedUnderUnionArm(t *testing.T) {
	t.Parallel()

	badInner := newScanPlan(t)
	badFilterValue, badFilterErr := plans.NewRecordQueryPredicatesFilterPlan(
		badInner, []predicates.QueryPredicate{newDistanceRankResidual(t, badInner.GetResultValue())})
	badFilter := mustConstruct(t, badFilterValue, badFilterErr)
	unionValue, unionErr := plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{newScanPlan(t), badFilter})
	union := mustConstruct(t, unionValue, unionErr)

	if got := findIndexOnlyResidual(union); got == nil {
		t.Fatal("did not find the index-only residual nested one level under a union arm")
	}
}

// TestFindIndexOnlyResidual_CleanTree pins the no-false-positive direction for the
// physical backstop.
func TestFindIndexOnlyResidual_CleanTree(t *testing.T) {
	t.Parallel()

	cleanInner := newScanPlan(t)
	cleanFilterValue, cleanFilterErr := plans.NewRecordQueryPredicatesFilterPlan(
		cleanInner,
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				docsField(t, cleanInner.GetResultValue(), 0),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "z1"),
			),
		})
	cleanFilter := mustConstruct(t, cleanFilterValue, cleanFilterErr)
	unionValue, unionErr := plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{newScanPlan(t), cleanFilter})
	union := mustConstruct(t, unionValue, unionErr)

	if got := findIndexOnlyResidual(union); got != nil {
		t.Fatalf("false positive on a clean tree: %v", got.Explain())
	}
}

// TestFindIndexOnlyLogicalResidual_NestedUnderQuantifier pins that the logical
// walk recurses past the root: an index-only DistanceRank predicate on a
// LogicalFilter nested one quantifier below the root reference must still be
// found, so the planner surfaces the clean UnplannableIndexOnlyResidualError
// when the Java !isIndexOnly() gate leaves such a filter unrealized.
func TestFindIndexOnlyLogicalResidual_NestedUnderQuantifier(t *testing.T) {
	t.Parallel()

	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(newScanExpr(t)))
	scanRootValue, scanRootErr := scanQ.RequireFlowedObjectValue()
	scanRoot := mustConstruct(t, scanRootValue, scanRootErr)
	badFilterValue, badFilterErr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{newDistanceRankResidual(t, scanRoot)}, scanQ)
	badFilter := mustConstruct(t, badFilterValue, badFilterErr)
	// Wrap the filter under a projection so the index-only filter sits at depth > 0.
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(badFilter))
	projValue, projErr := expressions.NewLogicalProjectionExpression(nil, projQ)
	proj := mustConstruct(t, projValue, projErr)
	root := expressions.InitialOf(proj)

	got := findIndexOnlyLogicalResidual(root)
	if got == nil {
		t.Fatal("did not find the index-only DistanceRank residual nested under the projection")
	}
}

// TestFindIndexOnlyLogicalResidual_CleanTree pins the no-false-positive
// direction: a logical tree whose filters carry only ordinary predicates
// returns nil, so the planner never spuriously reports an unplannable query.
func TestFindIndexOnlyLogicalResidual_CleanTree(t *testing.T) {
	t.Parallel()

	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(newScanExpr(t)))
	scanRootValue, scanRootErr := scanQ.RequireFlowedObjectValue()
	scanRoot := mustConstruct(t, scanRootValue, scanRootErr)
	cleanFilterValue, cleanFilterErr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				docsField(t, scanRoot, 0),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "z1"),
			),
		}, scanQ)
	cleanFilter := mustConstruct(t, cleanFilterValue, cleanFilterErr)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(cleanFilter))
	projValue, projErr := expressions.NewLogicalProjectionExpression(nil, projQ)
	proj := mustConstruct(t, projValue, projErr)
	root := expressions.InitialOf(proj)

	if got := findIndexOnlyLogicalResidual(root); got != nil {
		t.Fatalf("false positive on a clean tree: %v", got.Explain())
	}
}
