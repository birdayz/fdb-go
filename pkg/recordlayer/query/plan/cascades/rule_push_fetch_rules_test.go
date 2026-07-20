package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestMergeFetchIntoCoveringIndex_FiresOnFetchOverIndex(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_name", nil, []string{"MyRecord"}, values.UnknownType, false,
	)
	indexWrapper := indexPlan.WithCovering(nil)
	indexRef := expressions.InitialOf(indexWrapper)

	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	if _, ok := yielded[0].(*plans.RecordQueryIndexPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryIndexPlan, got %T", yielded[0])
	}
}

func TestMergeFetchIntoCoveringIndex_DoesNotFireOnNonCoveringIndex(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_name", nil, []string{"MyRecord"}, values.UnknownType, false,
	)
	// NOT marked as covering — MergeFetch should NOT fire.
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})

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
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})

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
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
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
		t.Fatalf("expected *plans.RecordQueryFetchFromPartialRecordPlan, got %T", yielded[0])
	}
}

func TestPushFilterThroughFetch_AllPushable(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		// Always translatable — return the value rebound to target.
		return v, true
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
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
		t.Fatalf("expected *plans.RecordQueryFetchFromPartialRecordPlan, got %T", yielded[0])
	}
}

func TestPushFilterThroughFetch_NoPushable(t *testing.T) {
	t.Parallel()

	filterInnerAlias := values.UniqueCorrelationIdentifier()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	// TranslateValueFunction that NEVER succeeds.
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, plans.UnableToTranslate, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
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

// TestTryTranslateValue_FinalCorrelationFilter pins F38: pushValueThroughFetch
// must apply Java's FINAL filter — a translated value still correlated to the
// source alias is NOT a successful push and must be declined (nil), even when the
// translate function reported ok=true. Go previously relied on nil-propagation
// from untranslatable leaves alone, which misses a leaf that "translates"
// (ok=true) yet leaves a residual source correlation (Java's
// "value.withChildren(mappedChildren) // this may be correlated to sourceAlias"
// arm, then translatedValueOptional.filter(v -> !v.getCorrelatedTo().contains(
// sourceAlias)) — ScanWithFetchMatchCandidate.java:71-75).
//
// Revert-proof: tryTranslateValueRec (the raw mapMaybe core) returns the
// still-correlated value; the tryTranslateValue wrapper rejects it. Collapse the
// wrapper into the core (drop the final filter) and the residual-correlated value
// leaks out — a nested read off a flat partial record, failing plan validation.
func TestTryTranslateValue_FinalCorrelationFilter(t *testing.T) {
	t.Parallel()

	src := values.UniqueCorrelationIdentifier()
	tgt := values.UniqueCorrelationIdentifier()

	indexPlan := plans.NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, values.UnknownType, false)

	// "Succeeds" (ok=true) but ECHOES the value back unchanged — the
	// unconditional-rebase escape: claims translation yet the result is still
	// QOV(src), correlated to the source alias.
	echoFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	echoFetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, echoFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)

	srcQOV := values.NewQuantifiedObjectValue(src)

	// Precondition: the raw recursive core returns the still-correlated value.
	raw := tryTranslateValueRec(echoFetch, src, tgt, srcQOV)
	if raw == nil {
		t.Fatal("precondition: tryTranslateValueRec should return the echoed value, got nil")
	}
	if _, corr := values.GetCorrelatedToOfValue(raw)[src]; !corr {
		t.Fatal("precondition: the echoed value must still be correlated to src")
	}

	// The wrapper applies Java's final filter and DECLINES it.
	if got := tryTranslateValue(echoFetch, src, tgt, srcQOV); got != nil {
		t.Fatalf("residual source-correlated translation must be declined (nil), got %v", got)
	}

	// Positive control (guard against over-rejection): a proper rebase
	// QOV(src) -> QOV(tgt) must be accepted, correlated to tgt not src.
	rebaseFn := func(_ values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		return values.NewQuantifiedObjectValue(targetAlias), true
	}
	rebaseFetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, rebaseFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	got := tryTranslateValue(rebaseFetch, src, tgt, srcQOV)
	if got == nil {
		t.Fatal("a clean rebase to the target alias must be accepted, got nil")
	}
	if _, corrSrc := values.GetCorrelatedToOfValue(got)[src]; corrSrc {
		t.Fatalf("accepted translation must not be correlated to src, got %v", got)
	}
	if _, corrTgt := values.GetCorrelatedToOfValue(got)[tgt]; !corrTgt {
		t.Fatalf("accepted translation must be correlated to tgt, got %v", got)
	}
}

func TestPushFilterThroughFetch_PartialPush(t *testing.T) {
	t.Parallel()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := indexPlan
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
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
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
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	resultVal := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	mapPlan := plans.NewRecordQueryMapPlan(nil, resultVal)
	mapQ := expressions.ForEachQuantifier(fetchRef)
	mapWrapper := mapPlan.WithQuantifiers([]expressions.Quantifier{mapQ})

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
	t.Fatalf("expected *plans.RecordQueryMapPlan, got %T", yielded[0])
}

func TestPushMapThroughFetch_DoesNotFire_WhenTranslationFails(t *testing.T) {
	t.Parallel()

	mapAlias := values.UniqueCorrelationIdentifier()

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	// UnableToTranslate — map can't be pushed.
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, plans.UnableToTranslate, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Use a correlated FieldValue so translation is actually attempted.
	resultVal := values.NewFieldValue(values.NewQuantifiedObjectValue(mapAlias), "x", values.TypeInt)
	mapPlan := plans.NewRecordQueryMapPlan(nil, resultVal)
	mapQ := expressions.NamedForEachQuantifier(mapAlias, fetchRef)
	mapWrapper := mapPlan.WithQuantifiers([]expressions.Quantifier{mapQ})

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
		indexWrapper := indexPlan
		indexRef := expressions.InitialOf(indexWrapper)

		fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
		)
		fetchQ := expressions.ForEachQuantifier(indexRef)
		fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
		fetchRef := expressions.InitialOf(fetchWrapper)
		return expressions.ForEachQuantifier(fetchRef)
	}

	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	// The union is its own cascades expression (RFC-184 W2).
	unionExpr := plans.NewRecordQueryUnionPlanFromQuantifiers([]expressions.Quantifier{q1, q2})

	ref := expressions.InitialOf(unionExpr)

	rule := NewPushUnionThroughFetchRule()
	yielded := FireImplementationRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Fetch(Union(idx_a, idx_b))
	if !IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected *plans.RecordQueryFetchFromPartialRecordPlan, got %T", yielded[0])
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
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)
	q1 := expressions.ForEachQuantifier(fetchRef)

	// Second child: plain scan (no fetch).
	scanPlan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanWrapper := &physicalScanWrapper{plan: scanPlan}
	scanRef := expressions.InitialOf(scanWrapper)
	q2 := expressions.ForEachQuantifier(scanRef)

	// The union is its own cascades expression (RFC-184 W2).
	unionExpr := plans.NewRecordQueryUnionPlanFromQuantifiers([]expressions.Quantifier{q1, q2})

	ref := expressions.InitialOf(unionExpr)

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
	indexWrapper := indexPlan
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
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
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

// TestPushUnionThroughFetch_RebuildsPlanOverPushedInners pins the Java
// withChildrenReferences shape (PushSetOperationThroughFetchRule.java:236-240):
// the yielded fetch's PLAN must be Fetch(Union(idxA, idxB)) — the union
// rebuilt over the pushed INNER plans. The pre-fix rule passed the STALE
// union plan (children still the fetch plans) into the wrapper and a
// nil-inner fetch shell above it, so extraction executed
// Fetch(Union(Fetch(idxA), Fetch(idxB))): every row fetched twice while
// the cost model priced the pushed-down shape.
func TestPushUnionThroughFetch_RebuildsPlanOverPushedInners(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	var indexPlans []*plans.RecordQueryIndexPlan
	makeChild := func(indexName string) expressions.Quantifier {
		indexPlan := plans.NewRecordQueryIndexPlan(
			indexName, nil, []string{"T"}, values.UnknownType, false,
		)
		indexPlans = append(indexPlans, indexPlan)
		indexWrapper := indexPlan
		fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
		)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}
	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	// The union is its own cascades expression (RFC-184 W2) — there is no
	// separate plan-snapshot to go stale; the pushed union is built fresh from
	// the live pushed-down leg quantifiers.
	unionExpr := plans.NewRecordQueryUnionPlanFromQuantifiers([]expressions.Quantifier{q1, q2})

	yielded := FireImplementationRule(NewPushUnionThroughFetchRule(), expressions.InitialOf(unionExpr))
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	fw, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected fetch wrapper, got %T", yielded[0])
	}
	up, ok := fw.GetInner().(*plans.RecordQueryUnionPlan)
	if !ok {
		t.Fatalf("fetch inner PLAN: got %T, want *RecordQueryUnionPlan (the rebuilt set-op, not a nil shell)", fw.GetInner())
	}
	inners := up.GetInners()
	if len(inners) != 2 {
		t.Fatalf("rebuilt union arity: got %d, want 2", len(inners))
	}
	for i, inner := range inners {
		if inner != plans.RecordQueryPlan(indexPlans[i]) {
			t.Fatalf("union child %d: got %T (%v), want the pushed INDEX plan — a Fetch child here is the double-fetch bug", i, inner, inner.Explain())
		}
	}
	// The union below the fetch must itself be the rebuilt bare plan carrying
	// the pushed index legs — the double-fetch bug would leave Fetch children.
	setOpExpr := findPhysicalExpr(fw.GetInnerQuantifier().GetRangesOver())
	uw, ok := setOpExpr.(*plans.RecordQueryUnionPlan)
	if !ok {
		t.Fatalf("fetch child expr: got %T, want *plans.RecordQueryUnionPlan", setOpExpr)
	}
	for i, inner := range uw.GetInners() {
		if inner != plans.RecordQueryPlan(indexPlans[i]) {
			t.Fatalf("pushed union child %d: got %T, want the pushed INDEX plan (a Fetch child is the double-fetch bug)", i, inner)
		}
	}
}

// TestPushInUnionThroughFetch_Fires pins that the DYNAMIC set-op arm
// fires with its single leg (Java RecordQueryInUnionPlan.isDynamic()).
// The pre-fix helper declined every call with len(quants) < 2, so the
// InUnion rule was registered but could never fire.
func TestPushInUnionThroughFetch_Fires(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", nil, []string{"T"}, values.UnknownType, false,
	)
	indexWrapper := indexPlan
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
	fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
	q := expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))

	inUnionPlan := plans.NewRecordQueryInUnionPlanWithMaxSize(
		nil, []string{"__in_b"}, nil, false, 7,
	)
	w := NewPhysicalInUnionWrapper(inUnionPlan, q)

	yielded := FireImplementationRule(NewPushInUnionThroughFetchRule(), expressions.InitialOf(w))
	if len(yielded) != 1 {
		t.Fatalf("expected the dynamic single-leg push to fire once, got %d yields", len(yielded))
	}
	fw, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected fetch wrapper, got %T", yielded[0])
	}
	ip, ok := fw.GetInner().(*plans.RecordQueryInUnionPlan)
	if !ok {
		t.Fatalf("fetch inner: got %T, want *RecordQueryInUnionPlan", fw.GetInner())
	}
	if ip.GetInner() != plans.RecordQueryPlan(indexPlan) {
		t.Fatalf("in-union child: got %T, want the pushed index plan", ip.GetInner())
	}
	if got := ip.GetBindingNames(); len(got) != 1 || got[0] != "__in_b" {
		t.Fatalf("binding names not preserved: %v", got)
	}
	if ip.GetMaxSize() != 7 {
		t.Fatalf("maxSize not preserved: %d", ip.GetMaxSize())
	}
}

// TestPushUnionThroughFetch_PartialPush pins Java's Case 2
// (PushSetOperationThroughFetchRule.java:242-251): with two fetch legs
// and one residual scan leg, the fetches merge below ONE fetch and the
// outer union survives over [Fetch(Union(idxA, idxB)), scan]. The
// pre-fix rule declined unless EVERY leg was a fetch.
func TestPushUnionThroughFetch_PartialPush(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	var indexPlans []*plans.RecordQueryIndexPlan
	makeChild := func(indexName string) expressions.Quantifier {
		indexPlan := plans.NewRecordQueryIndexPlan(
			indexName, nil, []string{"T"}, values.UnknownType, false,
		)
		indexPlans = append(indexPlans, indexPlan)
		indexWrapper := indexPlan
		fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			indexPlan, translateFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
		)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}
	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	scanPlan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanWrapper := &physicalScanWrapper{plan: scanPlan}
	q3 := expressions.ForEachQuantifier(expressions.InitialOf(scanWrapper))

	// The union is its own cascades expression (RFC-184 W2).
	unionExpr := plans.NewRecordQueryUnionPlanFromQuantifiers([]expressions.Quantifier{q1, q2, q3})

	yielded := FireImplementationRule(NewPushUnionThroughFetchRule(), expressions.InitialOf(unionExpr))
	if len(yielded) != 1 {
		t.Fatalf("expected Case-2 partial push to yield once, got %d", len(yielded))
	}
	outer, ok := yielded[0].(*plans.RecordQueryUnionPlan)
	if !ok {
		t.Fatalf("expected outer union plan, got %T", yielded[0])
	}
	outers := outer.GetInners()
	if len(outers) != 2 {
		t.Fatalf("outer union arity: got %d, want 2 (merged fetch + residual scan)", len(outers))
	}
	mergedFetch, ok := outers[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("outer leg 0: got %T, want the merged fetch", outers[0])
	}
	up, ok := mergedFetch.GetInner().(*plans.RecordQueryUnionPlan)
	if !ok || len(up.GetInners()) != 2 {
		t.Fatalf("merged fetch inner: got %T, want Union over the two index plans", mergedFetch.GetInner())
	}
	if outers[1] != plans.RecordQueryPlan(scanPlan) {
		t.Fatalf("outer leg 1: got %T, want the residual scan plan", outers[1])
	}
}

// TestPushIntersectionThroughFetch_RequiredValuesGate pins the
// tryPushValues port: an intersection's comparison keys must translate
// through EVERY leg's translation function; a declining leg drops out
// and (non-dynamic, one leg left) the rule declines entirely.
func TestPushIntersectionThroughFetch_RequiredValuesGate(t *testing.T) {
	t.Parallel()

	okFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	noFn := func(values.Value, values.CorrelationIdentifier, values.CorrelationIdentifier) (values.Value, bool) {
		return nil, false
	}
	makeChild := func(indexName string, fn plans.TranslateValueFunction) expressions.Quantifier {
		indexPlan := plans.NewRecordQueryIndexPlan(
			indexName, nil, []string{"T"}, values.UnknownType, false,
		)
		indexWrapper := indexPlan
		fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			indexPlan, fn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
		)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}

	key := values.NewFieldValue(nil, "PK", values.NullableLong)

	// One leg cannot answer the comparison key → decline.
	declining := plans.NewRecordQueryIntersectionPlanFromQuantifiers(
		[]expressions.Quantifier{makeChild("idx_a", okFn), makeChild("idx_b", noFn)},
		[]values.Value{key},
	)
	if got := FireImplementationRule(NewPushIntersectionThroughFetchRule(), expressions.InitialOf(declining)); len(got) != 0 {
		t.Fatalf("expected decline when a leg cannot answer the comparison key, got %d yields", len(got))
	}

	// Both legs answer → fires, comparison keys preserved on the rebuilt plan.
	firing := plans.NewRecordQueryIntersectionPlanFromQuantifiers(
		[]expressions.Quantifier{makeChild("idx_a", okFn), makeChild("idx_b", okFn)},
		[]values.Value{key},
	)
	yielded := FireImplementationRule(NewPushIntersectionThroughFetchRule(), expressions.InitialOf(firing))
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(yielded))
	}
	fw, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected fetch wrapper, got %T", yielded[0])
	}
	ipn, ok := fw.GetInner().(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf("fetch inner: got %T, want *RecordQueryIntersectionPlan", fw.GetInner())
	}
	if got := ipn.GetComparisonKeyValues(); len(got) != 1 {
		t.Fatalf("comparison keys not preserved: %v", got)
	}
}

// TestPushMergeSortUnionThroughFetch_Fires pins the ordered-union
// instantiation (Java PushSetOperationThroughFetchRule over
// RecordQueryUnionOnValuesPlan — PlanningRuleSet.java:158): the merge
// pushes below the fetch when every leg's translation function answers
// the comparison keys, and the rebuilt plan preserves keys, direction,
// and dedup mode.
func TestPushMergeSortUnionThroughFetch_Fires(t *testing.T) {
	t.Parallel()

	okFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	var indexPlans []*plans.RecordQueryIndexPlan
	makeChild := func(indexName string) expressions.Quantifier {
		indexPlan := plans.NewRecordQueryIndexPlan(
			indexName, nil, []string{"T"}, values.UnknownType, false,
		)
		indexPlans = append(indexPlans, indexPlan)
		indexWrapper := indexPlan
		fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			indexPlan, okFn, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
		)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}
	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	key := values.NewFieldValue(nil, "PK", values.NullableLong)
	msu := plans.NewRecordQueryMergeSortUnionPlanFromQuantifiers(
		[]expressions.Quantifier{q1, q2}, []values.Value{key}, true, true,
	)

	yielded := FireImplementationRule(NewPushMergeSortUnionThroughFetchRule(), expressions.InitialOf(msu))
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(yielded))
	}
	fw, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected fetch wrapper, got %T", yielded[0])
	}
	mp, ok := fw.GetInner().(*plans.RecordQueryMergeSortUnionPlan)
	if !ok {
		t.Fatalf("fetch inner: got %T, want *RecordQueryMergeSortUnionPlan", fw.GetInner())
	}
	if got := mp.GetInners(); len(got) != 2 || got[0] != plans.RecordQueryPlan(indexPlans[0]) {
		t.Fatalf("rebuilt children: %v", got)
	}
	if len(mp.GetComparisonKeys()) != 1 || !mp.IsReverse() || !mp.RemovesDuplicates() {
		t.Fatalf("attributes not preserved: keys=%d reverse=%v dedup=%v",
			len(mp.GetComparisonKeys()), mp.IsReverse(), mp.RemovesDuplicates())
	}
}
