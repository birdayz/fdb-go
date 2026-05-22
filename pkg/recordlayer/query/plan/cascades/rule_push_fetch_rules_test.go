package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestMergeFetchIntoCoveringIndex_FiresOnFetchOverIndex(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_name", nil, []string{"MyRecord"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan, covering: true}
	indexRef := expressions.InitialOf(indexWrapper)

	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	if _, ok := yielded[0].(*physicalIndexScanWrapper); !ok {
		t.Fatalf("expected physicalIndexScanWrapper, got %T", yielded[0])
	}
}

func TestMergeFetchIntoCoveringIndex_DoesNotFireOnNonCoveringIndex(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_name", nil, []string{"MyRecord"}, values.UnknownType, false,
	)
	// NOT marked as covering — MergeFetch should NOT fire.
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan, covering: false}
	indexRef := expressions.InitialOf(indexWrapper)

	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded (non-covering index), got %d", len(yielded))
	}
}

func TestMergeFetchIntoCoveringIndex_DoesNotFireOnNonIndex(t *testing.T) {
	t.Parallel()

	// Fetch over a filter (not an index scan) — should not fire.
	filterPlan := plans.NewRecordQueryFilterPlan(nil, nil)
	filterWrapper := NewPhysicalFilterWrapper(filterPlan, expressions.ForEachQuantifier(
		&expressions.Reference{},
	))
	filterRef := expressions.InitialOf(filterWrapper)

	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		nil, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(filterRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded, got %d", len(yielded))
	}
}

func TestPushDistinctThroughFetch_Fires(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	distinctPlan := plans.NewRecordQueryDistinctPlan(nil)
	distinctQ := expressions.ForEachQuantifier(fetchRef)
	distinctWrapper := NewPhysicalDistinctWrapper(distinctPlan, distinctQ)

	ref := expressions.InitialOf(distinctWrapper)

	rule := NewPushDistinctThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Fetch(Distinct(index))
	if !IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected physicalFetchFromPartialRecordWrapper, got %T", yielded[0])
	}
}

func TestPushFilterThroughFetch_AllPushable(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		// Always translatable — return the value rebound to target.
		return v, true
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Filter with one pushable predicate.
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred})
	filterQ := expressions.ForEachQuantifier(fetchRef)
	filterWrapper := NewPhysicalPredicatesFilterWrapper(filterPlan, filterQ)

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Fetch(Filter(index))
	if !IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected physicalFetchFromPartialRecordWrapper, got %T", yielded[0])
	}
}

func TestPushFilterThroughFetch_NoPushable(t *testing.T) {
	t.Parallel()

	filterInnerAlias := values.UniqueCorrelationIdentifier()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	// TranslateValueFunction that NEVER succeeds.
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, plans.UnableToTranslate, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Predicate correlated to the filter's alias — requires
	// translation but UnableToTranslate always fails.
	pred := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(filterInnerAlias),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred})
	filterQ := expressions.NamedForEachQuantifier(filterInnerAlias, fetchRef)
	filterWrapper := NewPhysicalPredicatesFilterWrapper(filterPlan, filterQ)

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded (nothing pushable), got %d", len(yielded))
	}
}

func TestPushFilterThroughFetch_PartialPush(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	// The filter's inner quantifier alias — predicates inside the
	// filter reference this alias. We create it with a known alias
	// so we can set up predicates correlated to it.
	filterInnerAlias := values.UniqueCorrelationIdentifier()

	// TranslateValueFunction: translates QuantifiedObjectValue
	// correlated to the source alias, but only the first call
	// succeeds (simulating an index that covers field "x" but not "y").
	callCount := 0
	translateFn := func(v values.Value, sourceAlias, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		callCount++
		if callCount == 1 {
			return values.NewQuantifiedObjectValue(targetAlias), true
		}
		return nil, false
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Both predicates are correlated to the filter's inner alias
	// (simulating real predicates that reference the flowing row).
	pushablePred := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(filterInnerAlias),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	residualPred := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(filterInnerAlias),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
	)
	filterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pushablePred, residualPred})
	filterQ := expressions.NamedForEachQuantifier(filterInnerAlias, fetchRef)
	filterWrapper := NewPhysicalPredicatesFilterWrapper(filterPlan, filterQ)

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Filter(residual, Fetch(Filter(pushed, index)))
	if IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected residual filter wrapper on top, got fetch wrapper directly")
	}
	if !IsPhysicalPredicatesFilter(yielded[0]) {
		t.Fatalf("expected physicalPredicatesFilterWrapper, got %T", yielded[0])
	}
}

func TestPushMapThroughFetch_Fires(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	resultVal := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	mapPlan := plans.NewRecordQueryMapPlan(nil, resultVal)
	mapQ := expressions.ForEachQuantifier(fetchRef)
	mapWrapper := NewPhysicalMapWrapper(mapPlan, mapQ)

	ref := expressions.InitialOf(mapWrapper)

	rule := NewPushMapThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Map(translated, index) — no fetch.
	if IsPhysicalMap(yielded[0]) {
		return // good
	}
	t.Fatalf("expected physicalMapWrapper, got %T", yielded[0])
}

func TestPushMapThroughFetch_DoesNotFire_WhenTranslationFails(t *testing.T) {
	t.Parallel()

	mapAlias := values.UniqueCorrelationIdentifier()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	// UnableToTranslate — map can't be pushed.
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, plans.UnableToTranslate, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Use a correlated FieldValue so translation is actually attempted.
	resultVal := values.NewFieldValue(values.NewQuantifiedObjectValue(mapAlias), "x", values.TypeInt)
	mapPlan := plans.NewRecordQueryMapPlan(nil, resultVal)
	mapQ := expressions.NamedForEachQuantifier(mapAlias, fetchRef)
	mapWrapper := NewPhysicalMapWrapper(mapPlan, mapQ)

	ref := expressions.InitialOf(mapWrapper)

	rule := NewPushMapThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded, got %d", len(yielded))
	}
}

func TestPushUnionThroughFetch_AllChildrenHaveFetches(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}

	// Build two fetch-over-index children.
	makeChild := func(indexName string) expressions.Quantifier {
		indexPlan := plans.NewRecordQueryIndexPlan(
			indexName, nil, []string{"T"}, values.UnknownType, false,
		)
		indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
		indexRef := expressions.InitialOf(indexWrapper)

		fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
		)
		fetchQ := expressions.ForEachQuantifier(indexRef)
		fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
		fetchRef := expressions.InitialOf(fetchWrapper)
		return expressions.ForEachQuantifier(fetchRef)
	}

	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	unionPlan := plans.NewRecordQueryUnionPlan(nil)
	unionWrapper := NewPhysicalUnionWrapper(unionPlan, []expressions.Quantifier{q1, q2})

	ref := expressions.InitialOf(unionWrapper)

	rule := NewPushUnionThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Fetch(Union(idx_a, idx_b))
	if !IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected physicalFetchFromPartialRecordWrapper, got %T", yielded[0])
	}
}

func TestPushUnionThroughFetch_DoesNotFire_OnlyOneChildHasFetch(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}

	// First child: fetch over index.
	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)
	q1 := expressions.ForEachQuantifier(fetchRef)

	// Second child: plain scan (no fetch).
	scanPlan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanWrapper := &physicalScanWrapper{plan: scanPlan}
	scanRef := expressions.InitialOf(scanWrapper)
	q2 := expressions.ForEachQuantifier(scanRef)

	unionPlan := plans.NewRecordQueryUnionPlan(nil)
	unionWrapper := NewPhysicalUnionWrapper(unionPlan, []expressions.Quantifier{q1, q2})

	ref := expressions.InitialOf(unionWrapper)

	rule := NewPushUnionThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded (not all children are fetches), got %d", len(yielded))
	}
}

// TestFieldValueChild_CorrelationTracking verifies that a FieldValue
// with a child (QuantifiedObjectValue) properly participates in
// correlation tracking — GetCorrelatedToOfValue discovers the child's
// quantifier alias.
func TestFieldValueChild_CorrelationTracking(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	child := values.NewQuantifiedObjectValue(alias)
	fv := values.NewFieldValue(child, "name", values.TypeString)

	correlated := values.GetCorrelatedToOfValue(fv)
	if _, ok := correlated[alias]; !ok {
		t.Fatalf("FieldValue with child should be correlated to alias %v", alias)
	}
}

// TestFieldValueChild_PushFilterDecision verifies end-to-end: a
// predicate with FieldValue(child=QOV(alias)) is correctly identified
// as correlated to the filter's inner alias, enabling proper push/
// residual classification in PushFilterThroughFetchRule.
func TestFieldValueChild_PushFilterDecision(t *testing.T) {
	t.Parallel()

	filterAlias := values.UniqueCorrelationIdentifier()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := &physicalIndexScanWrapper{plan: indexPlan}
	indexRef := expressions.InitialOf(indexWrapper)

	// TranslateValueFunction that succeeds for FieldValues with field "x"
	// but fails for field "y". Simulates an index covering column "x"
	// but not "y".
	translateFn := func(v values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		if fv, ok := v.(*values.FieldValue); ok {
			if fv.Field == "x" {
				return values.NewFieldValue(
					values.NewQuantifiedObjectValue(targetAlias), "x", values.TypeInt,
				), true
			}
		}
		return nil, false
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Predicates using FieldValue WITH child — correlated to filterAlias.
	pushablePred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(filterAlias), "x", values.TypeInt),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	residualPred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(filterAlias), "y", values.TypeInt),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
	)

	filterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pushablePred, residualPred})
	filterQ := expressions.NamedForEachQuantifier(filterAlias, fetchRef)
	filterWrapper := NewPhysicalPredicatesFilterWrapper(filterPlan, filterQ)

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	// Should yield 1: Filter(y, Fetch(Filter(x, index)))
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded (partial push), got %d", len(yielded))
	}
	// Top should be a residual filter (not a fetch directly).
	if !IsPhysicalPredicatesFilter(yielded[0]) {
		t.Fatalf("expected residual filter on top, got %T", yielded[0])
	}
}
