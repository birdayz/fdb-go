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

	row := values.NewRecordType("VectorRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "embedding", FieldType: values.NewArrayType(false, values.NotNullDouble)},
	})
	vector, err := plans.NewRecordQueryVectorIndexPlan(
		"vector_idx",
		nil,
		&values.ConstantValue{Value: []float64{1, 2, 3}, Typ: values.NewArrayType(false, values.NotNullDouble)},
		&values.ConstantValue{Value: int64(5), Typ: values.NotNullLong},
		predicates.ComparisonDistanceRankLessThanOrEq,
		nil,
		nil,
		[]string{"T"},
		row,
	)
	if err != nil {
		t.Fatalf("NewRecordQueryVectorIndexPlan: %v", err)
	}
	vector = vector.WithOrderedStream()
	limit, err := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.ForEachQuantifier(expressions.InitialOf(vector)),
		5,
		0,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRecordQueryLimitPlanFromQuantifier: %v", err)
	}

	yielded, err := FireImplementationRule(
		NewSinkLimitIntoVectorScanRule(),
		expressions.InitialOf(limit),
	)
	if err != nil {
		t.Fatalf("FireImplementationRule: %v", err)
	}
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
