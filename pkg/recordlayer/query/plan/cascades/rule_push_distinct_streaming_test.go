package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestPushDistinctBelowFilter_PreservesStreaming pins the review fold for
// TODO C5: pushing a distinct below a filter must RE-DERIVE the streaming mode
// against the NEW inner (the filter's inner), not reset it via the constructor.
// A filter preserves ordering, so a distinct streaming-eligible over
// Filter(inner) is still streaming-eligible over inner; a rebuild that dropped
// Streaming would seed the memo with the cross-page-buggy hash-set alternative.
// Here the filter's inner is an in-memory sort ordered by G over a RECORD<G>
// row, so the pushed distinct must come back Streaming=true.
func TestPushDistinctBelowFilter_PreservesStreaming(t *testing.T) {
	t.Parallel()

	gRec := values.NewRecordType("", false, []values.Field{
		{Name: "G", FieldType: values.TypeInt, Ordinal: 0},
	})
	scanPlan := plans.NewRecordQueryIndexPlan("idx_g", nil, []string{"T"}, gRec, false)
	scanWrapper := &physicalIndexScanWrapper{plan: scanPlan}
	scanRef := expressions.InitialOf(scanWrapper)

	sortKeys := []plans.SortKey{{
		Field:      "G",
		NullsFirst: true,
		ValueExpr:  values.NewFieldValueWithResolvedOrdinal("G", 0, values.TypeInt),
	}}
	sortPlan := plans.NewRecordQueryInMemorySortPlan(scanPlan, sortKeys)
	sortWrapper := newPhysicalInMemorySortWrapper(sortPlan, expressions.ForEachQuantifier(scanRef))
	sortRef := expressions.InitialOf(sortWrapper)

	pred := predicates.NewComparisonPredicate(
		values.NewFieldValueWithResolvedOrdinal("G", 0, values.TypeInt),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	filterPlan := plans.NewRecordQueryPredicatesFilterPlan(sortPlan, []predicates.QueryPredicate{pred})
	filterWrapper := NewPhysicalPredicatesFilterWrapper(filterPlan, expressions.ForEachQuantifier(sortRef))
	filterRef := expressions.InitialOf(filterWrapper)

	distinctPlan := plans.NewRecordQueryDistinctPlan(filterPlan)
	distinctWrapper := NewPhysicalDistinctWrapper(distinctPlan, expressions.ForEachQuantifier(filterRef))
	ref := expressions.InitialOf(distinctWrapper)

	rule := NewPushDistinctBelowFilterRule()
	yielded := FireImplementationRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded (Filter(Distinct(inner))), got %d", len(yielded))
	}
	fw, ok := yielded[0].(*physicalPredicatesFilterWrapper)
	if !ok {
		t.Fatalf("expected physicalPredicatesFilterWrapper, got %T", yielded[0])
	}
	inner := fw.plan.GetInner()
	dp, ok := inner.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("filter's inner = %T, want *RecordQueryDistinctPlan", inner)
	}
	if !dp.Streaming {
		t.Fatal("pushed distinct must be Streaming=true — the filter's inner is ordered by the " +
			"dedup key G, so a constructor rebuild that dropped Streaming would revert to the " +
			"cross-page-buggy hash-set")
	}
}
