package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestSinkLimitIntoVectorScanRule_FiresForMatchingLiteralLimit(t *testing.T) {
	t.Parallel()

	vector := plans.NewRecordQueryVectorIndexPlan(
		"vector_idx",
		nil,
		values.LiteralValue([]float64{1, 2, 3}),
		values.LiteralValue(int64(5)),
		predicates.ComparisonDistanceRankLessThanOrEq,
		nil,
		nil,
		[]string{"T"},
		values.UnknownType,
	).WithOrderedStream()
	limit := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.ForEachQuantifier(expressions.InitialOf(vector)),
		5,
		0,
		nil,
	)

	yielded := FireImplementationRule(
		NewSinkLimitIntoVectorScanRule(),
		expressions.InitialOf(limit),
	)
	if len(yielded) != 1 {
		t.Fatalf("expected one self-limiting vector scan, got %d", len(yielded))
	}
	folded, ok := yielded[0].(*plans.RecordQueryVectorIndexPlan)
	if !ok {
		t.Fatalf("yielded %T, want *plans.RecordQueryVectorIndexPlan", yielded[0])
	}
	if folded.IsOrderedStream() {
		t.Fatal("matching LIMIT was not folded into self-limiting vector mode")
	}
	if folded.GetIndexName() != "vector_idx" {
		t.Fatalf("folded vector index = %q, want vector_idx", folded.GetIndexName())
	}
}
