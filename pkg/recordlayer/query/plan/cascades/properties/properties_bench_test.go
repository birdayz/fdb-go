package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Benchmarks for the properties accessors. Capture baseline ns/op
// numbers for the three property walks: full Cost, Cardinality only,
// Ordering only.
//
// Hypothesis: Cardinality should be the same speed as Cost (it's a
// thin wrapper); Ordering should be faster (no walk, just one
// type-switch + maybe one Reference.Get).

func BenchmarkEstimateCost_FilterOverScan(b *testing.B) {
	scan := mustFullUnorderedScanExpression(b, []string{"T"}, propertyTestFlowedType())
	pred := predicates.NewValuePredicate(propertyField(b, "active", values.TypeBool))
	filter := mustLogicalFilterExpression(b,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateCost(filter)
	}
}

func BenchmarkEstimateCardinality_FilterOverScan(b *testing.B) {
	scan := mustFullUnorderedScanExpression(b, []string{"T"}, propertyTestFlowedType())
	pred := predicates.NewValuePredicate(propertyField(b, "active", values.TypeBool))
	filter := mustLogicalFilterExpression(b,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateCardinality(filter)
	}
}

func BenchmarkEstimateOrdering_SortOverScan(b *testing.B) {
	scan := mustFullUnorderedScanExpression(b, []string{"T"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(b, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(b, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateOrdering(sort)
	}
}

func BenchmarkIsOrdered_FilterOverSort(b *testing.B) {
	scan := mustFullUnorderedScanExpression(b, []string{"T"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(b, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(b, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustLogicalFilterExpression(b,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(sort)))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = IsOrdered(filter)
	}
}
