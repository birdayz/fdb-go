package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPushFetchConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct push-through-fetch fixture: " + err.Error())
	}
	return value
}

func pushFetchRowType() *values.RecordType {
	return values.NewRecordType("PUSH_FETCH_ROW", false, []values.Field{
		{Name: "x", FieldType: values.NullableLong},
		{Name: "y", FieldType: values.NullableLong},
		{Name: "name", FieldType: values.NullableString},
		{Name: "PK", FieldType: values.NotNullLong},
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableLong},
	})
}

func pushFetchIndex(indexName string) *plans.RecordQueryIndexPlan {
	return mustPushFetchConstruct(plans.NewRecordQueryIndexPlan(
		indexName, nil, []string{"T"}, pushFetchRowType(), false))
}

func pushFetchScan() *plans.RecordQueryScanPlan {
	return mustPushFetchConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, pushFetchRowType(), false))
}

func pushFetchQOV(alias values.CorrelationIdentifier) values.QuantifiedObjectValue {
	if alias == values.CurrentCorrelation() {
		layout := mustPushFetchConstruct(values.NewOrdinalLayoutForCarrierType(
			pushFetchRowType(),
			[]values.OrdinalTileSpec{{
				Start: 0,
				Width: len(pushFetchRowType().Fields),
				Kind:  values.OrdinalTileFlat,
			}},
			nil,
		))
		return layout.Carrier()
	}
	return mustPushFetchConstruct(values.NewQuantifiedObjectValue(
		alias, pushFetchRowType()))
}

func pushFetchField(root values.Value, field string) values.Value {
	request := mustPushFetchConstruct(values.FieldByName(field))
	return mustPushFetchConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

func pushFetchFieldForAlias(alias values.CorrelationIdentifier, field string) values.Value {
	return pushFetchField(pushFetchQOV(alias), field)
}

func pushFetchFetch(
	inner plans.RecordQueryPlan,
	translate plans.TranslateValueFunction,
) *plans.RecordQueryFetchFromPartialRecordPlan {
	return mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		inner, translate, pushFetchRowType(), plans.FetchIndexRecordsPrimaryKey))
}

func TestMergeFetchIntoCoveringIndex_FiresOnFetchOverIndex(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_name")
	coveringPlan := mustPushFetchConstruct(plans.NewRecordQueryCoveringIndexPlan(indexPlan))
	indexRef := expressions.InitialOf(coveringPlan)

	fetchPlan := pushFetchFetch(indexPlan, nil)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := mustFireImplementationRule(t, rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	if _, ok := yielded[0].(*plans.RecordQueryIndexPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryIndexPlan, got %T", yielded[0])
	}
}

func TestMergeFetchIntoCoveringIndex_DoesNotFireOnNonCoveringIndex(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_name")
	// NOT marked as covering — MergeFetch should NOT fire.
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	fetchPlan := pushFetchFetch(indexPlan, nil)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := mustFireImplementationRule(t, rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded (non-covering index), got %d", len(yielded))
	}
}

func TestMergeFetchIntoCoveringIndex_DoesNotFireOnNonIndex(t *testing.T) {
	t.Parallel()

	// Fetch over a filter (not an index scan) — should not fire.
	filterPlan := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		pushFetchScan(), nil))
	filterRef := expressions.InitialOf(filterPlan)

	fetchPlan := pushFetchFetch(filterPlan, nil)
	fetchQ := expressions.ForEachQuantifier(filterRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})

	ref := expressions.InitialOf(fetchWrapper)

	rule := NewMergeFetchIntoCoveringIndexRule()
	yielded := mustFireImplementationRule(t, rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded, got %d", len(yielded))
	}
}

func TestPushDistinctThroughFetch_Fires(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	distinctPlan := mustPushFetchConstruct(plans.NewRecordQueryDistinctPlan(indexPlan))
	distinctQ := expressions.ForEachQuantifier(fetchRef)
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryDistinctPlan (no
	// physicalDistinctWrapper); the push rule matches it directly.
	distinctWrapper := mustWithQuantifiers(t, distinctPlan, []expressions.Quantifier{distinctQ})

	ref := expressions.InitialOf(distinctWrapper)

	rule := NewPushDistinctThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

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

	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(v values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		// This fixture has one covered predicate column. Return the exact
		// partial-row read rebound to the pushed filter's target alias.
		return pushFetchFieldForAlias(targetAlias, "x"), true
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Filter with one pushable predicate.
	filterQ := expressions.ForEachQuantifier(fetchRef)
	pred := predicates.NewComparisonPredicate(
		pushFetchFieldForAlias(filterQ.GetAlias(), "x"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filterPlan := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		filterQ, []predicates.QueryPredicate{pred}))
	filterWrapper := mustWithQuantifiers(t, filterPlan, []expressions.Quantifier{filterQ})

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Fetch(Filter(index))
	if !IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected *plans.RecordQueryFetchFromPartialRecordPlan, got %T", yielded[0])
	}
	resultFetch := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	pushedFilter, ok := resultFetch.GetInner().(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("pushed fetch inner = %T, want predicates filter", resultFetch.GetInner())
	}
	if pushedFilter.GetInnerAlias().Name() == "" {
		t.Fatal("pushed filter dropped the translated predicate's binding alias")
	}
	if pushedFilter.GetInnerAlias() == pushedFilter.GetInnerQuantifier().GetAlias() {
		t.Fatal("fixture requires distinct logical predicate and physical memo-edge aliases")
	}
	correlated := pushedFilter.GetPredicates()[0].GetCorrelatedTo()
	if len(correlated) != 1 {
		t.Fatalf("pushed predicate correlations = %v, want one translated root", correlated)
	}
	if _, present := correlated[values.CurrentCorrelation()]; !present {
		t.Fatalf("pushed predicate correlations = %v, want selected-input current", correlated)
	}
	comparison, ok := pushedFilter.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("pushed predicate = %T, want *predicates.ComparisonPredicate",
			pushedFilter.GetPredicates()[0])
	}
	field, ok := values.AsFieldValue(comparison.Operand)
	if !ok {
		t.Fatalf("pushed predicate operand = %T, want exact FieldValue", comparison.Operand)
	}
	selectedLayout, err := pushedFilter.GetInner().ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("selected pushed-filter input layout: %v", err)
	}
	if field.ChildValue() != selectedLayout.Carrier() {
		t.Fatalf("pushed predicate root = %p, want exact selected carrier %p",
			field.ChildValue(), selectedLayout.Carrier())
	}
	independentCurrent := pushFetchQOV(values.CurrentCorrelation())
	if independentCurrent == selectedLayout.Carrier() {
		t.Fatal("fixture failed to mint an independent same-shaped current carrier")
	}
	if field.ChildValue() == independentCurrent {
		t.Fatal("pushed predicate accepted an independently minted current carrier")
	}
	originalCorrelated := pred.GetCorrelatedTo()
	if len(originalCorrelated) != 1 {
		t.Fatalf("source predicate correlations = %v, want original filter edge", originalCorrelated)
	}
	if _, present := originalCorrelated[filterQ.GetAlias()]; !present {
		t.Fatalf("source predicate correlations = %v, want unchanged alias %s",
			originalCorrelated, filterQ.GetAlias())
	}
}

func TestPushFilterThroughFetch_NoPushable(t *testing.T) {
	t.Parallel()

	filterInnerAlias := values.UniqueCorrelationIdentifier()

	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	// TranslateValueFunction that NEVER succeeds.
	fetchPlan := pushFetchFetch(indexPlan, plans.UnableToTranslate)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Predicate correlated to the filter's alias — requires
	// translation but UnableToTranslate always fails.
	pred := predicates.NewComparisonPredicate(
		pushFetchQOV(filterInnerAlias),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filterQ := expressions.NamedForEachQuantifier(filterInnerAlias, fetchRef)
	filterPlan := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		filterQ, []predicates.QueryPredicate{pred}))
	filterWrapper := mustWithQuantifiers(t, filterPlan, []expressions.Quantifier{filterQ})

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

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

	indexPlan := pushFetchIndex("idx_a")

	// "Succeeds" (ok=true) but ECHOES the value back unchanged — the
	// unconditional-rebase escape: claims translation yet the result is still
	// QOV(src), correlated to the source alias.
	echoFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	echoFetch := pushFetchFetch(indexPlan, echoFn)

	srcQOV := pushFetchQOV(src)

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
		return pushFetchQOV(targetAlias), true
	}
	rebaseFetch := pushFetchFetch(indexPlan, rebaseFn)
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

// A selected physical filter evaluates its predicate on the fetch's exact
// output carrier, while a fetch candidate translates from the filter's named
// edge. The rule may cross that boundary only for this fetch's carrier handle:
// accepting any same-shaped current root would collapse distinct row phases,
// and weakening the candidate's alias gate would collapse self-join legs.
func TestPushFilterThroughFetch_BridgesOnlyExactFetchOutputCarrier(t *testing.T) {
	t.Parallel()

	rowType := fetchOrdinalRowType()
	candidate := fetchOrdinalCandidate(rowType)
	fetch, ok := candidate.ToScanPlan(nil, false).(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok || fetch == nil {
		t.Fatalf("candidate scan = %T, want FetchFromPartialRecord", candidate.ToScanPlan(nil, false))
	}
	fetchLayout, err := fetch.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("fetch output layout: %v", err)
	}
	fetchCarrier := fetchLayout.Carrier()
	if fetchCarrier == nil {
		t.Fatal("fetch output layout has no exact carrier")
	}

	edgeAlias := values.NamedCorrelationIdentifier("filter_edge")
	edge := fetchOrdinalRoot(edgeAlias, rowType)
	targetAlias := values.UniqueCorrelationIdentifier()

	comparison := func(value values.Value) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			value,
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
		)
	}
	bridge := func(predicate predicates.QueryPredicate) predicates.QueryPredicate {
		t.Helper()
		bridged, bridgeErr := translateFetchOutputCarrierToEdge(
			predicate, fetchCarrier, edge)
		if bridgeErr != nil {
			t.Fatalf("bridge fetch output carrier: %v", bridgeErr)
		}
		return bridged
	}
	fieldRoot := func(value values.Value) values.QuantifiedObjectValue {
		t.Helper()
		field, isField := values.AsFieldValue(value)
		if !isField {
			t.Fatalf("value = %T, want exact FieldValue", value)
		}
		root, isQOV := values.AsQuantifiedObjectValue(field.ChildValue())
		if !isQOV {
			t.Fatalf("field child = %T, want exact QOV", field.ChildValue())
		}
		return root
	}

	// Positive: the exact fetch-owned current root crosses to the edge and then
	// the strict candidate translator moves the covered top-level CITY slot to
	// its partial-row target alias. The original remains carrier-rooted so it is
	// still valid if the rule must retain it as a residual.
	own := fetchOrdinalField(fetchCarrier, 2)
	ownPredicate := comparison(own)
	bridgedOwn := bridge(ownPredicate)
	if bridgedOwn == ownPredicate {
		t.Fatal("exact fetch carrier predicate was not moved to the filter edge")
	}
	originalField := ownPredicate.(*predicates.ComparisonPredicate).Operand
	originalRoot := fieldRoot(originalField)
	if originalRoot != fetchCarrier {
		t.Fatal("phase bridge mutated the original residual predicate")
	}
	bridgedField := bridgedOwn.(*predicates.ComparisonPredicate).Operand
	bridgedRoot := fieldRoot(bridgedField)
	if bridgedRoot.Correlation() != edgeAlias {
		t.Fatalf("bridged root = %v, want filter edge %s", bridgedRoot, edgeAlias.Name())
	}
	translatedOwn, pushed := tryPushPredicate(fetch, edgeAlias, targetAlias, bridgedOwn)
	if !pushed {
		t.Fatal("covered field on the exact fetch carrier did not push")
	}
	translatedComparison, ok := translatedOwn.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("translated predicate = %T, want ComparisonPredicate", translatedOwn)
	}
	assertFetchOrdinalField(t, translatedComparison.Operand, targetAlias, 2)

	assertDeclines := func(
		name string,
		value values.Value,
		wantBridgeChange bool,
	) {
		t.Helper()
		predicate := comparison(value)
		bridged := bridge(predicate)
		if changed := bridged != predicate; changed != wantBridgeChange {
			t.Fatalf("%s bridge changed = %t, want %t", name, changed, wantBridgeChange)
		}
		if translated, accepted := tryPushPredicate(
			fetch, edgeAlias, targetAlias, bridged); accepted || translated != nil {
			t.Fatalf("%s pushed as (%v, %t), want exact decline", name, translated, accepted)
		}
	}

	// Same exact type and reserved-current spelling are insufficient. A second
	// layout owns a different carrier handle and must remain in its own phase.
	independentLayout := mustFetchOrdinalConstruct(values.NewOrdinalLayoutForCarrierType(
		rowType,
		[]values.OrdinalTileSpec{{
			Start: 0,
			Width: len(rowType.Fields),
			Kind:  values.OrdinalTileFlat,
		}},
		nil,
	))
	if independentLayout.Carrier() == fetchCarrier {
		t.Fatal("precondition: independently built layout reused fetch carrier handle")
	}
	assertDeclines("independent same-type current", fetchOrdinalField(independentLayout.Carrier(), 2), false)

	// Correlation remains decisive for a same-shaped foreign named row.
	foreign := fetchOrdinalField(
		fetchOrdinalRoot(values.NamedCorrelationIdentifier("foreign_edge"), rowType), 2)
	assertDeclines("foreign named root", foreign, false)

	// A different exact current row is not this fetch's phase carrier either,
	// even when it happens to expose the same display column at the same slot.
	wrongType := values.NewRecordType("wrong_fetch_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ADDR", FieldType: values.NullableString},
		{Name: "CITY", FieldType: values.NullableLong},
	})
	wrongTypeLayout := mustFetchOrdinalConstruct(values.NewOrdinalLayoutForCarrierType(
		wrongType,
		[]values.OrdinalTileSpec{{
			Start: 0,
			Width: len(wrongType.Fields),
			Kind:  values.OrdinalTileFlat,
		}},
		nil,
	))
	wrongTypedField := fetchOrdinalField(wrongTypeLayout.Carrier(), 2)
	assertDeclines("wrong exact row type", wrongTypedField, false)

	// The bridge preserves the complete accessor path. ADDR.CITY crosses onto
	// the declared edge, then the candidate rejects it instead of flattening it
	// into the covered top-level CITY column.
	fusedPath := fetchOrdinalField(fetchCarrier, 1, 0)
	assertDeclines("fused accessor path", fusedPath, true)
}

func TestPushFilterThroughFetch_PartialPush(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_a")
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
			return pushFetchQOV(targetAlias), true
		}
		return nil, false
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Both predicates are correlated to the filter's inner alias
	// (simulating real predicates that reference the flowing row).
	pushablePred := predicates.NewComparisonPredicate(
		pushFetchQOV(filterInnerAlias),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	residualPred := predicates.NewComparisonPredicate(
		pushFetchQOV(filterInnerAlias),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
	)
	filterQ := expressions.NamedForEachQuantifier(filterInnerAlias, fetchRef)
	filterPlan := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		filterQ, []predicates.QueryPredicate{pushablePred, residualPred}))
	filterWrapper := mustWithQuantifiers(t, filterPlan, []expressions.Quantifier{filterQ})

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}
	// Result should be Filter(residual, Fetch(Filter(pushed, index)))
	if IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected residual filter wrapper on top, got fetch wrapper directly")
	}
	if !IsPhysicalPredicatesFilter(yielded[0]) {
		t.Fatalf("expected *plans.RecordQueryPredicatesFilterPlan, got %T", yielded[0])
	}
}

func TestPushMapThroughFetch_Fires(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	translateFn := func(_ values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		return pushFetchFieldForAlias(targetAlias, "x"), true
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	mapQ := expressions.ForEachQuantifier(fetchRef)
	resultVal := pushFetchFieldForAlias(mapQ.GetAlias(), "x")
	mapPlan := mustPushFetchConstruct(plans.NewRecordQueryMapPlanFromQuantifier(mapQ, resultVal))
	mapWrapper := mustWithQuantifiers(t, mapPlan, []expressions.Quantifier{mapQ})

	ref := expressions.InitialOf(mapWrapper)

	rule := NewPushMapThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

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

	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	// UnableToTranslate — map can't be pushed.
	fetchPlan := pushFetchFetch(indexPlan, plans.UnableToTranslate)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Use a correlated FieldValue so translation is actually attempted.
	resultVal := pushFetchFieldForAlias(mapAlias, "x")
	mapQ := expressions.NamedForEachQuantifier(mapAlias, fetchRef)
	mapPlan := mustPushFetchConstruct(plans.NewRecordQueryMapPlanFromQuantifier(mapQ, resultVal))
	mapWrapper := mustWithQuantifiers(t, mapPlan, []expressions.Quantifier{mapQ})

	ref := expressions.InitialOf(mapWrapper)

	rule := NewPushMapThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yielded, got %d", len(yielded))
	}
}

func TestMergeProjectionAndFetch_WholeRecordRetainsFetch(t *testing.T) {
	t.Parallel()

	newSubject := func(t *testing.T, wholeRecord bool) *expressions.Reference {
		t.Helper()
		parameterAlias := values.UniqueCorrelationIdentifier()
		rowType := testRecordRowType("T", "A", "B")
		candidate := newKnownDistinctValueIndexCandidate(
			"idx_a",
			[]string{"T"},
			[]string{"A"},
			[]values.CorrelationIdentifier{parameterAlias},
			rowType,
			false,
			nil,
		)
		template, ok := candidate.ToScanPlan(
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{},
			false,
		).(*plans.RecordQueryFetchFromPartialRecordPlan)
		if !ok {
			t.Fatalf("candidate scan = %T, want Fetch(IndexScan)", template)
		}
		indexPlan := template.GetInner()
		if indexPlan == nil {
			t.Fatal("candidate fetch has no index child")
		}
		fetch := mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
			expressions.ForEachQuantifier(expressions.InitialOf(indexPlan)),
			template.GetTranslateValueFunction(),
			template.GetResultType(),
			template.GetFetchIndexRecords(),
		))
		fetchRef := expressions.InitialOf(fetch)

		projectionAlias := values.UniqueCorrelationIdentifier()
		var projectedValue values.Value
		if wholeRecord {
			projectedValue = mustPushFetchConstruct(values.NewQuantifiedObjectValue(
				projectionAlias, rowType))
		} else {
			projectedValue = testColumnRef(
				mustPushFetchConstruct(values.NewQuantifiedObjectValue(projectionAlias, rowType)),
				rowType, "A", values.NullableLong,
			)
		}
		projection, err := plans.NewRecordQueryProjectionPlanFromQuantifier(
			[]values.Value{projectedValue},
			nil,
			expressions.NamedForEachQuantifier(projectionAlias, fetchRef),
		)
		if wholeRecord {
			if projection != nil || !errors.Is(err, values.ErrWholeRowProjection) {
				t.Fatalf("whole-record projection = (%T, %v), want constructor rejection",
					projection, err)
			}
			return nil
		}
		if err != nil {
			t.Fatalf("construct scalar projection: %v", err)
		}
		return expressions.InitialOf(projection)
	}

	if ref := newSubject(t, true); ref != nil {
		t.Fatal("rejected whole-record projection returned a memo Reference")
	}

	if yielded := mustFireImplementationRule(t,
		NewMergeProjectionAndFetchRule(),
		newSubject(t, false),
	); len(yielded) != 1 {
		t.Fatalf(
			"covered scalar projection yielded %d plans, want 1 positive control",
			len(yielded),
		)
	}
}

func TestPushUnionThroughFetch_AllChildrenHaveFetches(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}

	// Build two fetch-over-index children.
	makeChild := func(indexName string) expressions.Quantifier {
		indexPlan := pushFetchIndex(indexName)
		indexWrapper := indexPlan
		indexRef := expressions.InitialOf(indexWrapper)

		fetchPlan := pushFetchFetch(indexPlan, translateFn)
		fetchQ := expressions.ForEachQuantifier(indexRef)
		fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
		fetchRef := expressions.InitialOf(fetchWrapper)
		return expressions.ForEachQuantifier(fetchRef)
	}

	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	// The union is its own cascades expression (RFC-184 W2).
	unionExpr := mustPushFetchConstruct(plans.NewRecordQueryUnionPlanFromQuantifiers(
		[]expressions.Quantifier{q1, q2}))

	ref := expressions.InitialOf(unionExpr)

	rule := NewPushUnionThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

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
	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)
	q1 := expressions.ForEachQuantifier(fetchRef)

	// Second child: plain scan (no fetch).
	scanPlan := pushFetchScan()
	scanWrapper := scanPlan
	scanRef := expressions.InitialOf(scanWrapper)
	q2 := expressions.ForEachQuantifier(scanRef)

	// The union is its own cascades expression (RFC-184 W2).
	unionExpr := mustPushFetchConstruct(plans.NewRecordQueryUnionPlanFromQuantifiers(
		[]expressions.Quantifier{q1, q2}))

	ref := expressions.InitialOf(unionExpr)

	rule := NewPushUnionThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

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
	child := pushFetchQOV(alias)
	fv := pushFetchField(child, "name")

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

	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	indexRef := expressions.InitialOf(indexWrapper)

	// TranslateValueFunction that succeeds for FieldValues with field "x"
	// but fails for field "y". Simulates an index covering column "x"
	// but not "y".
	translateFn := func(v values.Value, _, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		if fv, ok := values.AsFieldValue(v); ok {
			if fv.DisplayName() == "x" {
				return pushFetchFieldForAlias(targetAlias, "x"), true
			}
		}
		return nil, false
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Predicates using FieldValue WITH child — correlated to filterAlias.
	pushablePred := predicates.NewComparisonPredicate(
		pushFetchFieldForAlias(filterAlias, "x"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	residualPred := predicates.NewComparisonPredicate(
		pushFetchFieldForAlias(filterAlias, "y"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
	)

	filterQ := expressions.NamedForEachQuantifier(filterAlias, fetchRef)
	filterPlan := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		filterQ, []predicates.QueryPredicate{pushablePred, residualPred}))
	filterWrapper := mustWithQuantifiers(t, filterPlan, []expressions.Quantifier{filterQ})

	ref := expressions.InitialOf(filterWrapper)

	rule := NewPushFilterThroughFetchRule()
	yielded := mustFireImplementationRule(t, rule, ref)

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
		indexPlan := pushFetchIndex(indexName)
		indexPlans = append(indexPlans, indexPlan)
		indexWrapper := indexPlan
		fetchPlan := pushFetchFetch(indexPlan, translateFn)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}
	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	// The union is its own cascades expression (RFC-184 W2) — there is no
	// separate plan-snapshot to go stale; the pushed union is built fresh from
	// the live pushed-down leg quantifiers.
	unionExpr := mustPushFetchConstruct(plans.NewRecordQueryUnionPlanFromQuantifiers(
		[]expressions.Quantifier{q1, q2}))

	yielded := mustFireImplementationRule(t, NewPushUnionThroughFetchRule(), expressions.InitialOf(unionExpr))
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
	indexPlan := pushFetchIndex("idx_a")
	indexWrapper := indexPlan
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	q := expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))

	// The InUnion is its own cascades expression over the live fetch edge now
	// (RFC-184 W2) — no wrapper snapshot.
	inUnionPlan := mustPushFetchConstruct(plans.NewRecordQueryInUnionPlanFromQuantifier(
		q, []string{"__in_b"}, nil, false, 7,
	))

	yielded := mustFireImplementationRule(t, NewPushInUnionThroughFetchRule(), expressions.InitialOf(inUnionPlan))
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
		indexPlan := pushFetchIndex(indexName)
		indexPlans = append(indexPlans, indexPlan)
		indexWrapper := indexPlan
		fetchPlan := pushFetchFetch(indexPlan, translateFn)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}
	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	scanPlan := pushFetchScan()
	scanWrapper := scanPlan
	q3 := expressions.ForEachQuantifier(expressions.InitialOf(scanWrapper))

	// The union is its own cascades expression (RFC-184 W2).
	unionExpr := mustPushFetchConstruct(plans.NewRecordQueryUnionPlanFromQuantifiers(
		[]expressions.Quantifier{q1, q2, q3}))

	yielded := mustFireImplementationRule(t, NewPushUnionThroughFetchRule(), expressions.InitialOf(unionExpr))
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
		indexPlan := pushFetchIndex(indexName)
		indexWrapper := indexPlan
		fetchPlan := pushFetchFetch(indexPlan, fn)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}

	key := pushFetchFieldForAlias(values.CurrentCorrelation(), "PK")

	// One leg cannot answer the comparison key → decline.
	declining := mustPushFetchConstruct(plans.NewRecordQueryIntersectionPlanFromQuantifiers(
		[]expressions.Quantifier{makeChild("idx_a", okFn), makeChild("idx_b", noFn)},
		[]values.Value{key},
	))
	if got := mustFireImplementationRule(t, NewPushIntersectionThroughFetchRule(), expressions.InitialOf(declining)); len(got) != 0 {
		t.Fatalf("expected decline when a leg cannot answer the comparison key, got %d yields", len(got))
	}

	// Both legs answer → fires, comparison keys preserved on the rebuilt plan.
	firing := mustPushFetchConstruct(plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering(
		[]expressions.Quantifier{makeChild("idx_a", okFn), makeChild("idx_b", okFn)},
		[]properties.ProvidedOrderingPart{{
			Value:     key,
			SortOrder: properties.ProvidedSortOrderDescending,
		}},
		true,
	))
	yielded := mustFireImplementationRule(t, NewPushIntersectionThroughFetchRule(), expressions.InitialOf(firing))
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
	if !ipn.IsReverse() {
		t.Fatal("push-through-fetch dropped the intersection's reverse flag")
	}
	if parts := ipn.GetComparisonKeyOrderingParts(); len(parts) != 1 ||
		parts[0].SortOrder != properties.ProvidedSortOrderDescending {
		t.Fatalf("push-through-fetch dropped semantic ordering parts: %#v", parts)
	}
}

func TestPushIntersectionThroughFetch_BridgesOnlyAdmittedComparisonCarrier(t *testing.T) {
	t.Parallel()

	rowType := fetchOrdinalRowType()
	makeChild := func(indexName string) expressions.Quantifier {
		candidate := newKnownDistinctValueIndexCandidate(
			indexName,
			[]string{"T"},
			[]string{"CITY"},
			[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("p0")},
			rowType,
			false,
			[]string{"ID"},
		)
		fetch, ok := candidate.ToScanPlan(nil, false).(*plans.RecordQueryFetchFromPartialRecordPlan)
		if !ok || fetch == nil {
			t.Fatalf("candidate scan = %T, want FetchFromPartialRecord", candidate.ToScanPlan(nil, false))
		}
		return expressions.ForEachQuantifier(expressions.FinalOf(fetch))
	}
	children := []expressions.Quantifier{makeChild("IDX_CITY_A"), makeChild("IDX_CITY_B")}
	firstLayout, err := children[0].GetRangesOver().FinalMembers()[0].(plans.RecordQueryPlan).ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("first fetch output layout: %v", err)
	}
	sourceLayout := mustFetchOrdinalConstruct(values.NewOrdinalLayoutForCarrierType(
		rowType,
		[]values.OrdinalTileSpec{{Start: 0, Width: len(rowType.Fields), Kind: values.OrdinalTileFlat}},
		nil,
	))
	source := sourceLayout.Carrier()
	if source == firstLayout.Carrier() {
		t.Fatal("precondition: declared comparison-key source reused the fetch output carrier")
	}
	key := fetchOrdinalField(source, 2)
	parts := []properties.ProvidedOrderingPart{{
		Value: key, SortOrder: properties.ProvidedSortOrderAscending,
	}}
	intersection := mustPushFetchConstruct(
		plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrderingAndSource(
			children, parts, false, source))
	stored := intersection.GetComparisonKeyValues()
	if len(stored) != 1 {
		t.Fatalf("stored comparison keys = %d, want 1", len(stored))
	}
	intersectionLayout, err := intersection.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("intersection output layout: %v", err)
	}
	storedField, ok := values.AsFieldValue(stored[0])
	if !ok {
		t.Fatalf("stored comparison key = %T, want FieldValue", stored[0])
	}
	storedRoot, ok := values.AsQuantifiedObjectValue(storedField.ChildValue())
	if !ok || storedRoot != intersectionLayout.Carrier() {
		t.Fatalf("stored comparison root = %v, want exact intersection carrier", storedField.ChildValue())
	}
	if originalField, ok := values.AsFieldValue(key); !ok || originalField.ChildValue() != source {
		t.Fatal("constructor mutated its source comparison key")
	}

	yielded := mustFireImplementationRule(
		t, NewPushIntersectionThroughFetchRule(), expressions.InitialOf(intersection))
	if len(yielded) != 1 {
		t.Fatalf("exact admitted comparison carrier yielded %d plans, want 1", len(yielded))
	}
	fetch, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("yield = %T, want FetchFromPartialRecord", yielded[0])
	}
	if _, ok := fetch.GetInner().(*plans.RecordQueryIntersectionPlan); !ok {
		t.Fatalf("pushed fetch inner = %T, want Intersection", fetch.GetInner())
	}

	assertRejected := func(
		name string,
		bad values.Value,
		declaredSource values.QuantifiedObjectValue,
	) {
		t.Helper()
		badParts := []properties.ProvidedOrderingPart{{
			Value: bad, SortOrder: properties.ProvidedSortOrderAscending,
		}}
		if result, constructErr := plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrderingAndSource(
			children, badParts, false, declaredSource); constructErr == nil || result != nil {
			t.Fatalf("%s construction = (%T, %v), want exact rejection", name, result, constructErr)
		}
	}

	independentLayout := mustFetchOrdinalConstruct(values.NewOrdinalLayoutForCarrierType(
		rowType,
		[]values.OrdinalTileSpec{{Start: 0, Width: len(rowType.Fields), Kind: values.OrdinalTileFlat}},
		nil,
	))
	assertRejected(
		"independently minted same-type current",
		fetchOrdinalField(independentLayout.Carrier(), 2),
		source,
	)
	foreignRoot := fetchOrdinalRoot(values.NamedCorrelationIdentifier("foreign_intersection"), rowType)
	assertRejected("foreign named root", fetchOrdinalField(foreignRoot, 2), foreignRoot)
	wrongType := values.NewRecordType("wrong_intersection", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ADDR", FieldType: rowType.Fields[1].FieldType},
		{Name: "CITY", FieldType: values.NullableLong},
	})
	wrongLayout := mustFetchOrdinalConstruct(values.NewOrdinalLayoutForCarrierType(
		wrongType,
		[]values.OrdinalTileSpec{{Start: 0, Width: len(wrongType.Fields), Kind: values.OrdinalTileFlat}},
		nil,
	))
	assertRejected("wrong exact type", fetchOrdinalField(wrongLayout.Carrier(), 2), wrongLayout.Carrier())
	assertRejected("nested path", fetchOrdinalField(source, 1, 0), source)
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
		indexPlan := pushFetchIndex(indexName)
		indexPlans = append(indexPlans, indexPlan)
		indexWrapper := indexPlan
		fetchPlan := pushFetchFetch(indexPlan, okFn)
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(indexWrapper))
		fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
		return expressions.ForEachQuantifier(expressions.InitialOf(fetchWrapper))
	}
	q1 := makeChild("idx_a")
	q2 := makeChild("idx_b")

	key := pushFetchFieldForAlias(values.CurrentCorrelation(), "PK")
	msu := mustPushFetchConstruct(plans.NewRecordQueryMergeSortUnionPlanFromQuantifiers(
		[]expressions.Quantifier{q1, q2}, []values.Value{key}, true, true,
	))

	yielded := mustFireImplementationRule(t, NewPushMergeSortUnionThroughFetchRule(), expressions.InitialOf(msu))
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

func TestPushInJoinThroughFetchRule_Fires(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		sourceKind plans.InSourceKind
		inValues   []any
	}{
		{
			name:       "static values",
			sourceKind: plans.InSourceValues,
			inValues:   []any{int64(1), int64(2)},
		},
		{
			name:       "runtime parameter",
			sourceKind: plans.InSourceParameter,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
				return v, true
			}
			indexPlan := pushFetchIndex("idx_in")
			fetch := mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
				expressions.ForEachQuantifier(expressions.InitialOf(indexPlan)),
				translateFn,
				pushFetchRowType(),
				plans.FetchIndexRecordsPrimaryKey,
			))
			inJoin := mustPushFetchConstruct(plans.NewRecordQueryInJoinPlanFromQuantifier(
				expressions.ForEachQuantifier(expressions.InitialOf(fetch)),
				"__in_value",
				true,
				true,
			))
			inJoin.SetInValues(tc.inValues)
			inJoin.SetSourceKind(tc.sourceKind)

			yielded := mustFireImplementationRule(t,
				NewPushInJoinThroughFetchRule(),
				expressions.InitialOf(inJoin),
			)
			if len(yielded) != 1 {
				t.Fatalf("expected one Fetch(InJoin(index)) result, got %d", len(yielded))
			}
			liftedFetch, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
			if !ok {
				t.Fatalf("yielded %T, want *plans.RecordQueryFetchFromPartialRecordPlan", yielded[0])
			}
			pushed, ok := liftedFetch.GetInner().(*plans.RecordQueryInJoinPlan)
			if !ok {
				t.Fatalf(
					"lifted fetch inner = %T, want *plans.RecordQueryInJoinPlan",
					liftedFetch.GetInner(),
				)
			}
			if pushed.GetInner() != plans.RecordQueryPlan(indexPlan) {
				t.Fatalf("pushed InJoin inner = %T, want the original index plan", pushed.GetInner())
			}
			if pushed.GetBindingName() != "__in_value" || !pushed.IsSorted() || !pushed.IsReverse() {
				t.Fatalf(
					"InJoin attributes changed: binding=%q sorted=%v reverse=%v",
					pushed.GetBindingName(),
					pushed.IsSorted(),
					pushed.IsReverse(),
				)
			}
			if pushed.GetSourceKind() != tc.sourceKind {
				t.Fatalf(
					"InJoin source kind = %v, want %v",
					pushed.GetSourceKind(),
					tc.sourceKind,
				)
			}
			gotValues := pushed.GetInValues()
			if len(gotValues) != len(tc.inValues) {
				t.Fatalf("InJoin values = %#v, want %#v", gotValues, tc.inValues)
			}
			for i := range gotValues {
				if gotValues[i] != tc.inValues[i] {
					t.Fatalf("InJoin value %d = %#v, want %#v", i, gotValues[i], tc.inValues[i])
				}
			}
		})
	}
}

func TestPushUnorderedUnionThroughFetchRule_Fires(t *testing.T) {
	t.Parallel()

	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	indexPlans := make([]*plans.RecordQueryIndexPlan, 0, 2)
	makeFetchLeg := func(indexName string) expressions.Quantifier {
		indexPlan := pushFetchIndex(indexName)
		indexPlans = append(indexPlans, indexPlan)
		fetch := mustPushFetchConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
			expressions.ForEachQuantifier(expressions.InitialOf(indexPlan)),
			translateFn,
			pushFetchRowType(),
			plans.FetchIndexRecordsPrimaryKey,
		))
		return expressions.ForEachQuantifier(expressions.InitialOf(fetch))
	}
	union := mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers(
		[]expressions.Quantifier{makeFetchLeg("idx_a"), makeFetchLeg("idx_b")},
	))

	yielded := mustFireImplementationRule(t,
		NewPushUnorderedUnionThroughFetchRule(),
		expressions.InitialOf(union),
	)
	if len(yielded) != 1 {
		t.Fatalf("expected one Fetch(UnorderedUnion(indexes)) result, got %d", len(yielded))
	}
	liftedFetch, ok := yielded[0].(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("yielded %T, want *plans.RecordQueryFetchFromPartialRecordPlan", yielded[0])
	}
	pushed, ok := liftedFetch.GetInner().(*plans.RecordQueryUnorderedUnionPlan)
	if !ok {
		t.Fatalf(
			"lifted fetch inner = %T, want *plans.RecordQueryUnorderedUnionPlan",
			liftedFetch.GetInner(),
		)
	}
	inners := pushed.GetInners()
	if len(inners) != 2 {
		t.Fatalf("pushed unordered union has %d children, want two", len(inners))
	}
	for i := range inners {
		if inners[i] != plans.RecordQueryPlan(indexPlans[i]) {
			t.Fatalf("pushed child %d = %T, want the original index plan", i, inners[i])
		}
	}
}

func TestRemoveProjectionRule_WholeRowIdentityRejectedAtAdmission(t *testing.T) {
	t.Parallel()

	scan := pushFetchScan()
	scanRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(scanRef)
	projection, err := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{pushFetchQOV(innerQ.GetAlias())},
		nil,
		innerQ,
	)
	if projection != nil || !errors.Is(err, values.ErrWholeRowProjection) {
		t.Fatalf("whole-row physical projection = (%T, %v), want nil and ErrWholeRowProjection",
			projection, err)
	}
}

func TestRemoveProjectionRule_AliasedWholeRowRejectedAtAdmission(t *testing.T) {
	t.Parallel()

	scan := pushFetchScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projection, err := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{pushFetchQOV(innerQ.GetAlias())},
		[]string{"RENAMED_ROW"},
		innerQ,
	)
	if projection != nil || !errors.Is(err, values.ErrWholeRowProjection) {
		t.Fatalf("aliased whole-row physical projection = (%T, %v), want nil and ErrWholeRowProjection",
			projection, err)
	}
}

func TestRemoveProjectionRule_DeclinesForWrongQuantifier(t *testing.T) {
	t.Parallel()

	scan := pushFetchScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	other := mustPushFetchConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("OTHER"), values.NotNullLong))
	projection := mustPushFetchConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{other},
		nil,
		innerQ,
	))

	yielded := mustFireImplementationRule(t,
		NewRemoveProjectionRule(),
		expressions.InitialOf(projection),
	)
	if len(yielded) != 0 {
		t.Fatalf("rule erased a projection over the wrong quantifier — got %d yields", len(yielded))
	}
}

func TestRemoveProjectionRule_EmptyAliasWholeRowRejectedAtAdmission(t *testing.T) {
	t.Parallel()

	scan := pushFetchScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projection, err := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{pushFetchQOV(innerQ.GetAlias())},
		[]string{""},
		innerQ,
	)
	if projection != nil || !errors.Is(err, values.ErrWholeRowProjection) {
		t.Fatalf("empty-alias whole-row physical projection = (%T, %v), want nil and ErrWholeRowProjection",
			projection, err)
	}
}
