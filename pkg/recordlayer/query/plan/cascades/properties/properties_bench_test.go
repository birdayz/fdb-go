package properties_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Benchmarks for the properties accessors. Capture baseline ns/op
// numbers for the three property walks: full Cost, Cardinality only,
// Ordering only.
//
// Hypothesis: Cardinality should be the same speed as Cost (it's a
// thin wrapper); Ordering should be faster (no walk, just one
// type-switch + maybe one Reference.Get).

func BenchmarkEstimateCost_FilterOverScan(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = properties.EstimateCost(filter)
	}
}

func BenchmarkEstimateCardinality_FilterOverScan(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = properties.EstimateCardinality(filter)
	}
}

func BenchmarkEstimateOrdering_SortOverScan(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = properties.EstimateOrdering(sort)
	}
}

func BenchmarkIsOrdered_FilterOverSort(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(sort)),
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = properties.IsOrdered(filter)
	}
}
