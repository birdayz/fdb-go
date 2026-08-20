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
	if !dp.IsStreaming() {
		t.Fatal("pushed distinct must be Streaming=true — the filter's inner is ordered by the " +
			"dedup key G, so a constructor rebuild that dropped Streaming would revert to the " +
			"memory-heavy hash-set")
	}
}

// TestPushDistinctThroughFetch_DeclinesStreamingRowDistinct is what became of
// this file's second rebuild site.
//
// It used to assert that pushing a full-row distinct through a fetch re-derived
// the streaming mode against the fetch's inner. That push is unsound and the
// rule no longer performs it: below the fetch the rows are PARTIAL records, and
// a full-row dedup cannot collapse the two partial rows one record produces
// when the inner is a union of covering scans over different indexes. So the
// streaming mode is no longer a question this rule can be asked — the node that
// carries the flag is not the node it matches.
//
// The fixture is kept, and the assertion inverted, because the shape it builds
// (a distinct over a fetch over an ORDERED inner) is the one that made the old
// push look attractive: it is exactly where a reader would expect the rule to
// fire. The streaming re-derivation itself remains covered by the filter push
// above, which does still handle the full-row node.
func TestPushDistinctThroughFetch_DeclinesStreamingRowDistinct(t *testing.T) {
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
	if len(yielded) != 0 {
		t.Fatalf("a full-row distinct was pushed below a fetch (%d plan(s) yielded). Ordered inner or "+
			"not, the dedup key is the whole ROW, and below the fetch the rows are partial records: "+
			"two partial rows for one record differ whenever they come from different covering "+
			"indexes, so the dedup collapses nothing and the record is fetched once per index.",
			len(yielded))
	}
}
