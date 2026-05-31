package cascades

import (
	"testing"

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

func newScan() plans.RecordQueryPlan {
	return plans.NewRecordQueryScanPlan([]string{"DOCS"}, values.UnknownType, false)
}

// TestFindIndexOnlyResidual_NestedUnderUnionArm pins that findIndexOnlyResidual
// recurses past the root (Graefe: "leaks at depth > 0"): an index-only
// DistanceRank residual nested inside a union arm — not at the root — must still
// be found, so validateNoIndexOnlyResidual rejects a plan that hides the
// unevaluable filter beneath a UNION/INTERSECTION rather than at the top.
func TestFindIndexOnlyResidual_NestedUnderUnionArm(t *testing.T) {
	t.Parallel()

	badFilter := plans.NewRecordQueryPredicatesFilterPlan(
		newScan(), []predicates.QueryPredicate{newDistanceRankResidual()})
	union := plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{newScan(), badFilter})

	got := findIndexOnlyResidual(union)
	if got == nil {
		t.Fatal("did not find the index-only residual nested one level under a union arm")
	}
}

// TestFindIndexOnlyResidual_CleanTree pins the no-false-positive direction: a
// nested tree whose filters carry only ordinary predicates returns nil, so the
// guard never rejects a legitimate plan.
func TestFindIndexOnlyResidual_CleanTree(t *testing.T) {
	t.Parallel()

	cleanFilter := plans.NewRecordQueryPredicatesFilterPlan(
		newScan(),
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "ZONE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "z1"),
			),
		},
	)
	union := plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{newScan(), cleanFilter})

	if got := findIndexOnlyResidual(union); got != nil {
		t.Fatalf("false positive on a clean tree: %v", got.Explain())
	}
}
