package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPushDistinctConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct push-distinct fixture: " + err.Error())
	}
	return value
}

func pushDistinctRowType() *values.RecordType {
	return values.NewRecordType("PushDistinctRow", false, []values.Field{
		{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
	})
}

func pushDistinctScanAndField() (*plans.RecordQueryIndexPlan, values.Value) {
	row := pushDistinctRowType()
	scan := mustPushDistinctConstruct(plans.NewRecordQueryIndexPlan(
		"idx_g", nil, []string{"T"}, row, false))
	qov, ok := values.AsQuantifiedObjectValue(scan.GetResultValue())
	if !ok {
		panic("push-distinct scan result is not a QOV")
	}
	return scan, mustPushDistinctConstruct(values.ResolveFieldOrdinals(qov, []int{0}))
}

func pushDistinctWithQuantifiers(
	t testing.TB, expression expressions.RelationalExpression, quantifiers []expressions.Quantifier,
) expressions.RelationalExpression {
	t.Helper()
	result, err := expression.WithQuantifiers(quantifiers)
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	return result
}

func firePushDistinctRule(
	t testing.TB, rule ImplementationRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireImplementationRule(rule, ref)
	if err != nil {
		t.Fatalf("FireImplementationRule: %v", err)
	}
	return result
}

// TestPushDistinctBelowFilter_PreservesStreaming pins the review fold for
// TODO C5: pushing a distinct below a filter must RE-DERIVE the streaming mode
// against the NEW inner (the filter's inner), not reset it via the constructor.
// A filter preserves ordering, so a distinct streaming-eligible over
// Filter(inner) is still streaming-eligible over inner; a rebuild that dropped
// Streaming would seed the memo with the memory-heavy hash-set alternative.
// Here the filter's inner is an in-memory sort ordered by G over a RECORD<G>
// row, so the pushed distinct must come back Streaming=true.
func TestPushDistinctBelowFilter_PreservesStreaming(t *testing.T) {
	t.Parallel()

	scanPlan, gField := pushDistinctScanAndField()

	sortKeys := []plans.SortKey{{
		Field:      "G",
		NullsFirst: true,
		ValueExpr:  gField,
	}}
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryInMemorySortPlan
	// (no physicalInMemorySortWrapper); it is its own physical member directly.
	sortPlan := mustPushDistinctConstruct(plans.NewRecordQueryInMemorySortPlan(scanPlan, sortKeys))
	sortRef := expressions.InitialOf(sortPlan)

	pred := predicates.NewComparisonPredicate(
		gField,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	filterPlan := mustPushDistinctConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		sortPlan, []predicates.QueryPredicate{pred}))
	filterWrapper := pushDistinctWithQuantifiers(
		t, filterPlan, []expressions.Quantifier{expressions.ForEachQuantifier(sortRef)})
	filterRef := expressions.InitialOf(filterWrapper)

	distinctPlan := mustPushDistinctConstruct(plans.NewRecordQueryDistinctPlan(filterPlan))
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryDistinctPlan (no
	// physicalDistinctWrapper); the push rule matches it directly.
	distinctWrapper := pushDistinctWithQuantifiers(
		t, distinctPlan, []expressions.Quantifier{expressions.ForEachQuantifier(filterRef)})
	ref := expressions.InitialOf(distinctWrapper)

	rule := NewPushDistinctBelowFilterRule()
	yielded := firePushDistinctRule(t, rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded (Filter(Distinct(inner))), got %d", len(yielded))
	}
	fw, ok := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryPredicatesFilterPlan, got %T", yielded[0])
	}
	inner := fw.GetInner()
	dp, ok := inner.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("filter's inner = %T, want *RecordQueryDistinctPlan", inner)
	}
	if !dp.Streaming {
		t.Fatal("pushed distinct must be Streaming=true — the filter's inner is ordered by the " +
			"dedup key G, so a constructor rebuild that dropped Streaming would revert to the " +
			"memory-heavy hash-set")
	}
}

// TestPushDistinctThroughFetch_PreservesStreaming is the symmetric pin for the
// second rebuild site: pushing a distinct through a fetch (Distinct(Fetch(inner))
// → Fetch(Distinct(inner))) must likewise re-derive the streaming mode against
// the fetch's inner. A fetch preserves ordering, so a distinct over an ordered
// inner stays streaming-eligible; the constructor rebuild would otherwise drop
// it to the memory-heavy hash-set (TODO C5).
func TestPushDistinctThroughFetch_PreservesStreaming(t *testing.T) {
	t.Parallel()

	scanPlan, gField := pushDistinctScanAndField()

	sortKeys := []plans.SortKey{{
		Field:      "G",
		NullsFirst: true,
		ValueExpr:  gField,
	}}
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryInMemorySortPlan
	// (no physicalInMemorySortWrapper); it is its own physical member directly.
	sortPlan := mustPushDistinctConstruct(plans.NewRecordQueryInMemorySortPlan(scanPlan, sortKeys))
	sortRef := expressions.InitialOf(sortPlan)

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := mustPushDistinctConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		sortPlan, translateFn, pushDistinctRowType(), plans.FetchIndexRecordsPrimaryKey,
	))
	fetchWrapper := pushDistinctWithQuantifiers(
		t, fetchPlan, []expressions.Quantifier{expressions.ForEachQuantifier(sortRef)})
	fetchRef := expressions.InitialOf(fetchWrapper)

	distinctPlan := mustPushDistinctConstruct(plans.NewRecordQueryDistinctPlan(fetchPlan))
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryDistinctPlan (no
	// physicalDistinctWrapper); the push rule matches it directly.
	distinctWrapper := pushDistinctWithQuantifiers(
		t, distinctPlan, []expressions.Quantifier{expressions.ForEachQuantifier(fetchRef)})
	ref := expressions.InitialOf(distinctWrapper)

	rule := NewPushDistinctThroughFetchRule()
	yielded := firePushDistinctRule(t, rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded (Fetch(Distinct(inner))), got %d", len(yielded))
	}
	fw, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryFetchFromPartialRecordPlan, got %T", yielded[0])
	}
	inner := fw.GetInner()
	dp, ok := inner.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("fetch's inner = %T, want *RecordQueryDistinctPlan", inner)
	}
	if !dp.Streaming {
		t.Fatal("pushed distinct must be Streaming=true — the fetch's inner is ordered by the " +
			"dedup key G, so a constructor rebuild that dropped Streaming would revert to the " +
			"memory-heavy hash-set")
	}
}
