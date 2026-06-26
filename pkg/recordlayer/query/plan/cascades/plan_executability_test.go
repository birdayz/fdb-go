package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func newDistanceRankResidual() predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		values.NewEuclideanDistanceRowNumberValue(
			[]values.Value{&values.FieldValue{Field: "ZONE", Typ: values.TypeString}},
			[]values.Value{&values.FieldValue{Field: "EMBEDDING"}},
		),
		predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(3)),
	)
}

func newScanExpr() expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression([]string{"DOCS"}, values.UnknownType)
}

func newScanPlan() plans.RecordQueryPlan {
	return plans.NewRecordQueryScanPlan([]string{"DOCS"}, values.UnknownType, false)
}

// TestFindIndexOnlyResidual_NestedUnderUnionArm pins that the PHYSICAL catch-all
// backstop recurses past the root (Graefe: "leaks at depth > 0"): an index-only
// DistanceRank residual nested inside a union arm — not at the root — must still be
// found, so validateNoIndexOnlyResidual rejects a plan that hides the unevaluable
// filter beneath a UNION/INTERSECTION. This is the path the ImplementFilterRule
// gate does NOT cover (a physical filter built by another producer).
func TestFindIndexOnlyResidual_NestedUnderUnionArm(t *testing.T) {
	t.Parallel()

	badFilter := plans.NewRecordQueryPredicatesFilterPlan(
		newScanPlan(), []predicates.QueryPredicate{newDistanceRankResidual()})
	union := plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{newScanPlan(), badFilter})

	if got := findIndexOnlyResidual(union); got == nil {
		t.Fatal("did not find the index-only residual nested one level under a union arm")
	}
}

// TestFindIndexOnlyResidual_CleanTree pins the no-false-positive direction for the
// physical backstop.
func TestFindIndexOnlyResidual_CleanTree(t *testing.T) {
	t.Parallel()

	cleanFilter := plans.NewRecordQueryPredicatesFilterPlan(
		newScanPlan(),
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "ZONE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "z1"),
			),
		})
	union := plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{newScanPlan(), cleanFilter})

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

	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(newScanExpr()))
	badFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{newDistanceRankResidual()}, scanQ)
	// Wrap the filter under a projection so the index-only filter sits at depth > 0.
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(badFilter))
	proj := expressions.NewLogicalProjectionExpression(nil, projQ)
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

	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(newScanExpr()))
	cleanFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "ZONE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "z1"),
			),
		}, scanQ)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(cleanFilter))
	proj := expressions.NewLogicalProjectionExpression(nil, projQ)
	root := expressions.InitialOf(proj)

	if got := findIndexOnlyLogicalResidual(root); got != nil {
		t.Fatalf("false positive on a clean tree: %v", got.Explain())
	}
}
