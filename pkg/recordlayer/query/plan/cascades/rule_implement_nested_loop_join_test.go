package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

func mustNLJConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct nested-loop-join fixture: " + err.Error())
	}
	return value
}

func nljSimpleRowType(recordName string) *values.RecordType {
	return values.NewRecordType(recordName, false, []values.Field{{
		Name: "ID", FieldType: values.NotNullLong,
	}})
}

func nljLogicalScan(recordName string) *expressions.FullUnorderedScanExpression {
	return mustNLJConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordName}, nljSimpleRowType(recordName)))
}

func nljPhysicalScan(recordName string) *plans.RecordQueryScanPlan {
	return mustNLJConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordName}, nljSimpleRowType(recordName), false))
}

func nljFlowed(quantifier expressions.Quantifier) values.Value {
	return mustNLJConstruct(quantifier.RequireFlowedObjectValue())
}

func nljField(quantifier expressions.Quantifier, ordinal int) values.Value {
	return mustNLJConstruct(values.ResolveFieldOrdinals(nljFlowed(quantifier), []int{ordinal}))
}

type nljPrimaryKeyPlanContext struct {
	PlanContext
	primaryKey []string
}

func (c nljPrimaryKeyPlanContext) GetPrimaryKeyColumns(string) []string {
	return append([]string(nil), c.primaryKey...)
}

func TestNormalizeCorrelatedExplodeCollectionPlan(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("T1")
	logicalType := values.NewRecordType("T1", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: values.NewArrayType(true, values.NotNullInt)},
	})
	physicalType := values.NewRecordType("", false, logicalType.Fields)
	logicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, logicalType))
	physicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, physicalType))
	collection := mustNLJConstruct(values.ResolveFieldOrdinals(logicalRoot, []int{1}))
	explode := mustNLJConstruct(plans.NewRecordQueryExplodePlanWithOrdinality(collection, true))

	normalizedPlan, changed, err := normalizeCorrelatedExplodeCollectionPlan(
		explode, alias, physicalRoot)
	if err != nil {
		t.Fatalf("normalize Explode: %v", err)
	}
	if !changed || normalizedPlan == explode {
		t.Fatal("name-only logical source difference did not rebuild Explode")
	}
	normalizedExplode, ok := normalizedPlan.(*plans.RecordQueryExplodePlan)
	if !ok || !normalizedExplode.IsWithOrdinality() {
		t.Fatalf("normalized plan = %T, want WITH ORDINALITY Explode", normalizedPlan)
	}
	normalizedCollection, ok := values.AsFieldValue(normalizedExplode.GetCollectionValue())
	if !ok || normalizedCollection.ChildValue() != physicalRoot {
		t.Fatalf("normalized collection root = %T/%v, want exact physical root %p",
			normalizedExplode.GetCollectionValue(), normalizedCollection, physicalRoot)
	}
	if path := normalizedCollection.Path().Ordinals(); len(path) != 1 || path[0] != 1 {
		t.Fatalf("normalized collection path = %v, want [1]", path)
	}
	originalCollection, ok := values.AsFieldValue(explode.GetCollectionValue())
	if !ok || originalCollection.ChildValue() != logicalRoot {
		t.Fatal("normalization mutated the source Explode collection")
	}

	filterAlias := values.NamedCorrelationIdentifier("X")
	filter := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		explode,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		filterAlias))
	normalizedPlan, changed, err = normalizeCorrelatedExplodeCollectionPlan(
		filter, alias, physicalRoot)
	if err != nil {
		t.Fatalf("normalize filtered Explode: %v", err)
	}
	normalizedFilter, ok := normalizedPlan.(*plans.RecordQueryPredicatesFilterPlan)
	if !changed || !ok || normalizedFilter == filter {
		t.Fatalf("filtered Explode normalization = %T changed=%v", normalizedPlan, changed)
	}
	if normalizedFilter.GetInnerAlias() != filterAlias {
		t.Fatalf("filtered Explode alias = %v, want %v",
			normalizedFilter.GetInnerAlias(), filterAlias)
	}
	filteredExplode, ok := normalizedFilter.GetInner().(*plans.RecordQueryExplodePlan)
	if !ok {
		t.Fatalf("normalized filter child = %T, want Explode", normalizedFilter.GetInner())
	}
	filteredCollection, ok := values.AsFieldValue(filteredExplode.GetCollectionValue())
	if !ok || filteredCollection.ChildValue() != physicalRoot {
		t.Fatalf("filtered collection root = %T/%v, want exact physical root %p",
			filteredExplode.GetCollectionValue(), filteredCollection, physicalRoot)
	}
	if filter.GetInner() != explode {
		t.Fatal("normalization mutated the source filter child")
	}

	// Inline VALUES freezes its public SQL column spelling while its selected
	// physical Explode carrier retains the constructed row's original spelling.
	// The exact ordinal/type contract makes that top-level name normalization
	// safe; neither a rendered name nor a name lookup participates.
	caseAlias := values.NamedCorrelationIdentifier("VALUES")
	upperType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: values.NewArrayType(true, values.NotNullInt)},
	})
	lowerType := values.NewRecordType("", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "arr", FieldType: values.NewArrayType(true, values.NotNullInt)},
	})
	upperRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(caseAlias, upperType))
	lowerRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(caseAlias, lowerType))
	upperCollection := mustNLJConstruct(values.ResolveFieldOrdinals(upperRoot, []int{1}))
	caseExplode := mustNLJConstruct(plans.NewRecordQueryExplodePlanWithOrdinality(upperCollection, true))

	casePlan, changed, err := normalizeCorrelatedExplodeCollectionPlan(
		caseExplode, caseAlias, lowerRoot)
	if err != nil {
		t.Fatalf("normalize constructed-row field names: %v", err)
	}
	caseNormalized, ok := casePlan.(*plans.RecordQueryExplodePlan)
	if !changed || !ok || caseNormalized == caseExplode {
		t.Fatalf("constructed-row normalization = %T changed=%v", casePlan, changed)
	}
	caseField, ok := values.AsFieldValue(caseNormalized.GetCollectionValue())
	if !ok {
		t.Fatalf("constructed-row collection = %T, want exact FieldValue", caseNormalized.GetCollectionValue())
	}
	caseRoot, ok := values.AsQuantifiedObjectValue(caseField.ChildValue())
	if !ok || caseRoot.Correlation() != caseAlias || !caseRoot.FlowedType().Equals(lowerType) {
		t.Fatalf("constructed-row root = %T/%v, want %s over %v", caseField.ChildValue(), caseRoot, caseAlias, lowerType)
	}
	if path := caseField.Path().Ordinals(); len(path) != 1 || path[0] != 1 {
		t.Fatalf("constructed-row collection path = %v, want [1]", path)
	}
	originalCaseField, ok := values.AsFieldValue(caseExplode.GetCollectionValue())
	if !ok || originalCaseField.ChildValue() != upperRoot {
		t.Fatal("constructed-row normalization mutated the source Explode")
	}

	assertConstructedRowDeclines := func(
		label string,
		source values.CorrelationIdentifier,
		targetType *values.RecordType,
	) {
		t.Helper()
		targetRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(source, targetType))
		got, gotChanged, gotErr := normalizeCorrelatedExplodeCollectionPlan(
			caseExplode, source, targetRoot)
		if gotErr != nil || gotChanged || got != caseExplode {
			t.Fatalf("%s changed constructed-row Explode: plan=%T changed=%v err=%v", label, got, gotChanged, gotErr)
		}
	}
	assertConstructedRowDeclines("foreign alias", values.NamedCorrelationIdentifier("FOREIGN"), lowerType)
	assertConstructedRowDeclines("width drift", caseAlias, values.NewRecordType("", false, []values.Field{
		{Name: "arr", FieldType: values.NewArrayType(true, values.NotNullInt)},
	}))
	assertConstructedRowDeclines("leaf type drift", caseAlias, values.NewRecordType("", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "arr", FieldType: values.NewArrayType(true, values.NotNullLong)},
	}))
	assertConstructedRowDeclines("record nullability drift", caseAlias, values.NewRecordType("", true, lowerType.Fields))
	assertConstructedRowDeclines("ordinal path drift", caseAlias, values.NewRecordType("", false, []values.Field{
		{Name: "arr", FieldType: values.NewArrayType(true, values.NotNullInt)},
		{Name: "id", FieldType: values.NotNullLong},
	}))
	if originalCaseField.ChildValue() != upperRoot {
		t.Fatal("negative constructed-row probes mutated the source collection")
	}

	foreignPlan, changed, err := normalizeCorrelatedExplodeCollectionPlan(
		explode, values.NamedCorrelationIdentifier("FOREIGN"), physicalRoot)
	if err != nil || changed || foreignPlan != explode {
		t.Fatalf("foreign alias changed Explode: plan=%T changed=%v err=%v",
			foreignPlan, changed, err)
	}
	typeDrift := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: values.NewArrayType(true, values.NotNullLong)},
	})
	typeDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, typeDrift))
	driftedPlan, changed, err := normalizeCorrelatedExplodeCollectionPlan(
		explode, alias, typeDriftRoot)
	if err != nil || changed || driftedPlan != explode {
		t.Fatalf("exact element-type drift changed Explode: plan=%T changed=%v err=%v",
			driftedPlan, changed, err)
	}
	pathDrift := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "OTHER", FieldType: values.NewArrayType(true, values.NotNullInt)},
	})
	pathDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, pathDrift))
	driftedPlan, changed, err = normalizeCorrelatedExplodeCollectionPlan(
		explode, alias, pathDriftRoot)
	if err != nil || changed || driftedPlan != explode {
		t.Fatalf("accessor-name drift changed Explode: plan=%T changed=%v err=%v",
			driftedPlan, changed, err)
	}

	ordinary := nljPhysicalScan("ORDINARY")
	ordinaryPlan, changed, err := normalizeCorrelatedExplodeCollectionPlan(
		ordinary, alias, physicalRoot)
	if err != nil || changed || ordinaryPlan != ordinary {
		t.Fatalf("ordinary correlated leg changed: plan=%T changed=%v err=%v",
			ordinaryPlan, changed, err)
	}
}

func TestNormalizeCorrelatedScanComparisonPlan(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("U")
	logicalType := values.NewRecordType("U", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "V", FieldType: values.NullableLong},
	})
	physicalType := values.NewRecordType("", false, logicalType.Fields)
	logicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, logicalType))
	physicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, physicalType))
	logicalID := mustNLJConstruct(values.ResolveFieldOrdinals(logicalRoot, []int{0}))
	equality := &predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: logicalID,
	}
	merged := predicates.EmptyComparisonRange().Merge(equality)
	if !merged.Ok {
		t.Fatal("construct correlated comparison range")
	}
	ranges := []*predicates.ComparisonRange{merged.Range}

	scan := mustNLJConstruct(plans.NewRecordQueryScanPlan(
		[]string{"INNER"}, nljSimpleRowType("INNER"), false)).
		WithScanComparisons(ranges)
	index := mustNLJConstruct(plans.NewRecordQueryIndexPlan(
		"IDX", ranges, []string{"INNER"}, nljSimpleRowType("INNER"), false))

	for _, test := range []struct {
		name string
		plan plans.RecordQueryPlan
	}{
		{name: "scan", plan: scan},
		{name: "index", plan: index},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalized, changed, err := normalizeCorrelatedScanComparisonPlan(
				test.plan, alias, physicalRoot)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || normalized == test.plan {
				t.Fatal("name-only outer source difference did not rebuild comparison plan")
			}
			comparisonPlan, ok := normalized.(interface {
				GetScanComparisons() []*predicates.ComparisonRange
			})
			if !ok {
				t.Fatalf("normalized plan = %T, want comparison-bearing plan", normalized)
			}
			operand := comparisonPlan.GetScanComparisons()[0].GetEqualityComparison().Operand
			field, ok := values.AsFieldValue(operand)
			if !ok || field.ChildValue() != physicalRoot {
				t.Fatalf("normalized operand root = %T/%v, want exact physical root %p",
					operand, field, physicalRoot)
			}
			if path := field.Path().Ordinals(); len(path) != 1 || path[0] != 0 {
				t.Fatalf("normalized operand path = %v, want [0]", path)
			}
		})
	}

	// A polymorphic table scan retains its executable comparison below an exact
	// TypeFilter wrapper. The wrapper is transparent to the correlated operand;
	// rebuilding it must preserve its discriminator set and physical edge
	// identity while replacing only the comparison-bearing child.
	typeFilter := mustNLJConstruct(plans.NewRecordQueryTypeFilterPlan(
		[]string{"T4", "T4"}, scan))
	originalTypeFilterQ := typeFilter.GetQuantifiers()[0]
	normalizedTypeFilterPlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		typeFilter, alias, physicalRoot)
	if err != nil {
		t.Fatalf("normalize type-filtered probe: %v", err)
	}
	normalizedTypeFilter, ok := normalizedTypeFilterPlan.(*plans.RecordQueryTypeFilterPlan)
	if !changed || !ok || normalizedTypeFilter == typeFilter {
		t.Fatalf("type-filtered normalization = %T changed=%v",
			normalizedTypeFilterPlan, changed)
	}
	if got := normalizedTypeFilter.GetRecordTypes(); len(got) != 1 || got[0] != "T4" {
		t.Fatalf("normalized TypeFilter record types = %v, want [T4]", got)
	}
	normalizedTypeFilterQ := normalizedTypeFilter.GetQuantifiers()[0]
	if normalizedTypeFilterQ.GetAlias() != originalTypeFilterQ.GetAlias() ||
		normalizedTypeFilterQ.Kind() != originalTypeFilterQ.Kind() ||
		normalizedTypeFilterQ.GetRangesOver().Stage() != originalTypeFilterQ.GetRangesOver().Stage() {
		t.Fatal("normalized TypeFilter changed its quantifier alias, kind, or stage")
	}
	normalizedTypeScan, ok := normalizedTypeFilter.GetInner().(*plans.RecordQueryScanPlan)
	if !ok || normalizedTypeScan == scan {
		t.Fatalf("normalized TypeFilter child = %T, want rebuilt Scan",
			normalizedTypeFilter.GetInner())
	}
	typeFilterOperand := normalizedTypeScan.GetScanComparisons()[0].GetEqualityComparison().Operand
	typeFilterField, ok := values.AsFieldValue(typeFilterOperand)
	if !ok || typeFilterField.ChildValue() != physicalRoot {
		t.Fatalf("normalized type-filtered operand = %T/%v, want exact physical root %p",
			typeFilterOperand, typeFilterField, physicalRoot)
	}
	if typeFilter.GetInner() != scan ||
		typeFilter.GetQuantifiers()[0].GetAlias() != originalTypeFilterQ.GetAlias() ||
		scan.GetScanComparisons()[0].GetEqualityComparison().Operand != logicalID {
		t.Fatal("type-filtered normalization mutated the source wrapper, edge, or scan")
	}

	// A gathered outer join binds its whole row under a synthetic alias while
	// retaining U as an exact source window. A correlated scan below the FlatMap
	// still reads U, so the selected window — not the synthetic whole-row alias —
	// is the authority which must normalize U's nominal logical record name.
	outerLayout, err := values.NewOrdinalLayoutForCarrierType(
		physicalType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{{
			Source: physicalRoot, FieldPaths: [][]int{{0}, {1}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	boxAlias := values.NamedCorrelationIdentifier("U$BOX")
	boxRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(boxAlias, physicalType))
	windowNormalizedPlan, changed, err := normalizeCorrelatedScanComparisonPlanForOuterLayout(
		scan, boxAlias, boxRoot, outerLayout)
	if err != nil {
		t.Fatalf("normalize selected retained source: %v", err)
	}
	windowNormalizedScan, ok := windowNormalizedPlan.(*plans.RecordQueryScanPlan)
	if !changed || !ok || windowNormalizedScan == scan {
		t.Fatalf("retained-source normalization = %T changed=%v",
			windowNormalizedPlan, changed)
	}
	windowOperand := windowNormalizedScan.GetScanComparisons()[0].GetEqualityComparison().Operand
	windowField, ok := values.AsFieldValue(windowOperand)
	if !ok || windowField.ChildValue() != physicalRoot {
		t.Fatalf("retained-source operand = %T/%v, want exact window root %p",
			windowOperand, windowField, physicalRoot)
	}
	if scan.GetScanComparisons()[0].GetEqualityComparison().Operand != logicalID {
		t.Fatal("retained-source normalization mutated the source scan comparison")
	}

	// A clustered selected inner is itself a FlatMap. The enclosing PA/U
	// correlation can be consumed by a scan below either retained leg; the
	// normalization must cross that producer without replacing its runtime
	// aliases or mutating either selected child.
	bAlias := values.NamedCorrelationIdentifier("B")
	cAlias := values.NamedCorrelationIdentifier("C")
	bRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(bAlias, scan.GetResultType()))
	cScan := nljPhysicalScan("C")
	cRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(cAlias, cScan.GetResultType()))
	clusteredResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name: "B_ID", Value: mustNLJConstruct(values.ResolveFieldOrdinals(bRoot, []int{0})),
		},
		values.RecordConstructorField{
			Name: "C_ID", Value: mustNLJConstruct(values.ResolveFieldOrdinals(cRoot, []int{0})),
		},
	)
	clustered := mustNLJConstruct(plans.NewRecordQueryFlatMapPlan(
		scan, cScan, bAlias, cAlias, clusteredResult, false))
	clusteredNormalizedPlan, changed, err := normalizeCorrelatedScanComparisonPlanForOuterLayout(
		clustered, boxAlias, boxRoot, outerLayout)
	if err != nil {
		t.Fatalf("normalize retained source below clustered FlatMap: %v", err)
	}
	clusteredNormalized, ok := clusteredNormalizedPlan.(*plans.RecordQueryFlatMapPlan)
	if !changed || !ok || clusteredNormalized == clustered {
		t.Fatalf("clustered normalization = %T changed=%v",
			clusteredNormalizedPlan, changed)
	}
	if clusteredNormalized.GetOuterAlias() != bAlias ||
		clusteredNormalized.GetInnerAlias() != cAlias ||
		clusteredNormalized.GetInner() != cScan {
		t.Fatal("clustered normalization changed runtime aliases or the untouched inner leg")
	}
	clusteredScan, ok := clusteredNormalized.GetOuter().(*plans.RecordQueryScanPlan)
	if !ok || clusteredScan == scan {
		t.Fatalf("clustered normalized outer = %T, want rebuilt Scan",
			clusteredNormalized.GetOuter())
	}
	clusteredOperand := clusteredScan.GetScanComparisons()[0].GetEqualityComparison().Operand
	clusteredField, ok := values.AsFieldValue(clusteredOperand)
	if !ok || clusteredField.ChildValue() != physicalRoot {
		t.Fatalf("clustered retained-source operand = %T/%v, want exact window root %p",
			clusteredOperand, clusteredField, physicalRoot)
	}
	if clustered.GetOuter() != scan || scan.GetScanComparisons()[0].GetEqualityComparison().Operand != logicalID {
		t.Fatal("clustered normalization mutated the source FlatMap or scan")
	}

	foreignWindowRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN_U"), physicalType))
	foreignLayout, err := values.NewOrdinalLayoutForCarrierType(
		physicalType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{{
			Source: foreignWindowRoot, FieldPaths: [][]int{{0}, {1}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	foreignWindowPlan, changed, err := normalizeCorrelatedScanComparisonPlanForOuterLayout(
		scan, boxAlias, boxRoot, foreignLayout)
	if err != nil || changed || foreignWindowPlan != scan {
		t.Fatalf("foreign retained source changed: plan=%T changed=%v err=%v",
			foreignWindowPlan, changed, err)
	}

	driftedWindowType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullString},
		{Name: "V", FieldType: values.NullableLong},
	})
	driftedWindowRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, driftedWindowType))
	driftedLayout, err := values.NewOrdinalLayoutForCarrierType(
		driftedWindowType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{{
			Source: driftedWindowRoot, FieldPaths: [][]int{{0}, {1}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	driftedBoxRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(boxAlias, driftedWindowType))
	driftedWindowPlan, changed, err := normalizeCorrelatedScanComparisonPlanForOuterLayout(
		scan, boxAlias, driftedBoxRoot, driftedLayout)
	if err != nil || changed || driftedWindowPlan != scan {
		t.Fatalf("retained exact-type drift changed: plan=%T changed=%v err=%v",
			driftedWindowPlan, changed, err)
	}

	filter := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		scan,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		values.NamedCorrelationIdentifier("INNER")))
	normalizedFilterPlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		filter, alias, physicalRoot)
	if err != nil {
		t.Fatalf("normalize residual-filter probe: %v", err)
	}
	normalizedFilter, ok := normalizedFilterPlan.(*plans.RecordQueryPredicatesFilterPlan)
	if !changed || !ok || normalizedFilter == filter {
		t.Fatalf("residual-filter normalization = %T changed=%v", normalizedFilterPlan, changed)
	}
	normalizedScan, ok := normalizedFilter.GetInner().(*plans.RecordQueryScanPlan)
	if !ok {
		t.Fatalf("normalized residual-filter child = %T, want Scan", normalizedFilter.GetInner())
	}
	filteredOperand := normalizedScan.GetScanComparisons()[0].GetEqualityComparison().Operand
	filteredField, ok := values.AsFieldValue(filteredOperand)
	if !ok || filteredField.ChildValue() != physicalRoot {
		t.Fatalf("normalized residual-filter operand = %T/%v, want exact physical root %p",
			filteredOperand, filteredField, physicalRoot)
	}
	if filter.GetInner() != scan {
		t.Fatal("residual-filter normalization mutated the source child")
	}

	// A correlated index probe can remain below Covering, a residual Filter,
	// and Fetch. Fetch is transparent to the comparison program, but it carries
	// independent metadata which the normalizer must preserve copy-on-write.
	covering := mustNLJConstruct(plans.NewRecordQueryCoveringIndexPlan(index))
	fetchFilterAlias := values.NamedCorrelationIdentifier("FETCH_INNER")
	fetchFilter := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		covering,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		fetchFilterAlias))
	fetchResultType := nljSimpleRowType("FETCH_RESULT")
	translateMarker := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	translate := func(
		_ values.Value,
		_, _ values.CorrelationIdentifier,
	) (values.Value, bool) {
		return translateMarker, true
	}
	fetch := mustNLJConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		fetchFilter,
		translate,
		fetchResultType,
		plans.FetchIndexRecordsSyntheticConstituents))
	normalizedFetchPlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		fetch, alias, physicalRoot)
	if err != nil {
		t.Fatalf("normalize fetched covering probe: %v", err)
	}
	normalizedFetch, ok := normalizedFetchPlan.(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !changed || !ok || normalizedFetch == fetch {
		t.Fatalf("fetched probe normalization = %T changed=%v", normalizedFetchPlan, changed)
	}
	if normalizedFetch.GetResultType() != fetchResultType ||
		normalizedFetch.GetFetchIndexRecords() != plans.FetchIndexRecordsSyntheticConstituents {
		t.Fatal("fetched probe normalization changed its result contract or fetch mode")
	}
	if translated, accepted := normalizedFetch.PushValue(
		logicalID, alias, values.NamedCorrelationIdentifier("TARGET")); !accepted || translated != translateMarker {
		t.Fatal("fetched probe normalization changed its translate function")
	}
	normalizedFetchFilter, ok := normalizedFetch.GetInner().(*plans.RecordQueryPredicatesFilterPlan)
	if !ok || normalizedFetchFilter == fetchFilter ||
		normalizedFetchFilter.GetInnerAlias() != fetchFilterAlias {
		t.Fatalf("normalized Fetch child = %T, want rebuilt Filter with preserved alias",
			normalizedFetch.GetInner())
	}
	normalizedCovering, ok := normalizedFetchFilter.GetInner().(*plans.RecordQueryCoveringIndexPlan)
	if !ok || normalizedCovering == covering {
		t.Fatalf("normalized Filter child = %T, want rebuilt CoveringIndexScan",
			normalizedFetchFilter.GetInner())
	}
	normalizedIndex, ok := plans.IndexPlanOf(normalizedCovering)
	if !ok {
		t.Fatalf("normalized covering child = %T, want IndexPlan", normalizedCovering)
	}
	fetchedOperand := normalizedIndex.GetScanComparisons()[0].GetEqualityComparison().Operand
	fetchedField, ok := values.AsFieldValue(fetchedOperand)
	if !ok || fetchedField.ChildValue() != physicalRoot {
		t.Fatalf("normalized fetched operand = %T/%v, want exact physical root %p",
			fetchedOperand, fetchedField, physicalRoot)
	}
	if fetch.GetInner() != fetchFilter || fetchFilter.GetInner() != covering ||
		covering.GetIndexPlan() != index || equality.Operand != logicalID {
		t.Fatal("fetched probe normalization mutated its source wrapper chain")
	}

	foreignFetchPlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		fetch, values.NamedCorrelationIdentifier("FOREIGN"), physicalRoot)
	if err != nil || changed || foreignFetchPlan != fetch {
		t.Fatalf("foreign fetched source changed: plan=%T changed=%v err=%v",
			foreignFetchPlan, changed, err)
	}
	fetchTypeDrift := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullString},
		{Name: "V", FieldType: values.NullableLong},
	})
	fetchTypeDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, fetchTypeDrift))
	typeDriftFetchPlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		fetch, alias, fetchTypeDriftRoot)
	if err != nil || changed || typeDriftFetchPlan != fetch {
		t.Fatalf("fetched exact-type drift changed: plan=%T changed=%v err=%v",
			typeDriftFetchPlan, changed, err)
	}

	// A selected FlatMap binds its outer row under alias U using the exact
	// physical carrier. The correlated predicate is executable state owned by
	// the inner filter, not by the scan comparison below it, so it must be
	// normalized even when the child itself has nothing to rewrite.
	plainInner := nljPhysicalScan("PLAIN_INNER")
	outerPredicate := predicates.NewComparisonPredicate(
		logicalID,
		predicates.Comparison{
			Type: predicates.ComparisonGreaterThan,
			Operand: &values.ConstantValue{
				Value: int64(0), Typ: values.NotNullLong,
			},
		})
	innerBinding := values.NamedCorrelationIdentifier("INNER_BINDING")
	predicateFilter := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		plainInner, []predicates.QueryPredicate{outerPredicate}, innerBinding))
	normalizedPredicatePlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		predicateFilter, alias, physicalRoot)
	if err != nil {
		t.Fatalf("normalize correlated residual predicate: %v", err)
	}
	normalizedPredicateFilter, ok := normalizedPredicatePlan.(*plans.RecordQueryPredicatesFilterPlan)
	if !changed || !ok || normalizedPredicateFilter == predicateFilter {
		t.Fatalf("predicate-only normalization = %T changed=%v", normalizedPredicatePlan, changed)
	}
	if normalizedPredicateFilter.GetInner() != plainInner ||
		normalizedPredicateFilter.GetInnerAlias() != innerBinding {
		t.Fatal("predicate-only normalization changed the selected child or local binding alias")
	}
	normalizedOuterPredicate, ok := normalizedPredicateFilter.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("normalized residual = %T, want ComparisonPredicate",
			normalizedPredicateFilter.GetPredicates()[0])
	}
	normalizedOuterField, ok := values.AsFieldValue(normalizedOuterPredicate.Operand)
	if !ok || normalizedOuterField.ChildValue() != physicalRoot {
		t.Fatalf("normalized residual root = %T/%v, want exact physical root %p",
			normalizedOuterPredicate.Operand, normalizedOuterField, physicalRoot)
	}
	if outerPredicate.Operand != logicalID || predicateFilter.GetPredicates()[0] != outerPredicate {
		t.Fatal("predicate-only normalization mutated the source predicate or filter")
	}

	foreignPredicatePlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		predicateFilter, values.NamedCorrelationIdentifier("FOREIGN"), physicalRoot)
	if err != nil || changed || foreignPredicatePlan != predicateFilter {
		t.Fatalf("foreign predicate source changed: plan=%T changed=%v err=%v",
			foreignPredicatePlan, changed, err)
	}
	predicateTypeDrift := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullString},
		{Name: "V", FieldType: values.NullableLong},
	})
	predicateTypeDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, predicateTypeDrift))
	typeDriftPredicatePlan, changed, err := normalizeCorrelatedScanComparisonPlan(
		predicateFilter, alias, predicateTypeDriftRoot)
	if err != nil || changed || typeDriftPredicatePlan != predicateFilter {
		t.Fatalf("predicate exact-type drift changed: plan=%T changed=%v err=%v",
			typeDriftPredicatePlan, changed, err)
	}

	if ranges[0].GetEqualityComparison() != equality || equality.Operand != logicalID {
		t.Fatal("normalization mutated the source comparison range")
	}
	foreign, changed, err := normalizeCorrelatedScanComparisonPlan(
		scan, values.NamedCorrelationIdentifier("FOREIGN"), physicalRoot)
	if err != nil || changed || foreign != scan {
		t.Fatalf("foreign alias changed scan: plan=%T changed=%v err=%v",
			foreign, changed, err)
	}
	typeDrift := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullString},
		{Name: "V", FieldType: values.NullableLong},
	})
	typeDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, typeDrift))
	drifted, changed, err := normalizeCorrelatedScanComparisonPlan(
		scan, alias, typeDriftRoot)
	if err != nil || changed || drifted != scan {
		t.Fatalf("exact leaf-type drift changed scan: plan=%T changed=%v err=%v",
			drifted, changed, err)
	}
	pathDrift := values.NewRecordType("", false, []values.Field{
		{Name: "OTHER", FieldType: values.NotNullLong},
		{Name: "V", FieldType: values.NullableLong},
	})
	pathDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, pathDrift))
	drifted, changed, err = normalizeCorrelatedScanComparisonPlan(
		scan, alias, pathDriftRoot)
	if err != nil || changed || drifted != scan {
		t.Fatalf("field-name/path drift changed scan: plan=%T changed=%v err=%v",
			drifted, changed, err)
	}
}

func mustNLJField(value values.Value) values.FieldValue {
	field, ok := values.AsFieldValue(value)
	if !ok {
		panic("construct nested-loop-join fixture: resolved value is not a FieldValue")
	}
	return field
}

func TestCorrelationForSourceAliasPreservesExactQuantifierIdentity(t *testing.T) {
	t.Parallel()
	unique := values.UniqueCorrelationIdentifier()
	if namedTwin := values.NamedCorrelationIdentifier(unique.Name()); namedTwin == unique {
		t.Fatal("test requires rendered-equal Unique and Named identifiers to remain distinct")
	}
	if got := correlationForSourceAlias(unique, unique.Name()); got != unique {
		t.Fatalf("matching source spelling reconstructed exact alias as %v, want original Unique %v", got, unique)
	}
	if got := correlationForSourceAlias(unique, ""); got != unique {
		t.Fatalf("empty source metadata changed exact alias to %v, want %v", got, unique)
	}
	if got := correlationForSourceAlias(unique, "INNER"); got != values.NamedCorrelationIdentifier("INNER") {
		t.Fatalf("distinct source alias = %v, want explicit named INNER binding", got)
	}
}

func TestCollisionFreeExistentialOuterCorrelationSeparatesRetainedScalar(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("T")
	scalarAlias := values.NamedCorrelationIdentifier("VAL")
	outer := nljPhysicalScan("T")
	outerQ := expressions.NamedPhysicalQuantifier(
		outerAlias, expressions.FinalOfAtStage(outer, expressions.StageCanonical))
	outerRoot := nljFlowed(outerQ)
	explode := mustNLJConstruct(plans.NewRecordQueryExplodePlan(
		values.NewArrayConstructorValue(values.NotNullInt, []values.Value{
			&values.ConstantValue{Value: 7, Typ: values.NotNullInt},
		})))
	scalarQ := expressions.NamedPhysicalQuantifier(
		scalarAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	scalarRoot := mustNLJConstruct(scalarQ.RequireFlowedObjectValue())
	flat := mustNLJConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, scalarQ, outerAlias, scalarAlias,
		values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "_0", Value: outerRoot},
			values.RecordConstructorField{Name: "_1", Value: scalarRoot}),
		false))
	layout, err := flat.ProvidedOutputLayout()
	if err != nil {
		t.Fatal(err)
	}
	if provided, provideErr := values.LayoutProvides(layout, scalarRoot); provideErr != nil || !provided {
		t.Fatalf("setup scalar window = (%t, %v), windows=%v want exact retained source",
			provided, provideErr, layout.WindowSources())
	}

	fresh, err := collisionFreeExistentialOuterCorrelation(nil, flat, scalarAlias)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IsZero() || fresh == scalarAlias || fresh.Name() == scalarAlias.Name() {
		t.Fatalf("collision-free whole-row alias = %v, want fresh identity distinct from VAL", fresh)
	}
	foreign := values.NamedCorrelationIdentifier("FOREIGN")
	unchanged, err := collisionFreeExistentialOuterCorrelation(nil, flat, foreign)
	if err != nil || unchanged != foreign {
		t.Fatalf("foreign candidate = (%v, %v), want pointer-stable identity", unchanged, err)
	}
	if provided, provideErr := values.LayoutProvides(layout, scalarRoot); provideErr != nil || !provided {
		t.Fatalf("collision check mutated source layout: (%t, %v)", provided, provideErr)
	}
}

func TestSelectedExistentialOuterLayoutAuthorityRebuildsLiveMiddleFlatMap(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("T")
	elementAlias := values.NamedCorrelationIdentifier("X")

	outer := nljPhysicalScan("T")
	element := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{
			Name: "EK",
			Value: &values.ConstantValue{
				Typ: values.NotNullLong, Value: int64(101),
			},
		},
		values.RecordConstructorField{
			Name: "LABEL",
			Value: &values.ConstantValue{
				Typ: values.NullableString, Value: "element",
			},
		},
	)
	element.SetTypeName("ELEM")
	elements := values.NewArrayConstructorValue(
		element.Type(), []values.Value{element})
	explode := mustNLJConstruct(plans.NewRecordQueryExplodePlan(elements))

	// Construct the middle FlatMap over live memo groups before either child
	// has a selected physical winner. Its immutable layout therefore cannot
	// claim X even though the result program directly retains X as one record.
	outerRef := expressions.InitialOf(outer)
	elementRef := expressions.InitialOf(explode)
	outerQ := expressions.NamedPhysicalQuantifier(outerAlias, outerRef)
	elementQ := expressions.NamedPhysicalQuantifier(elementAlias, elementRef)
	outerRoot := mustNLJConstruct(outerQ.RequireFlowedObjectValue())
	elementRoot := mustNLJConstruct(elementQ.RequireFlowedObjectValue())
	resultValue := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "OUTER", Value: outerRoot},
		values.RecordConstructorField{Name: "X", Value: elementRoot},
	)
	middle := mustNLJConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, elementQ, outerAlias, elementAlias, resultValue, false))
	originalLayout := mustNLJConstruct(middle.ProvidedOutputLayout())
	if provided, _ := values.LayoutProvides(originalLayout, elementRoot); provided {
		t.Fatal("live middle FlatMap unexpectedly published X before child selection")
	}

	// Stamping the same child members makes the selected one-level clone a
	// private extraction-equivalent authority. The live reference and member
	// remain untouched.
	outerRef.SetWinner(outer)
	elementRef.SetWinner(explode)
	elementKey := mustNLJField(mustNLJConstruct(
		values.ResolveFieldOrdinals(elementRoot, []int{0})))
	predicate := predicates.NewComparisonPredicate(
		elementKey, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	call := NewExpressionRuleCall(expressions.InitialOf(middle), nil, EmptyPlanContext())
	rebuilt, changed, err := selectedExistentialOuterLayoutAuthority(
		call, middle, []predicates.QueryPredicate{predicate}, nil)
	if err != nil {
		t.Fatalf("selected outer layout authority: %v", err)
	}
	selected, ok := rebuilt.(*plans.RecordQueryFlatMapPlan)
	if !changed || !ok || selected == middle {
		t.Fatalf("selected outer = %T changed=%v, want detached rebuilt FlatMap", rebuilt, changed)
	}
	selectedLayout := mustNLJConstruct(selected.ProvidedOutputLayout())
	if provided, provideErr := values.LayoutProvides(selectedLayout, elementRoot); provideErr != nil || !provided {
		t.Fatalf("selected clone X/ELEM window = (%t, %v), windows=%v",
			provided, provideErr, selectedLayout.WindowSources())
	}
	fresh, err := collisionFreeExistentialOuterCorrelation(nil, selected, elementAlias)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IsZero() || fresh == elementAlias || fresh.Name() == elementAlias.Name() {
		t.Fatalf("selected X/ELEM collision alias = %v, want fresh identity", fresh)
	}

	if provided, _ := values.LayoutProvides(originalLayout, elementRoot); provided {
		t.Fatal("selected rebuild mutated the live middle FlatMap layout")
	}
	quantifiers := middle.GetQuantifiers()
	if len(quantifiers) != 2 || quantifiers[0].GetRangesOver() != outerRef ||
		quantifiers[1].GetRangesOver() != elementRef {
		t.Fatal("selected rebuild replaced a live middle FlatMap edge")
	}
	if resultValue.Fields[1].Value != elementRoot || elementKey.ChildValue() != elementRoot {
		t.Fatal("selected rebuild mutated the source result or predicate root")
	}
}

func TestProjectedRetainedSourceSelectsAndNormalizesLateOuterLayout(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("T")
	elementAlias := values.NamedCorrelationIdentifier("X")
	wholeAlias := values.NamedCorrelationIdentifier("OUTER")

	outer := nljPhysicalScan("T")
	element := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{
			Name: "EK",
			Value: &values.ConstantValue{
				Typ: values.NotNullLong, Value: int64(101),
			},
		},
		values.RecordConstructorField{
			Name: "LABEL",
			Value: &values.ConstantValue{
				Typ: values.NullableString, Value: "element",
			},
		},
	)
	element.SetTypeName("ELEM")
	exactElementType, ok := element.Type().(*values.RecordType)
	if !ok {
		t.Fatalf("element type = %T, want concrete record", element.Type())
	}
	explode := mustNLJConstruct(plans.NewRecordQueryExplodePlan(
		values.NewArrayConstructorValue(element.Type(), []values.Value{element})))

	outerRef := expressions.InitialOf(outer)
	elementRef := expressions.InitialOf(explode)
	outerQ := expressions.NamedPhysicalQuantifier(outerAlias, outerRef)
	elementQ := expressions.NamedPhysicalQuantifier(elementAlias, elementRef)
	outerRoot := mustNLJConstruct(outerQ.RequireFlowedObjectValue())
	elementRoot := mustNLJConstruct(elementQ.RequireFlowedObjectValue())
	middleResult := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "OUTER", Value: outerRoot},
		values.RecordConstructorField{Name: "X", Value: elementRoot},
	)
	middle := mustNLJConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, elementQ, outerAlias, elementAlias, middleResult, false))
	originalLayout := mustNLJConstruct(middle.ProvidedOutputLayout())
	if provided, _ := values.LayoutProvides(originalLayout, elementRoot); provided {
		t.Fatal("live projected-only fixture unexpectedly publishes X before selection")
	}

	// X occurs nowhere in a predicate or inner plan. Its sole executable use is
	// the projected result, whose logical RECORD declaration must select the
	// late X/ELEM layout and then normalize onto that exact window.
	logicalElementType := values.NewRecordType(
		"RECORD", false, exactElementType.Fields)
	logicalElementRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, logicalElementType))
	logicalElementKey := mustNLJConstruct(values.ResolveFieldOrdinals(
		logicalElementRoot, []int{0}))
	projectedResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "EK", Value: logicalElementKey})

	outerRef.SetWinner(outer)
	elementRef.SetWinner(explode)
	call := NewExpressionRuleCall(expressions.InitialOf(middle), nil, EmptyPlanContext())
	rebuilt, changed, err := selectedExistentialOuterLayoutAuthority(
		call, middle, nil, nil, projectedResult)
	if err != nil {
		t.Fatalf("projected-only selected layout: %v", err)
	}
	selected, ok := rebuilt.(*plans.RecordQueryFlatMapPlan)
	if !changed || !ok || selected == middle {
		t.Fatalf("projected-only selected outer = %T changed=%v, want detached clone",
			rebuilt, changed)
	}
	selectedLayout := mustNLJConstruct(selected.ProvidedOutputLayout())
	if provided, provideErr := values.LayoutProvides(selectedLayout, elementRoot); provideErr != nil || !provided {
		t.Fatalf("projected-only selected X/ELEM = (%t, %v), windows=%v",
			provided, provideErr, selectedLayout.WindowSources())
	}
	var selectedElementSource values.QuantifiedObjectValue
	for _, source := range selectedLayout.WindowSources() {
		if source.Correlation() == elementAlias &&
			source.FlowedType().Equals(elementRoot.FlowedType()) {
			if selectedElementSource != nil {
				t.Fatal("selected projected-only layout publishes ambiguous X/ELEM windows")
			}
			selectedElementSource = source
		}
	}
	if selectedElementSource == nil {
		t.Fatal("selected projected-only layout has no exact X/ELEM window source")
	}
	physicalWhole := mustNLJConstruct(values.NewQuantifiedObjectValue(
		wholeAlias, selectedLayout.Carrier().FlowedType()))
	normalized, err := normalizeCorrelatedValueForOuterLayout(
		projectedResult, wholeAlias, physicalWhole, selectedLayout)
	if err != nil {
		t.Fatalf("normalize projected X.EK: %v", err)
	}
	normalizedRecord, ok := normalized.(*values.RecordConstructorValue)
	if !ok || normalizedRecord == projectedResult || len(normalizedRecord.Fields) != 1 {
		t.Fatalf("normalized projected result = %T/%p, want rebuilt one-slot record",
			normalized, normalized)
	}
	normalizedKey, ok := values.AsFieldValue(normalizedRecord.Fields[0].Value)
	if !ok || normalizedKey.ChildValue() != selectedElementSource {
		t.Fatalf("normalized projected key = %T/%v, want exact X/ELEM root %p",
			normalizedRecord.Fields[0].Value, normalizedKey, selectedElementSource)
	}
	if path := normalizedKey.Path().Ordinals(); len(path) != 1 || path[0] != 0 {
		t.Fatalf("normalized projected path = %v, want [0]", path)
	}
	if !existentialProgramsRequireRetainedOuterLayout(
		nil, nil, selectedLayout, normalized) {
		t.Fatal("projected X.EK alone did not make the selected layout load-bearing")
	}

	foreignRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN"), logicalElementType))
	foreignKey := mustNLJConstruct(values.ResolveFieldOrdinals(foreignRoot, []int{0}))
	foreignResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "EK", Value: foreignKey})
	foreignSelected, foreignChanged, err := selectedExistentialOuterLayoutAuthority(
		call, middle, nil, nil, foreignResult)
	if err != nil || foreignChanged || foreignSelected != middle {
		t.Fatalf("foreign projected source selected outer = %T changed=%v err=%v",
			foreignSelected, foreignChanged, err)
	}
	foreignNormalized, err := normalizeCorrelatedValueForOuterLayout(
		foreignResult, wholeAlias, physicalWhole, selectedLayout)
	if err != nil || foreignNormalized != foreignResult ||
		existentialProgramsRequireRetainedOuterLayout(
			nil, nil, selectedLayout, foreignNormalized) {
		t.Fatalf("foreign projected source = normalized %v dependent=%v err=%v, want unchanged/nondependent",
			foreignNormalized != foreignResult,
			existentialProgramsRequireRetainedOuterLayout(nil, nil, selectedLayout, foreignNormalized), err)
	}

	typeDriftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, values.NewRecordType("RECORD", false, []values.Field{
			{Name: "EK", FieldType: values.NotNullString},
			{Name: "LABEL", FieldType: values.NullableString},
		})))
	typeDriftKey := mustNLJConstruct(values.ResolveFieldOrdinals(typeDriftRoot, []int{0}))
	typeDriftResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "EK", Value: typeDriftKey})
	typeDriftNormalized, err := normalizeCorrelatedValueForOuterLayout(
		typeDriftResult, wholeAlias, physicalWhole, selectedLayout)
	if err != nil || typeDriftNormalized != typeDriftResult {
		t.Fatalf("projected exact-type drift normalized=%v err=%v, want unchanged",
			typeDriftNormalized != typeDriftResult, err)
	}
	if !existentialProgramsRequireRetainedOuterLayout(
		nil, nil, selectedLayout, typeDriftResult) {
		t.Fatal("same-correlation type drift was not conservatively pinned")
	}

	if projectedResult.Fields[0].Value != logicalElementKey {
		t.Fatal("projected-only normalization mutated the source result program")
	}
	originalKey, ok := values.AsFieldValue(projectedResult.Fields[0].Value)
	if !ok || originalKey.ChildValue() != logicalElementRoot {
		t.Fatal("projected-only normalization mutated the logical X/RECORD root")
	}
	if provided, _ := values.LayoutProvides(originalLayout, elementRoot); provided {
		t.Fatal("projected-only selection mutated the live middle layout")
	}
}

func TestAdmitCorrelatedFastPathOuterValueRequiresOneExactLayoutOwner(t *testing.T) {
	t.Parallel()
	elementAlias := values.NamedCorrelationIdentifier("X")
	wholeAlias := values.NamedCorrelationIdentifier("OUTER")
	detailType := values.NewRecordType("DETAIL", false, []values.Field{
		{Name: "DK", FieldType: values.NullableLong},
	})
	elementType := values.NewRecordType("ELEM", false, []values.Field{
		{Name: "EK", FieldType: values.NotNullLong},
		{Name: "DETAIL", FieldType: detailType},
	})
	logicalElementType := values.NewRecordType("RECORD", false, elementType.Fields)
	elementRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, elementType))
	logicalElementRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, logicalElementType))
	logicalElementKey := mustNLJField(mustNLJConstruct(
		values.ResolveFieldOrdinals(logicalElementRoot, []int{0})))

	layoutProgram := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{
			Name: "ID",
			Value: &values.ConstantValue{
				Typ: values.NotNullLong, Value: int64(1),
			},
		},
		values.RecordConstructorField{Name: "X", Value: elementRoot},
	)
	layout := mustNLJConstruct(
		values.NewFlatOrdinalLayoutForRetainedResult(layoutProgram, nil))
	wholeType := values.NewRecordType("OUTER_ROW", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "X", FieldType: elementType},
	})
	wholeBinding := mustNLJConstruct(values.NewQuantifiedObjectValue(
		wholeAlias, wholeType))

	normalized, correlation, retained, admitted, err := admitCorrelatedFastPathOuterValue(logicalElementKey, wholeBinding, layout)
	if err != nil {
		t.Fatalf("admit generic X/RECORD: %v", err)
	}
	if !admitted || !retained || correlation != elementAlias ||
		normalized == nil || normalized.ChildValue() != elementRoot {
		t.Fatalf("generic X/RECORD admission = (%v, %v, retained=%v, admitted=%v), want exact X/ELEM",
			normalized, correlation, retained, admitted)
	}
	if path := normalized.Path().Ordinals(); len(path) != 1 || path[0] != 0 {
		t.Fatalf("normalized element path = %v, want [0]", path)
	}

	wholeID := mustNLJField(mustNLJConstruct(
		values.ResolveFieldOrdinals(wholeBinding, []int{0})))
	normalizedWhole, wholeCorrelation, wholeRetained, wholeAdmitted, err := admitCorrelatedFastPathOuterValue(wholeID, wholeBinding, layout)
	if err != nil {
		t.Fatalf("admit exact whole row: %v", err)
	}
	if !wholeAdmitted || wholeRetained || wholeCorrelation != wholeAlias ||
		normalizedWhole == nil || normalizedWhole.ChildValue() != wholeBinding {
		t.Fatalf("whole-row admission = (%v, %v, retained=%v, admitted=%v), want exact whole binding",
			normalizedWhole, wholeCorrelation, wholeRetained, wholeAdmitted)
	}

	field := func(root values.QuantifiedObjectValue, path ...int) values.FieldValue {
		t.Helper()
		return mustNLJField(mustNLJConstruct(values.ResolveFieldOrdinals(root, path)))
	}
	assertDeclines := func(
		name string,
		operand values.FieldValue,
		binding values.QuantifiedObjectValue,
	) {
		t.Helper()
		got, gotCorrelation, gotRetained, gotAdmitted, gotErr := admitCorrelatedFastPathOuterValue(operand, binding, layout)
		if gotErr != nil || got != nil || !gotCorrelation.IsZero() ||
			gotRetained || gotAdmitted {
			t.Fatalf("%s admission = (%v, %v, retained=%v, admitted=%v, err=%v), want clean decline",
				name, got, gotCorrelation, gotRetained, gotAdmitted, gotErr)
		}
	}

	foreignRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN"), logicalElementType))
	assertDeclines("foreign alias", field(foreignRoot, 0), wholeBinding)
	widthDrift := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, values.NewRecordType("RECORD", false, []values.Field{
			{Name: "EK", FieldType: values.NotNullLong},
		})))
	assertDeclines("width drift", field(widthDrift, 0), wholeBinding)
	leafDrift := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, values.NewRecordType("RECORD", false, []values.Field{
			{Name: "EK", FieldType: values.NotNullString},
			{Name: "DETAIL", FieldType: detailType},
		})))
	assertDeclines("leaf drift", field(leafDrift, 0), wholeBinding)
	nullabilityDrift := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, values.NewRecordType("RECORD", true, elementType.Fields)))
	assertDeclines("record nullability drift", field(nullabilityDrift, 0), wholeBinding)
	nestedDriftType := values.NewRecordType("DETAIL", false, []values.Field{
		{Name: "DK", FieldType: values.NullableString},
	})
	nestedPathDrift := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, values.NewRecordType("RECORD", false, []values.Field{
			{Name: "EK", FieldType: values.NotNullLong},
			{Name: "DETAIL", FieldType: nestedDriftType},
		})))
	assertDeclines("nested path drift", field(nestedPathDrift, 1, 0), wholeBinding)

	// The whole binding and retained window intentionally use the same rendered
	// source alias and are each name-only compatible with the generic request.
	// Neither can win without guessing, so the shortcut must decline.
	ambiguousWholeType := values.NewRecordType(
		"WHOLE_X", false, elementType.Fields)
	ambiguousWhole := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, ambiguousWholeType))
	assertDeclines("ambiguous whole/window owner", logicalElementKey, ambiguousWhole)

	if logicalElementKey.ChildValue() != logicalElementRoot {
		t.Fatal("admission mutated the generic X/RECORD operand")
	}
	if path := logicalElementKey.Path().Ordinals(); len(path) != 1 || path[0] != 0 {
		t.Fatalf("source operand path mutated to %v", path)
	}
	if provided, provideErr := values.LayoutProvides(layout, elementRoot); provideErr != nil || !provided {
		t.Fatalf("admission mutated exact X/ELEM layout source: (%t, %v)", provided, provideErr)
	}
}

func TestRetainedWindowExistentialPinsOuterAgainstOrderedAlternativeRecovery(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("T")
	elementAlias := values.NamedCorrelationIdentifier("X")
	innerAlias := values.NamedCorrelationIdentifier("U")
	outerType := nljSimpleRowType("T")

	orderedOuter := mustNLJConstruct(plans.NewRecordQueryIndexPlan(
		"IDX_T_ID", nil, []string{"T"}, outerType, false,
	)).WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"ID"}, nil, false)
	unorderedOuter := mustNLJConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, outerType, false))
	element := values.NewRawRecordConstructorValue(values.RecordConstructorField{
		Name: "EK",
		Value: &values.ConstantValue{
			Typ: values.NotNullLong, Value: int64(101),
		},
	})
	element.SetTypeName("ELEM")
	elements := values.NewArrayConstructorValue(element.Type(), []values.Value{element})
	explode := mustNLJConstruct(plans.NewRecordQueryExplodePlan(elements))

	// This alternative was built over live children and therefore has the
	// correct result type but no exact retained X window.
	liveOuterChild := expressions.InitialOf(unorderedOuter)
	liveElementChild := expressions.InitialOf(explode)
	liveOuterQ := expressions.NamedPhysicalQuantifier(outerAlias, liveOuterChild)
	liveElementQ := expressions.NamedPhysicalQuantifier(elementAlias, liveElementChild)
	liveOuterRoot := mustNLJConstruct(liveOuterQ.RequireFlowedObjectValue())
	liveElementRoot := mustNLJConstruct(liveElementQ.RequireFlowedObjectValue())
	liveOuterID := mustNLJConstruct(values.ResolveFieldOrdinals(liveOuterRoot, []int{0}))
	resultValue := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: liveOuterID},
		values.RecordConstructorField{Name: "X", Value: liveElementRoot},
	)
	missingWindow := mustNLJConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		liveOuterQ, liveElementQ, outerAlias, elementAlias, resultValue, true))
	missingLayout := mustNLJConstruct(missingWindow.ProvidedOutputLayout())
	if provided, _ := values.LayoutProvides(missingLayout, liveElementRoot); provided {
		t.Fatal("live alternative unexpectedly publishes X/ELEM")
	}

	// The selected sibling is extraction-equivalent but ranges over detached
	// physical children, so it proves the exact X/ELEM source window.
	selectedOuterQ := expressions.NamedPhysicalQuantifier(
		outerAlias, expressions.FinalOfAtStage(orderedOuter, expressions.StageCanonical))
	selectedElementQ := expressions.NamedPhysicalQuantifier(
		elementAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	selectedOuter := mustNLJConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		selectedOuterQ, selectedElementQ,
		outerAlias, elementAlias, resultValue, true))
	selectedLayout := mustNLJConstruct(selectedOuter.ProvidedOutputLayout())
	if provided, provideErr := values.LayoutProvides(selectedLayout, liveElementRoot); provideErr != nil || !provided {
		t.Fatalf("selected alternative X/ELEM = (%t, %v), windows=%v",
			provided, provideErr, selectedLayout.WindowSources())
	}

	// Put both alternatives in the live equivalence group and stamp the exact
	// one as its current winner. MemoizeExpression would return this replaceable
	// two-member edge; the retained-window proof must instead force a private
	// final singleton into the yielded existential FlatMap.
	liveOuterRef := expressions.InitialOf(selectedOuter)
	if !liveOuterRef.Insert(missingWindow) {
		t.Fatal("setup did not retain the missing-window outer alternative")
	}
	liveOuterRef.SetWinner(selectedOuter)
	if len(liveOuterRef.AllMembers()) != 2 {
		t.Fatalf("live outer group has %d alternatives, want 2", len(liveOuterRef.AllMembers()))
	}
	memo := NewMemo(nil)
	memo.RegisterReference(liveOuterRef)
	callRef := expressions.InitialOf(nljPhysicalScan("CALL_ROOT"))
	memo.RegisterReference(callRef)
	context := nljPrimaryKeyPlanContext{
		PlanContext: EmptyPlanContext(), primaryKey: []string{"ID"},
	}
	call := NewExpressionRuleCallWithMemo(callRef, nil, context, memo)
	if got := call.MemoizeExpression(selectedOuter); got != liveOuterRef {
		t.Fatal("setup: selected outer did not resolve to the two-alternative live group")
	}

	wholeBinding := mustNLJConstruct(values.NewQuantifiedObjectValue(
		elementAlias, selectedLayout.Carrier().FlowedType()))
	innerType := nljSimpleRowType("U")
	innerScan := mustNLJConstruct(plans.NewRecordQueryScanPlan(
		[]string{"U"}, innerType, false))
	innerRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(innerAlias, innerType))
	innerID := mustNLJConstruct(values.ResolveFieldOrdinals(innerRoot, []int{0}))
	elementKey := mustNLJConstruct(values.ResolveFieldOrdinals(liveElementRoot, []int{0}))
	joinPredicate := predicates.NewComparisonPredicate(
		elementKey,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: innerID},
	)
	rule := NewImplementNestedLoopJoinRule()
	if !rule.tryExistsFlatMap(
		call,
		wholeBinding,
		selectedOuter,
		innerScan,
		elementAlias,
		elementAlias,
		innerAlias,
		wholeBinding,
		selectedLayout,
		false,
		false,
		selectedOuter,
		innerScan,
		false,
		false,
		[]predicates.QueryPredicate{joinPredicate},
	) {
		t.Fatal("retained-window primary-key fast path declined the mutation fixture")
	}
	if err := call.Err(); err != nil {
		t.Fatalf("retained-window fast path: %v", err)
	}
	yielded := call.Yielded()
	if len(yielded) != 1 {
		t.Fatalf("retained-window fast path yielded %d plans, want 1", len(yielded))
	}
	existsFlatMap, ok := yielded[0].(*plans.RecordQueryFlatMapPlan)
	if !ok {
		t.Fatalf("retained-window fast path yielded %T, want FlatMap", yielded[0])
	}
	existsQuantifiers := existsFlatMap.GetQuantifiers()
	if len(existsQuantifiers) != 2 {
		t.Fatalf("existential FlatMap has %d quantifiers, want 2", len(existsQuantifiers))
	}
	pinnedOuterRef := existsQuantifiers[0].GetRangesOver()
	if pinnedOuterRef == nil {
		t.Fatal("yielded existential FlatMap has no outer edge")
	}
	pinnedMembers := pinnedOuterRef.Members()
	pinnedFinals := pinnedOuterRef.FinalMembers()
	if pinnedOuterRef == liveOuterRef || len(pinnedMembers) != 0 ||
		len(pinnedFinals) != 1 || pinnedFinals[0] != selectedOuter {
		t.Fatalf("yielded outer edge = members %d finals %d live=%v, want private selected Final singleton",
			len(pinnedMembers), len(pinnedFinals), pinnedOuterRef == liveOuterRef)
	}

	// Exercise the exact candidate collector used by late ImplementSort
	// recovery. The replaceable live group still contains both alternatives,
	// while the yielded edge can expose only the selected source authority.
	orderingResult := flatMapOrderingResultForChild(
		existsFlatMap, existsFlatMap.GetOuterAlias(), true)
	variants, err := collectJoinLegOrderingVariants(
		pinnedOuterRef,
		properties.PreserveOrdering(),
		orderingResult,
		existsFlatMap.GetOuterAlias(),
		lessWithHashTieBreak(call.CostModel()),
		false,
		context,
	)
	if err != nil {
		t.Fatalf("collect ordered outer variants: %v", err)
	}
	if len(variants) != 1 || variants[0].expr != selectedOuter {
		t.Fatalf("late ordering recovery saw %d variants, want only selected X/ELEM authority", len(variants))
	}
	if len(liveOuterRef.AllMembers()) != 2 {
		t.Fatal("pinning or ordered recovery mutated the original two-alternative group")
	}
	if provided, _ := values.LayoutProvides(missingLayout, liveElementRoot); provided {
		t.Fatal("pinning manufactured an X/ELEM window on the foreign alternative")
	}

	// Independently pin the classifier-driven arm: the PK match reads the
	// complete outer row (so retainedWindow=false), while a residual program
	// still reads X/ELEM. That residual makes the selected layout load-bearing
	// and must preserve the same private outer authority through the added
	// predicates-filter wrapper.
	wholeID := mustNLJConstruct(values.ResolveFieldOrdinals(wholeBinding, []int{0}))
	wholeMatch := predicates.NewComparisonPredicate(
		wholeID,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: innerID},
	)
	elementResidual := predicates.NewComparisonPredicate(
		elementKey, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	outerLayoutDependent := existentialProgramsRequireRetainedOuterLayout(
		[]predicates.QueryPredicate{elementResidual}, nil, selectedLayout)
	if !outerLayoutDependent {
		t.Fatal("X/ELEM residual did not make the existential depend on its selected outer layout")
	}
	foreignResidualRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN"), liveElementRoot.FlowedType()))
	foreignResidual := predicates.NewComparisonPredicate(
		mustNLJConstruct(values.ResolveFieldOrdinals(foreignResidualRoot, []int{0})),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	if existentialProgramsRequireRetainedOuterLayout(
		[]predicates.QueryPredicate{foreignResidual}, nil, selectedLayout) {
		t.Fatal("foreign residual claimed the selected X/ELEM layout window")
	}

	residualCallRef := expressions.InitialOf(nljPhysicalScan("CALL_ROOT_RESIDUAL"))
	memo.RegisterReference(residualCallRef)
	residualCall := NewExpressionRuleCallWithMemo(
		residualCallRef, nil, context, memo)
	if !rule.tryExistsFlatMap(
		residualCall,
		wholeBinding,
		selectedOuter,
		innerScan,
		elementAlias,
		elementAlias,
		innerAlias,
		wholeBinding,
		selectedLayout,
		false,
		outerLayoutDependent,
		selectedOuter,
		innerScan,
		false,
		false,
		[]predicates.QueryPredicate{wholeMatch, elementResidual},
	) {
		t.Fatal("whole-row PK fast path with retained-window residual declined")
	}
	if err := residualCall.Err(); err != nil {
		t.Fatalf("retained-window residual fast path: %v", err)
	}
	residualYielded := residualCall.Yielded()
	if len(residualYielded) != 1 {
		t.Fatalf("retained-window residual yielded %d plans, want 1", len(residualYielded))
	}
	residualFlatMap, ok := residualYielded[0].(*plans.RecordQueryFlatMapPlan)
	if !ok {
		t.Fatalf("retained-window residual yielded %T, want FlatMap", residualYielded[0])
	}
	residualOuterRef := residualFlatMap.GetQuantifiers()[0].GetRangesOver()
	if residualOuterRef == nil || residualOuterRef == liveOuterRef ||
		len(residualOuterRef.Members()) != 0 || len(residualOuterRef.FinalMembers()) != 1 {
		t.Fatal("retained-window residual did not freeze its filtered outer in a private Final edge")
	}
	outerFilter, ok := residualOuterRef.FinalMembers()[0].(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("retained-window residual outer = %T, want PredicatesFilter",
			residualOuterRef.FinalMembers()[0])
	}
	filterQuantifiers := outerFilter.GetQuantifiers()
	if len(filterQuantifiers) != 1 {
		t.Fatalf("outer residual filter has %d quantifiers, want 1", len(filterQuantifiers))
	}
	filterChildRef := filterQuantifiers[0].GetRangesOver()
	if filterChildRef == nil || len(filterChildRef.Members()) != 0 ||
		len(filterChildRef.FinalMembers()) != 1 || filterChildRef.FinalMembers()[0] != selectedOuter {
		t.Fatal("outer residual filter reopened the two-alternative live outer group")
	}
}

func TestPredicateCorrelationProvidedByOuterLayoutRequiresExactSource(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("T")
	scalarAlias := values.NamedCorrelationIdentifier("VAL")
	outer := nljPhysicalScan("T")
	outerQ := expressions.NamedPhysicalQuantifier(
		outerAlias, expressions.FinalOfAtStage(outer, expressions.StageCanonical))
	outerRoot := nljFlowed(outerQ)
	explode := mustNLJConstruct(plans.NewRecordQueryExplodePlan(
		values.NewArrayConstructorValue(values.NotNullInt, []values.Value{
			&values.ConstantValue{Value: 7, Typ: values.NotNullInt},
		})))
	scalarQ := expressions.NamedPhysicalQuantifier(
		scalarAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	scalarRoot := mustNLJConstruct(scalarQ.RequireFlowedObjectValue())
	flat := mustNLJConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, scalarQ, outerAlias, scalarAlias,
		values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "_0", Value: outerRoot},
			values.RecordConstructorField{Name: "_1", Value: scalarRoot}),
		false))
	scalarPredicate := predicates.NewComparisonPredicate(
		scalarRoot, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	if !predicateCorrelationProvidedByOuterLayout(scalarPredicate, flat, scalarAlias) {
		t.Fatal("exact retained scalar was not admitted by the selected outer layout")
	}
	wrongType := mustNLJConstruct(values.NewQuantifiedObjectValue(scalarAlias, values.NotNullLong))
	wrongPredicate := predicates.NewComparisonPredicate(
		wrongType, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	if predicateCorrelationProvidedByOuterLayout(wrongPredicate, flat, scalarAlias) {
		t.Fatal("same-correlation wrong exact type was admitted")
	}
	foreign := values.NamedCorrelationIdentifier("FOREIGN")
	if predicateCorrelationProvidedByOuterLayout(scalarPredicate, flat, foreign) {
		t.Fatal("foreign correlation was admitted")
	}
	if provided, provideErr := values.LayoutProvides(
		mustNLJConstruct(flat.ProvidedOutputLayout()), scalarRoot); provideErr != nil || !provided {
		t.Fatalf("predicate admission mutated source layout: (%t, %v)", provided, provideErr)
	}
}

func TestTranslateExistentialWholeRowPredicatesPreservesSameAliasScalar(t *testing.T) {
	t.Parallel()
	oldAlias := values.NamedCorrelationIdentifier("X")
	freshAlias := values.UniqueCorrelationIdentifier()
	wholeType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "X", Ordinal: 1, FieldType: values.NotNullInt},
	})
	oldWhole := mustNLJConstruct(values.NewQuantifiedObjectValue(oldAlias, wholeType))
	freshWhole := mustNLJConstruct(values.NewQuantifiedObjectValue(freshAlias, wholeType))
	scalar := mustNLJConstruct(values.NewQuantifiedObjectValue(oldAlias, values.NotNullInt))
	foreignWhole := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN"), wholeType))
	wrongType := values.NewRecordType("", false, []values.Field{
		{Name: "OTHER", Ordinal: 0, FieldType: values.NotNullLong},
	})
	wrongSameAlias := mustNLJConstruct(values.NewQuantifiedObjectValue(oldAlias, wrongType))

	wholePredicate := predicates.NewComparisonPredicate(
		oldWhole, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	scalarPredicate := predicates.NewComparisonPredicate(
		scalar, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	foreignPredicate := predicates.NewComparisonPredicate(
		foreignWhole, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	wrongTypePredicate := predicates.NewComparisonPredicate(
		wrongSameAlias, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	original := []predicates.QueryPredicate{
		wholePredicate, scalarPredicate, foreignPredicate, wrongTypePredicate,
	}

	translated, err := translateExistentialWholeRowPredicates(
		original, oldWhole, freshWhole)
	if err != nil {
		t.Fatal(err)
	}
	if len(translated) != len(original) {
		t.Fatalf("translated predicates = %d, want %d", len(translated), len(original))
	}
	operand := func(index int) values.Value {
		t.Helper()
		comparison, ok := translated[index].(*predicates.ComparisonPredicate)
		if !ok {
			t.Fatalf("translated predicate %d = %T, want ComparisonPredicate", index, translated[index])
		}
		return comparison.Operand
	}
	if got := operand(0); got != freshWhole {
		t.Fatalf("whole-row operand = %p/%v, want exact fresh root %p/%v",
			got, got, freshWhole, freshWhole)
	}
	if got := operand(1); got != scalar {
		t.Fatalf("same-alias scalar operand = %p/%v, want preserved %p/%v",
			got, got, scalar, scalar)
	}
	if got := operand(2); got != foreignWhole {
		t.Fatalf("foreign whole-row operand = %p/%v, want preserved %p/%v",
			got, got, foreignWhole, foreignWhole)
	}
	if got := operand(3); got != wrongSameAlias {
		t.Fatalf("same-alias wrong-type operand = %p/%v, want preserved %p/%v",
			got, got, wrongSameAlias, wrongSameAlias)
	}
	if wholePredicate.Operand != oldWhole || scalarPredicate.Operand != scalar ||
		foreignPredicate.Operand != foreignWhole || wrongTypePredicate.Operand != wrongSameAlias {
		t.Fatal("whole-row translation mutated a source predicate")
	}

	resultValue := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "whole", Value: oldWhole},
		values.RecordConstructorField{Name: "scalar", Value: scalar},
		values.RecordConstructorField{Name: "foreign", Value: foreignWhole},
		values.RecordConstructorField{Name: "wrong", Value: wrongSameAlias})
	translatedValue, err := translateExistentialWholeRowValue(
		resultValue, oldWhole, freshWhole)
	if err != nil {
		t.Fatal(err)
	}
	translatedRecord, ok := translatedValue.(*values.RecordConstructorValue)
	if !ok || len(translatedRecord.Fields) != 4 {
		t.Fatalf("translated result value = %T/%v, want four-field constructor",
			translatedValue, translatedValue)
	}
	if translatedRecord.Fields[0].Value != freshWhole ||
		translatedRecord.Fields[1].Value != scalar ||
		translatedRecord.Fields[2].Value != foreignWhole ||
		translatedRecord.Fields[3].Value != wrongSameAlias {
		t.Fatalf("translated result fields = %v, want only whole root replaced",
			translatedRecord.Fields)
	}
	if resultValue.Fields[0].Value != oldWhole || resultValue.Fields[1].Value != scalar {
		t.Fatal("whole-row translation mutated the source result value")
	}

	inner := nljPhysicalScan("INNER")
	filter := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		inner, original, values.NamedCorrelationIdentifier("INNER_EDGE")))
	translatedPlan, err := translateExistentialWholeRowPlanPredicates(
		filter, oldWhole, freshWhole)
	if err != nil {
		t.Fatal(err)
	}
	translatedFilter, ok := translatedPlan.(*plans.RecordQueryPredicatesFilterPlan)
	if !ok || translatedFilter == filter {
		t.Fatalf("translated plan = %T/%p, want rebuilt predicate filter", translatedPlan, translatedPlan)
	}
	translatedWhole := translatedFilter.GetPredicates()[0].(*predicates.ComparisonPredicate)
	translatedScalar := translatedFilter.GetPredicates()[1].(*predicates.ComparisonPredicate)
	if translatedWhole.Operand != freshWhole || translatedScalar.Operand != scalar {
		t.Fatalf("translated filter roots = (%v,%v), want (%v,%v)",
			translatedWhole.Operand, translatedScalar.Operand, freshWhole, scalar)
	}
	if filter.GetPredicates()[0] != wholePredicate || wholePredicate.Operand != oldWhole {
		t.Fatal("plan predicate translation mutated the selected source filter")
	}

	wrongReplacement := mustNLJConstruct(values.NewQuantifiedObjectValue(freshAlias, wrongType))
	if result, typeErr := translateExistentialWholeRowPredicates(
		original, oldWhole, wrongReplacement,
	); result != nil || typeErr == nil {
		t.Fatalf("wrong-type replacement = (%v, %v), want nil,error", result, typeErr)
	}
}

func TestImplementNestedLoopJoin_Fires(t *testing.T) {
	t.Parallel()

	// Build: Select([a.id = b.id], [Scan(A), Scan(B)])
	scanA := nljLogicalScan("A")
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := nljLogicalScan("B")
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	joinPred := predicates.NewComparisonPredicate(
		nljField(scanAQ, 0),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: nljField(scanBQ, 0)},
	)

	sel := mustNLJConstruct(expressions.NewSelectExpression(
		nljFlowed(scanAQ),
		[]expressions.Quantifier{scanAQ, scanBQ},
		[]predicates.QueryPredicate{joinPred},
	))
	selRef := expressions.InitialOf(sel)

	// NLJ fires during PLANNING phase (ImplementationRule). Run Plan() to
	// trigger both EXPLORE and PLANNING; physical wrappers land in Members.
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(selRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// After planning, the Select should have a physical NLJ member.
	foundNLJ := false
	for _, m := range selRef.AllMembers() {
		if IsPhysicalNestedLoopJoin(m) {
			foundNLJ = true
			break
		}
	}
	if !foundNLJ {
		t.Fatal("ImplementNestedLoopJoinRule didn't produce a physical NLJ member")
	}
}

// TestImplementNestedLoopJoin_CurrentRootsAreNotSharedExternalSibling pins the
// distinction between a phase-local physical carrier and a real correlation.
// Each independently implemented projection below reads its own `_current`
// scan row. Reference.GetCorrelatedTo consequently reports `_current` for both
// legs, but the two exact carrier QOVs are distinct and do not identify a third
// table excluded from this bipartition. The ordinary materialized cross join
// must remain admissible (the two-CTE cross-product shape).
func TestImplementNestedLoopJoin_CurrentRootsAreNotSharedExternalSibling(t *testing.T) {
	t.Parallel()

	projectedLeg := func(t *testing.T, recordName string) *expressions.Reference {
		t.Helper()
		logicalScanRef := expressions.InitialOf(nljLogicalScan(recordName))
		logicalScanQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(recordName), logicalScanRef)
		logicalCurrentLayout := mustNLJConstruct(values.NewOrdinalLayoutForCarrierType(
			nljSimpleRowType(recordName),
			[]values.OrdinalTileSpec{{Start: 0, Width: 1, Kind: values.OrdinalTileFlat}},
			nil,
		))
		logicalCurrent := logicalCurrentLayout.Carrier()
		logicalCurrentField := mustNLJConstruct(values.ResolveFieldOrdinals(
			logicalCurrent, []int{0}))
		logicalProjection := mustNLJConstruct(expressions.NewLogicalProjectionExpression(
			[]values.Value{logicalCurrentField}, logicalScanQ))
		legRef := expressions.InitialOf(logicalProjection)

		physicalScan := nljPhysicalScan(recordName)
		physicalLayout := mustNLJConstruct(physicalScan.ProvidedOutputLayout())
		physicalField := mustNLJConstruct(values.ResolveFieldOrdinals(
			physicalLayout.Carrier(), []int{0}))
		physicalProjection := mustNLJConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{physicalField}, physicalScan))
		if !legRef.InsertFinal(physicalProjection) {
			t.Fatalf("insert %s physical projection final", recordName)
		}
		if _, current := legRef.GetCorrelatedTo()[values.CurrentCorrelation()]; !current {
			t.Fatalf("setup: %s projected leg does not report its current carrier", recordName)
		}
		return legRef
	}

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("LO"), projectedLeg(t, "LEFT"))
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("HI"), projectedLeg(t, "RIGHT"))
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "LEFT_ID", Value: nljField(leftQ, 0)},
		values.RecordConstructorField{Name: "RIGHT_ID", Value: nljField(rightQ, 0)},
	)
	selectExpr := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		result,
		[]expressions.Quantifier{leftQ, rightQ},
		nil,
		[]string{"LO", "HI"},
		expressions.JoinCross,
	))

	results := mustFireExpressionRule(
		t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(selectExpr))
	if len(results) == 0 {
		t.Fatal("independent current-rooted projected legs yielded no materialized join")
	}
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			return
		}
	}
	t.Fatalf("independent current-rooted projected legs yielded no NLJ; first result %T", results[0])
}

// TestImplementNestedLoopJoin_SharedNamedExternalSiblingStillDeclines is the
// mutation control for the exception above. Unlike `_current`, a named D root
// referenced by both legs denotes a real excluded sibling. Materializing the
// two-leg fragment alone would leave D unbound, so the incomplete-bipartition
// guard must continue to decline it.
func TestImplementNestedLoopJoin_SharedNamedExternalSiblingStillDeclines(t *testing.T) {
	t.Parallel()

	externalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("D"), nljSimpleRowType("D")))
	externalField := mustNLJConstruct(values.ResolveFieldOrdinals(externalRoot, []int{0}))
	correlatedLeg := func(t *testing.T, recordName string) *expressions.Reference {
		t.Helper()
		logicalScanRef := expressions.InitialOf(nljLogicalScan(recordName))
		logicalScanQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(recordName), logicalScanRef)
		logicalProjection := mustNLJConstruct(expressions.NewLogicalProjectionExpression(
			[]values.Value{externalField}, logicalScanQ))
		legRef := expressions.InitialOf(logicalProjection)
		physicalProjection := mustNLJConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{externalField}, nljPhysicalScan(recordName)))
		if !legRef.InsertFinal(physicalProjection) {
			t.Fatalf("insert %s external-correlated projection final", recordName)
		}
		if _, found := legExternalAliases(
			legRef, map[values.CorrelationIdentifier]struct{}{},
		)[externalRoot.Correlation()]; !found {
			t.Fatalf("setup: %s leg does not retain named external D", recordName)
		}
		return legRef
	}

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("L"), correlatedLeg(t, "LEFT"))
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("R"), correlatedLeg(t, "RIGHT"))
	selectExpr := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "LEFT_D", Value: nljField(leftQ, 0)},
			values.RecordConstructorField{Name: "RIGHT_D", Value: nljField(rightQ, 0)},
		),
		[]expressions.Quantifier{leftQ, rightQ},
		nil,
		[]string{"L", "R"},
		expressions.JoinCross,
	))

	results := mustFireExpressionRule(
		t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(selectExpr))
	if len(results) != 0 {
		t.Fatalf("shared named external sibling yielded %d unsafe implementation(s), first %T",
			len(results), results[0])
	}
}

func TestImplementNestedLoopJoin_DoesNotFireOnSingleQuantifier(t *testing.T) {
	t.Parallel()

	// Select with only 1 quantifier (not a join).
	scan := nljLogicalScan("A")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sel := mustNLJConstruct(expressions.NewSelectExpression(
		nljFlowed(scanQ),
		[]expressions.Quantifier{scanQ},
		nil,
	))
	selRef := expressions.InitialOf(sel)

	results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), selRef)
	if len(results) != 0 {
		t.Fatal("ImplementNestedLoopJoinRule should NOT fire on single-quantifier Select")
	}
}

func TestImplementNestedLoopJoin_RecordExplodeOuterSupportsCorrelatedExplodeInner(t *testing.T) {
	t.Parallel()

	arrayValue := values.NewArrayConstructorValue(values.NotNullInt, []values.Value{
		&values.ConstantValue{Typ: values.NotNullInt, Value: int32(101)},
	})
	row := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(1)}},
		values.RecordConstructorField{Name: "ARR", Value: arrayValue},
	)
	rowType := row.Type()
	rows := values.NewArrayConstructorValue(rowType, []values.Value{row})
	originalElement := rows.Elements[0]
	originalType := rows.ElementType

	outerLogical := mustNLJConstruct(expressions.NewExplodeExpression(rows))
	outerPhysical := mustNLJConstruct(plans.NewRecordQueryExplodePlan(rows))
	outerRef := expressions.InitialOf(outerLogical)
	outerRef.InsertFinal(outerPhysical)
	outerAlias := values.NamedCorrelationIdentifier("VALUES")
	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	outerRow := nljFlowed(outerQ)
	outerArray := mustNLJConstruct(values.ResolveFieldOrdinals(outerRow, []int{1}))

	innerLogical := mustNLJConstruct(expressions.NewExplodeExpressionWithOrdinality(outerArray, true))
	innerPhysical := mustNLJConstruct(plans.NewRecordQueryExplodePlanWithOrdinality(outerArray, true))
	innerRef := expressions.InitialOf(innerLogical)
	innerRef.InsertFinal(innerPhysical)
	innerAlias := values.NamedCorrelationIdentifier("U")
	innerQ := expressions.NamedForEachQuantifier(innerAlias, innerRef)
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "OUTER", Value: outerRow},
		values.RecordConstructorField{Name: "INNER", Value: nljFlowed(innerQ)},
	)
	sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		result,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{"VALUES", "U"},
		expressions.JoinCross,
	))

	results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	foundFlatMap := false
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryFlatMapPlan); ok {
			foundFlatMap = true
		}
	}
	if !foundFlatMap {
		t.Fatalf("record-valued outer Explode plus correlated inner Explode yielded no FlatMap; results=%d", len(results))
	}
	if rows.ElementType != originalType || rows.Elements[0] != originalElement || !row.Type().Equals(rowType) {
		t.Fatal("record-valued NLJ admission mutated its source collection")
	}

	scalarRows := &values.ConstantValue{
		Typ:   values.NewArrayType(false, values.NotNullInt),
		Value: []any{int32(1), int32(2)},
	}
	scalarLogical := mustNLJConstruct(expressions.NewExplodeExpression(scalarRows))
	scalarPhysical := mustNLJConstruct(plans.NewRecordQueryExplodePlan(scalarRows))
	scalarRef := expressions.InitialOf(scalarLogical)
	scalarRef.InsertFinal(scalarPhysical)
	scalarQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("IN"), scalarRef)
	scanLogical := nljLogicalScan("T")
	scanRef := expressions.InitialOf(scanLogical)
	scanRef.InsertFinal(nljPhysicalScan("T"))
	scanQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("T"), scanRef)
	scalarSelect := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		nljFlowed(scanQ),
		[]expressions.Quantifier{scalarQ, scanQ},
		nil,
		[]string{"IN", "T"},
		expressions.JoinCross,
	))
	if got := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(scalarSelect)); len(got) != 0 {
		t.Fatalf("scalar uncorrelated Explode yielded %d NLJ alternatives, want IN-rule ownership", len(got))
	}
}

func TestImplementNestedLoopJoin_PlanOutput(t *testing.T) {
	t.Parallel()

	scanA := nljLogicalScan("A")
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := nljLogicalScan("B")
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	sel := mustNLJConstruct(expressions.NewSelectExpression(
		nljFlowed(scanAQ),
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
	))
	selRef := expressions.InitialOf(sel)

	// Plan the join.
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(selRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalNestedLoopJoin(plan) && !IsPhysicalFlatMap(plan) {
		t.Fatalf("expected NLJ or FlatMap plan, got %T", plan)
	}

	// Verify explain output.
	explain := ExplainPhysicalPlan(plan)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("NLJ Explain: %s", explain)
}

// TestImplementNestedLoopJoin_StrictSingleForcesCompensatedFlatMap pins the
// semantic carrier used by correlated scalar subqueries. A later simplification
// can remove the inner plan's last syntactic reference to the outer row (for
// example, `inner.fk = outer.id OR TRUE`), but that must not make the
// strict-single edge eligible for the ordinary materialized NLJ path: that path
// has no at-most-one-row check and would silently fan out the outer row.
//
// The edge flag is the durable contract. Even with two completely independent
// scan references, it must force the existing FlatMap + strict
// FirstOrDefault compensation and must produce no unwrapped NLJ alternative.
func TestImplementNestedLoopJoin_StrictSingleForcesCompensatedFlatMap(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("O")
	innerAlias := values.NamedCorrelationIdentifier("I")

	outerLogical := nljLogicalScan("OUTER")
	outerRef := expressions.InitialOf(outerLogical)
	outerRef.InsertFinal(nljPhysicalScan("OUTER"))

	innerLogical := nljLogicalScan("INNER")
	innerRef := expressions.InitialOf(innerLogical)
	innerRef.InsertFinal(nljPhysicalScan("INNER"))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedForEachStrictSingleQuantifier(innerAlias, innerRef)
	sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		nljFlowed(outerQ),
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{"O", "I"},
		expressions.JoinLeftOuter,
	))

	results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	if len(results) == 0 {
		t.Fatal("strict-single select yielded no physical implementation")
	}

	foundStrictFlatMap := false
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			t.Fatalf("strict-single select yielded an unwrapped materialized NLJ: %T", result)
		}
		flatMap, ok := result.(*plans.RecordQueryFlatMapPlan)
		if !ok {
			continue
		}
		hasStrictFirstOrDefault := false
		plans.Walk(flatMap, func(plan plans.RecordQueryPlan) bool {
			if first, ok := plan.(*plans.RecordQueryFirstOrDefaultPlan); ok && first.IsStrict() {
				hasStrictFirstOrDefault = true
			}
			return true
		})
		if hasStrictFirstOrDefault {
			foundStrictFlatMap = true
		}
	}
	if !foundStrictFlatMap {
		t.Fatalf("strict-single select yielded no FlatMap with strict FirstOrDefault; results: %d", len(results))
	}
}

func TestTranslatePredicateLogicalSourceUsesTheSelectedPhysicalCarrier(t *testing.T) {
	t.Parallel()
	logicalType := values.NewRecordType("B", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
	})
	physicalType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
	})
	logicalAlias := values.NamedCorrelationIdentifier("B")
	logicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(logicalAlias, logicalType))
	logicalField := mustNLJConstruct(values.ResolveFieldOrdinals(logicalRoot, []int{1}))
	physicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.UniqueCorrelationIdentifier(), physicalType))
	predicate := predicates.NewComparisonPredicate(
		logicalField,
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(2), Typ: values.NullableLong},
		},
	)

	translated, err := translatePredicateLogicalSource(
		[]predicates.QueryPredicate{predicate}, logicalAlias, physicalRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(translated) != 1 {
		t.Fatalf("translated predicates = %d, want 1", len(translated))
	}
	var roots []values.QuantifiedObjectValue
	_, err = predicates.TransformEmbeddedValuesChecked(
		translated[0], func(value values.Value) (values.Value, error) {
			values.WalkValue(value, func(node values.Value) bool {
				if root, ok := values.AsQuantifiedObjectValue(node); ok {
					roots = append(roots, root)
				}
				return true
			})
			return value, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != physicalRoot {
		t.Fatalf("translated roots = %v, want the exact selected physical root", roots)
	}

	// The CONFLICT is a different row SHAPE under the same alias. A RecordName
	// is provenance and compares equal (Java's Type.Record.equals), so
	// "other-B" over B's exact fields IS B and the rejection below would have
	// nothing to reject.
	conflictingType := values.NewRecordType("other-B", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
		{Name: "CONFLICTING_ONLY", Ordinal: 2, FieldType: values.NullableLong},
	})
	conflictingRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(logicalAlias, conflictingType))
	conflictingField := mustNLJConstruct(values.ResolveFieldOrdinals(conflictingRoot, []int{1}))
	conflictingPredicate := predicates.NewComparisonPredicate(
		logicalField,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: conflictingField},
	)
	if result, conflictErr := translatePredicateLogicalSource(
		[]predicates.QueryPredicate{conflictingPredicate}, logicalAlias, physicalRoot, nil,
	); result != nil || conflictErr == nil {
		t.Fatalf("conflicting logical source translation = (%v,%v), want nil,error", result, conflictErr)
	}

	// The SAME two-shapes-one-alias input, with the second shape declared as a
	// source the selected row RETAINS. It is then not a second declaration of
	// the row at all, so the retarget proceeds and touches only the row. This
	// is the chained-unnest shape (`FROM t, t.arr AS x, x.sub AS y` binds the
	// merged row as Y and keeps Y's own element inside it); the arm above and
	// this one differ ONLY in whether the layout admits the second reading,
	// which is exactly what decides real-vs-apparent conflict.
	retained, retainedErr := translatePredicateLogicalSource(
		[]predicates.QueryPredicate{conflictingPredicate}, logicalAlias, physicalRoot,
		[]values.Type{conflictingType})
	if retainedErr != nil {
		t.Fatalf("retained-window logical source translation: %v", retainedErr)
	}
	if len(retained) != 1 {
		t.Fatalf("retained-window translation produced %d predicates, want 1", len(retained))
	}
	var retainedRoots []values.QuantifiedObjectValue
	if _, walkErr := predicates.TransformEmbeddedValuesChecked(
		retained[0], func(value values.Value) (values.Value, error) {
			values.WalkValue(value, func(node values.Value) bool {
				if root, ok := values.AsQuantifiedObjectValue(node); ok {
					retainedRoots = append(retainedRoots, root)
				}
				return true
			})
			return value, nil
		}); walkErr != nil {
		t.Fatal(walkErr)
	}
	// Both sides survive and they are DIFFERENT objects: the row moved to the
	// selected physical carrier, the retained window kept its authored
	// correlation and its own exact type. A translation that rewrote both would
	// leave two identical roots here and silently make the predicate compare a
	// row against itself.
	if len(retainedRoots) != 2 {
		t.Fatalf("retained-window translation left %d roots, want 2", len(retainedRoots))
	}
	if retainedRoots[0] != physicalRoot {
		t.Fatalf("row root = %v, want the selected physical carrier", retainedRoots[0])
	}
	if retainedRoots[1] != conflictingRoot {
		t.Fatalf("retained window root = %v, want the authored window untouched", retainedRoots[1])
	}
}

func TestNormalizeMaterializedJoinProgramsPreservesProgramsAndChecksLegs(t *testing.T) {
	t.Parallel()
	leftAlias := values.NamedCorrelationIdentifier("L")
	rightAlias := values.NamedCorrelationIdentifier("R")
	foreignAlias := values.NamedCorrelationIdentifier("FOREIGN")
	// The logical rows carry the table's nominal record name; the physical
	// carrier below is anonymous. That is NOT a type difference — a RecordName
	// is provenance and Java's Type.Record.equals ignores it — so the programs
	// already name the carrier's own row and there is nothing to re-root.
	logicalType := func(name string) values.Type {
		return values.NewRecordType(name, false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
			{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
		})
	}
	physicalType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
	})
	leftLogicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		leftAlias, logicalType("LEFT")))
	rightLogicalRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		rightAlias, logicalType("RIGHT")))
	foreignRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(
		foreignAlias, logicalType("FOREIGN")))
	leftID := mustNLJConstruct(values.ResolveFieldOrdinals(leftLogicalRoot, []int{0}))
	rightID := mustNLJConstruct(values.ResolveFieldOrdinals(rightLogicalRoot, []int{0}))
	foreignID := mustNLJConstruct(values.ResolveFieldOrdinals(foreignRoot, []int{0}))
	predicate := predicates.NewValuePredicate(values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "L", Value: leftID},
		values.RecordConstructorField{Name: "R", Value: rightID},
		values.RecordConstructorField{Name: "F", Value: foreignID},
	))
	// A NARROWER same-alias retained window. It is the shape the removed
	// rewrite most had to avoid touching, and the pin that it still survives.
	narrowLeftType := values.NewRecordType("LEFT_WINDOW", false, []values.Field{{
		Name: "ID", Ordinal: 0, FieldType: values.NullableLong,
	}})
	narrowLeftRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(leftAlias, narrowLeftType))
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "L", Value: leftID},
		values.RecordConstructorField{Name: "R", Value: rightID},
		values.RecordConstructorField{Name: "LW", Value: narrowLeftRoot},
		values.RecordConstructorField{Name: "F", Value: foreignID},
	)
	leftPlan := mustNLJConstruct(plans.NewRecordQueryScanPlan(
		[]string{"LEFT"}, physicalType, false))
	rightPlan := mustNLJConstruct(plans.NewRecordQueryScanPlan(
		[]string{"RIGHT"}, physicalType, false))

	translated, normalizedResult, err := normalizeMaterializedJoinPrograms(
		[]predicates.QueryPredicate{predicate},
		resultValue,
		leftPlan, leftAlias, rightPlan, rightAlias)
	if err != nil {
		t.Fatal(err)
	}
	if len(translated) != 1 || translated[0] != predicate {
		t.Fatalf("predicate program = %v, want the input program preserved", translated)
	}
	if normalizedResult != resultValue {
		t.Fatal("result program was rebuilt; a program already on the carrier's own row must be left alone")
	}
	// The returned slice is a COPY: a caller that mutates it must not reach the
	// input. Checked here because the rewrite that used to guarantee it is gone.
	translated[0] = nil
	if preserved := []predicates.QueryPredicate{predicate}; preserved[0] != predicate {
		t.Fatal("returned predicate slice aliases the caller's")
	}

	// Every leg root the programs carry is already the selected carrier's own
	// exact row, which is what makes the re-rooting unnecessary rather than
	// merely skipped. The narrow window is deliberately NOT — it is a different
	// object and stays one.
	for alias, plan := range map[values.CorrelationIdentifier]plans.RecordQueryPlan{
		leftAlias: leftPlan, rightAlias: rightPlan,
	} {
		carrier := values.PhysicalCarrierType(mustNLJConstruct(plan.ProvidedOutputLayout()))
		root := leftLogicalRoot
		if alias == rightAlias {
			root = rightLogicalRoot
		}
		if !root.FlowedType().Equals(carrier) {
			t.Fatalf("%s logical root %s is not the selected carrier row %s",
				alias.Name(), root.FlowedType(), carrier)
		}
	}
	if narrowLeftRoot.FlowedType().Equals(leftLogicalRoot.FlowedType()) {
		t.Fatal("the narrow retained window must be a different exact row, or it pins nothing")
	}

	// A leg that cannot state its selected plan or alias is still a malformed
	// plan and still fails closed — the one check that survived.
	for name, call := range map[string]func() ([]predicates.QueryPredicate, values.Value, error){
		"missing left plan": func() ([]predicates.QueryPredicate, values.Value, error) {
			return normalizeMaterializedJoinPrograms(
				[]predicates.QueryPredicate{predicate}, resultValue,
				nil, leftAlias, rightPlan, rightAlias)
		},
		"missing right alias": func() ([]predicates.QueryPredicate, values.Value, error) {
			return normalizeMaterializedJoinPrograms(
				[]predicates.QueryPredicate{predicate}, resultValue,
				leftPlan, leftAlias, rightPlan, values.CorrelationIdentifier{})
		},
	} {
		if gotPreds, gotResult, gotErr := call(); gotPreds != nil || gotResult != nil || gotErr == nil {
			t.Fatalf("%s = (%v,%v,%v), want nil,nil,error", name, gotPreds, gotResult, gotErr)
		}
	}
}

// TestImplementNestedLoopJoin_DualStrictSingleFailsClosed covers a malformed
// shape the SQL translator does not emit: both legs claim scalar cardinality.
// The current FlatMap compensation can enforce one inner leg per outer, not two
// mutually inner legs. The rule must therefore decline the shape rather than
// fall back to an ordinary materialized NLJ that enforces neither contract.
func TestImplementNestedLoopJoin_DualStrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	leftAlias := values.NamedCorrelationIdentifier("L")
	rightAlias := values.NamedCorrelationIdentifier("R")

	leftRef := expressions.InitialOf(nljLogicalScan("LEFT"))
	leftRef.InsertFinal(nljPhysicalScan("LEFT"))
	rightRef := expressions.InitialOf(nljLogicalScan("RIGHT"))
	rightRef.InsertFinal(nljPhysicalScan("RIGHT"))

	leftQ := expressions.NamedForEachStrictSingleQuantifier(leftAlias, leftRef)
	rightQ := expressions.NamedForEachStrictSingleQuantifier(rightAlias, rightRef)
	sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		nljFlowed(leftQ),
		[]expressions.Quantifier{leftQ, rightQ},
		nil,
		[]string{"L", "R"},
		expressions.JoinInner,
	))

	results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	if len(results) != 0 {
		for _, result := range results {
			if _, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok {
				t.Fatalf("dual strict-single select yielded an unwrapped materialized NLJ: %T", result)
			}
		}
		t.Fatalf("dual strict-single select must fail closed, got %d implementation(s)", len(results))
	}
}

// TestImplementNestedLoopJoin_StrictSingleUnsupportedShapesFailClosed pins the
// rule's global carrier invariant. StrictSingle has exactly one implementation:
// LEFT OUTER [plain outer, strict right]. Other join kinds, orientations, and
// special arms must not consume or materialize a flagged edge without the exact
// scalar-subquery semantics owned by that path.
func TestImplementNestedLoopJoin_StrictSingleUnsupportedShapesFailClosed(t *testing.T) {
	t.Parallel()

	newScanRef := func(name string) *expressions.Reference {
		ref := expressions.InitialOf(nljLogicalScan(name))
		ref.InsertFinal(nljPhysicalScan(name))
		return ref
	}
	assertNoImplementation := func(t *testing.T, sel *expressions.SelectExpression) {
		t.Helper()
		results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
		if len(results) != 0 {
			t.Fatalf("strict-single unsupported shape yielded %d implementation(s), including %T",
				len(results), results[0])
		}
	}

	t.Run("full_outer", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinFullOuter,
		)))
	})

	t.Run("inner", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinInner,
		)))
	})

	t.Run("cross", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinCross,
		)))
	})

	t.Run("strict_left", func(t *testing.T) {
		leftQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinLeftOuter,
		)))
	})

	t.Run("null_on_empty_left", func(t *testing.T) {
		leftQ := expressions.NamedForEachNullOnEmptyQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			expressions.JoinLeftOuter,
		)))
	})

	t.Run("three_quantifier_existential", func(t *testing.T) {
		leftQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		rightQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
		existAlias := values.NamedCorrelationIdentifier("E")
		existQ := expressions.NamedExistentialQuantifier(existAlias, newScanRef("EXISTS"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithAliases(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, rightQ, existQ},
			[]predicates.QueryPredicate{mustExistentialAlias(t, existAlias)},
			[]string{"L", "R", "E"},
		)))
	})

	t.Run("two_quantifier_existential", func(t *testing.T) {
		leftQ := expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
		existAlias := values.NamedCorrelationIdentifier("E")
		existQ := expressions.NamedExistentialQuantifier(existAlias, newScanRef("EXISTS"))
		assertNoImplementation(t, mustNLJConstruct(expressions.NewSelectExpressionWithAliases(
			nljFlowed(leftQ),
			[]expressions.Quantifier{leftQ, existQ},
			[]predicates.QueryPredicate{mustExistentialAlias(t, existAlias)},
			[]string{"L", "E"},
		)))
	})
}

// TestImplementNestedLoopJoin_ExistsShortcutRejectsFanOutCandidate pins the
// raw correlated-index shortcut used by nested EXISTS. A composite
// (FK, TAGS FAN_OUT) index has no entry when TAGS is empty, even when FK
// matches, so using its flat first-column metadata as a correlated FK probe
// would turn a true EXISTS into false. The ordinary (FK, STATUS) index remains
// eligible and is selected after the fan-out candidate is rejected.
func TestImplementNestedLoopJoin_ExistsShortcutRejectsFanOutCandidate(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("O")
	innerAlias := values.NamedCorrelationIdentifier("I")

	// Both legs flow their declared row type, and both comparands are baked
	// against it. The index shortcut resolves the candidate's first column name
	// against the inner leg's layout once and then compares ordinals, so an
	// untyped leg has no layout to resolve in and declines before candidate
	// selection is ever reached — which would make this test pass for the wrong
	// reason (nothing selected because nothing was tried).
	outerRowType := values.Type(nljTestLayouts["OUTER"])
	innerRowType := values.Type(nljTestLayouts["INNERFK"])

	outerScan := mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, outerRowType))
	outerRef := expressions.InitialOf(outerScan)
	outerRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerRowType, false)))

	innerScan := mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"INNER"}, innerRowType))
	innerRef := expressions.InitialOf(innerScan)
	innerRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"INNER"}, innerRowType, false)))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedExistentialQuantifier(innerAlias, innerRef)
	outerID := nljBakedRef(t, "OUTER", outerAlias, "ID")
	innerFK := nljBakedRef(t, "INNERFK", innerAlias, "FK")
	joinPredicate := predicates.NewComparisonPredicate(
		innerFK,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerID},
	)
	selectExpr := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(
		nljFlowed(outerQ),
		[]expressions.Quantifier{outerQ, innerQ},
		[]predicates.QueryPredicate{
			joinPredicate,
			mustExistentialAlias(t, innerAlias),
		},
		[]string{"O", "I"},
	))

	fanOut := true
	scalar := false
	scalarFanType := gen.Field_SCALAR
	newCandidate := func(name string, columns []string, createsDuplicates *bool) MatchCandidate {
		aliases := make([]values.CorrelationIdentifier, len(columns))
		for i := range aliases {
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		return NewValueIndexScanMatchCandidateWithFunctions(
			name,
			[]string{"INNER"},
			columns,
			nil,
			aliases,
			innerRowType,
			false,
			nil,
			createsDuplicates,
		).WithKeyComponentTypes(syntheticIndexKeyTypes(len(columns)))
	}
	functionCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"INNER$cardinality_fk",
		[]string{"INNER"},
		[]string{"FK"},
		[]string{FunctionKindCardinality},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		innerRowType,
		false,
		nil,
		&scalar,
	)
	nestedCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"INNER$addr_fk",
		[]string{"INNER"},
		[]string{"FK"},
		nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		innerRowType,
		false,
		nil,
		&scalar,
	).WithRootKeyExpression(&gen.KeyExpression{Nesting: &gen.Nesting{
		Parent: &gen.Field{
			FieldName: proto.String("ADDR"),
			FanType:   &scalarFanType,
		},
		Child: candidateTestKeyField("FK", gen.Field_SCALAR),
	}})
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newCandidate("INNER$fk_tags_fanout", []string{"FK", "TAGS"}, &fanOut),
		functionCandidate,
		nestedCandidate,
		newCandidate("INNER$fk_status", []string{"FK", "STATUS"}, &scalar),
	}}

	results := mustFireExpressionRuleWithMemo(t,
		NewImplementNestedLoopJoinRule(),
		expressions.InitialOf(selectExpr),
		ctx,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("expected one EXISTS FlatMap alternative, got %d", len(results))
	}
	flatMap, ok := results[0].(*plans.RecordQueryFlatMapPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryFlatMapPlan, got %T", results[0])
	}

	var selected []string
	var selectedTypes []values.Type
	plans.Walk(flatMap, func(plan plans.RecordQueryPlan) bool {
		if indexPlan, ok := plan.(*plans.RecordQueryIndexPlan); ok {
			selected = append(selected, indexPlan.GetIndexName())
			selectedTypes = indexPlan.GetKeyComponentTypes()
		}
		return true
	})
	if len(selected) != 1 || selected[0] != "INNER$fk_status" {
		t.Fatalf("EXISTS shortcut selected indexes %v, want only the non-fan-out FK index", selected)
	}
	if len(selectedTypes) != 2 || selectedTypes[0].Code() != values.TypeCodeLong ||
		selectedTypes[1].Code() != values.TypeCodeLong {
		t.Fatalf("EXISTS shortcut lost candidate physical key types: %v", selectedTypes)
	}
}

// TestMaterializedNLJOrdinalLayoutMatches pins a real production defect: a
// ChildrenAsSet-swapped firing of ImplementNestedLoopJoinRule (the
// join-commutativity exploration in fireExprRuleOnMember,
// expression_matcher.go) reuses the logical select's resultValue UNCHANGED
// under the swapped (outer, inner) assignment
// (expressions.SelectExpression.WithSwappedQuantifiers). That is sound for
// an ordinary correlation-addressed resultValue (order-independent by
// design) but NOT for a pristine ORDINAL SEED — a RecordConstructorValue of
// baked FrontierPinned FieldValues whose ordinals assume ONE FIXED physical
// field order, baked when the seed was first built. The materialized NLJ's
// own executor (executor.go's concatLegPositionals/mergeRows) ALWAYS
// concatenates outer-then-inner, so a swapped orientation whose seed still
// assumes the OTHER leg first silently (or loudly) misreads the runtime
// merged row.
//
// This was found via TestFDB_LeftJoinExistsResidual/I1 going from a correct
// empty result to a hard "correlated FieldValue ID (correlation E) evaluated
// against an unbound/unrecognized context" runtime error once a cost-model
// fix (RFC-192 follow-up) made the previously-never-cheapest swapped
// orientation the memo's winner for the first time — the bug was always
// latent, just unreachable before. A first fix attempt compared alias
// STRINGS (sel.GetSourceAliases() falling back to the quantifier's own
// GetAlias()) and regressed two OTHER real things: several RIGHT-OUTER-JOIN
// queries (whose quantifier identity is the synthetic one while
// sourceAliases carries the real leg name — the opposite divergence
// direction) and, worse, TestFDB_QuotedMachineShapedAliases/join_legs — a
// user alias quoted as "q$2" is byte-identical to a synthetic
// UniqueCorrelationIdentifier that happens to reach counter 2
// (values.CorrelationIdentifier is a bare wrapped string), so alias-string
// guessing is fundamentally unsafe. The final fix compares leg ROW SHAPE
// (RecordName + fields) instead — never a name.
func TestMaterializedNLJOrdinalLayoutMatches(t *testing.T) {
	t.Parallel()

	t1Type := nljTestLayout("T1", "ID", "V")
	t2Type := nljTestLayout("T2", "ID", "T1_ID")
	t1 := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"T1"}, t1Type, false))
	t2 := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"T2"}, t2Type, false))
	seed, _ := reconstructFoldStep1Seed(t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner)
	if seed == nil {
		t.Fatal("setup: expected reconstructFoldStep1Seed to build a seed for two scan legs")
	}
	// Sanity-check the setup matches TestReconstructFoldStep1Seed's own pin:
	// T1 at offset 0 (2 columns), T2 at offset 2.
	if w, _ := ordinalSeedLegWindowsOf(seed); w == nil || w[values.NamedCorrelationIdentifier("T1")].Offset != 0 || w[values.NamedCorrelationIdentifier("T2")].Offset != 2 {
		t.Fatalf("setup: unexpected seed layout %+v", w)
	}

	t.Run("matching orientation passes", func(t *testing.T) {
		t.Parallel()
		if !materializedNLJOrdinalLayoutMatches(seed, t1, t2) {
			t.Fatal("T1-outer/T2-inner matches the seed's own T1@0,T2@2 layout — must pass")
		}
	})

	t.Run("swapped orientation is rejected", func(t *testing.T) {
		t.Parallel()
		if materializedNLJOrdinalLayoutMatches(seed, t2, t1) {
			t.Fatal("T2-outer/T1-inner contradicts the seed's T1-first layout — must be rejected")
		}
	})

	t.Run("non-ordinal-seed resultValue is always order-independent", func(t *testing.T) {
		t.Parallel()
		correlationAddressed := mustNLJConstruct(values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("X"), t1Type))
		if !materializedNLJOrdinalLayoutMatches(correlationAddressed, t2, t1) {
			t.Fatal("a non-ordinal-seed (correlation-addressed) resultValue must match regardless of orientation")
		}
	})

	t.Run("adversarial quoted alias shaped like the machine namespace never matters", func(t *testing.T) {
		t.Parallel()
		// The real production regression: a user alias quoted as "q$2" is
		// byte-identical, as a values.CorrelationIdentifier, to a synthetic
		// UniqueCorrelationIdentifier that happens to reach counter 2 — an
		// alias-string-based check cannot tell them apart. Naming the legs
		// "Q$1"/"Q$2" here must not change the verdict at all, because this
		// check never looks at a name.
		q1 := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"T1"}, t1Type, false))
		q2 := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"T2"}, t2Type, false))
		adversarialSeed, _ := reconstructFoldStep1Seed(q1, q2, values.NamedCorrelationIdentifier("Q$1"), values.NamedCorrelationIdentifier("Q$2"), plans.JoinInner)
		if adversarialSeed == nil {
			t.Fatal("setup: expected a seed for the adversarially-named legs")
		}
		if !materializedNLJOrdinalLayoutMatches(adversarialSeed, q1, q2) {
			t.Fatal("matching orientation must pass regardless of the legs' alias spelling")
		}
		if materializedNLJOrdinalLayoutMatches(adversarialSeed, q2, q1) {
			t.Fatal("swapped orientation must still be rejected regardless of the legs' alias spelling")
		}
	})

	t.Run("cannot verify structurally defaults to true", func(t *testing.T) {
		t.Parallel()
		// A plan whose own GetResultType() isn't a (Relation-of-)RecordType
		// (here: nil, standing in for an opaque/erased result) can't be
		// compared against the seed's leg shapes at all — under-detecting is
		// the documented safe default.
		if !materializedNLJOrdinalLayoutMatches(seed, nil, t2) {
			t.Fatal("an unverifiable outer plan must default to true (under-detection is safe)")
		}
	})

	t.Run("self-join (identical leg shapes) is always safe", func(t *testing.T) {
		t.Parallel()
		s1 := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"T1"}, t1Type, false))
		s2 := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"T1"}, t1Type, false))
		selfSeed, _ := reconstructFoldStep1Seed(s1, s2, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), plans.JoinInner)
		if selfSeed == nil {
			t.Fatal("setup: expected a seed for two same-shaped legs")
		}
		if !materializedNLJOrdinalLayoutMatches(selfSeed, s1, s2) || !materializedNLJOrdinalLayoutMatches(selfSeed, s2, s1) {
			t.Fatal("identical leg shapes are structurally indistinguishable — both orientations must pass")
		}
	})
}

// fusedNestedFieldValue builds a FUSED baked nested reference (Field=leaf,
// Child=the bare source QOV directly, Resolved carrying a TWO-accessor
// [parent, leaf] path) — the shape a baked `alias.parent.leaf` reference
// takes, as opposed to a flat `alias.leaf` top-level column. Mirrors
// fkChainCorrelatedNestedEq (fk_chain_cardinality_test.go), which pins the
// identical hole in the sibling fk-chain cardinality cap.
func fusedNestedFieldValue(alias values.CorrelationIdentifier, parent, leaf string) values.FieldValue {
	nestedType := values.NewRecordType(parent, false, []values.Field{
		{Name: "PADDING", FieldType: values.NullableLong},
		{Name: leaf, FieldType: values.NullableLong},
	})
	rootType := values.NewRecordType("nested_root", false, []values.Field{
		{Name: parent, FieldType: nestedType},
	})
	root := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, rootType))
	return mustNLJField(mustNLJConstruct(values.ResolveFieldOrdinals(root, []int{0, 1})))
}

// TestLegCorrelationOf_DeclinesFusedNestedSameLeafName pins the wrong-rows
// hole the old fieldValueAliasAndCol's bare-column allowlist guarded: a fused
// multi-accessor bake (Child=QOV directly, Resolved=[ADDRESS, ID],
// Field="ID") passes the Child==QOV check while still reading a NESTED
// record's column, so the quantifier's row is not the layout its leaf names.
// Reporting the root quantifier would let `i.address.id` stand in for a bare
// top-level `I.ID` reference in matchJoinPKPredicate.
func TestLegCorrelationOf_DeclinesFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	fused := fusedNestedFieldValue(values.NamedCorrelationIdentifier("I"), "ADDRESS", "ID")
	if leg, ok := legCorrelationOf(fused); ok {
		t.Fatalf("legCorrelationOf(fused I.ADDRESS.ID) = (%v, true), want ok=false — "+
			"a nested reference must never report the root quantifier as the leg it reads", leg)
	}
}

// TestLegCorrelationOf_AcceptsBareTopLevelColumn is the accept-direction
// companion: a genuine bare top-level reference must still report its leg, so
// the decline above is specific to the fused shape rather than a blanket
// refusal that would silently disable the fast path.
func TestLegCorrelationOf_AcceptsBareTopLevelColumn(t *testing.T) {
	t.Parallel()

	bare := nljBakedRef(t, "INNER", values.NamedCorrelationIdentifier("I"), "ID")
	leg, ok := legCorrelationOf(bare)
	if !ok || leg.String() != "I" {
		t.Fatalf("legCorrelationOf(bare I.ID) = (%v, %v), want (I, true) — "+
			"the decline must not over-reach to ordinary references", leg, ok)
	}
}

// TestReadsKeyColumn_Dimensions is the identity comparison that replaced the
// leaf-name match, probed on each element of RFC-197's triple SEPARATELY.
//
// Each case differs from the accepted one in exactly ONE element, so a
// mutation that drops that element is caught here and nowhere else. That
// separation is the point: on the explaindiff corpus the domain check and the
// correlation check reject the same predicates, so a corpus-only check cannot
// tell which of the two is doing the work, and a fix satisfying only one of
// them would measure as complete.
func TestReadsKeyColumn_Dimensions(t *testing.T) {
	t.Parallel()

	innerLayout := nljTestLayouts["INNER"]
	innerFrontier := values.OrdinalDomainOfType(innerLayout)
	innerCorr := values.NamedCorrelationIdentifier("I")

	// The inner table's primary key, resolved once against the inner leg's own
	// layout — the boundary rule, and the only place the name is legitimate.
	keyIdent, ok := values.OrdinalOfNameIn(innerLayout, "ID")
	if !ok {
		t.Fatal("setup: INNER.ID must resolve against INNER's own layout")
	}

	t.Run("accepts the real key column", func(t *testing.T) {
		t.Parallel()
		ref := nljBakedRef(t, "INNER", innerCorr, "ID")
		if !readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("I.ID must match INNER's primary key ID — over-declining silently disables the probe")
		}
	})

	t.Run("declines a same-named column of another quantifier", func(t *testing.T) {
		t.Parallel()
		// O.ID: same leaf name, same ordinal (0), DIFFERENT correlation and
		// domain. This is the shape the name-keyed proof could not see at all.
		ref := nljBakedRef(t, "OUTER", values.NamedCorrelationIdentifier("O"), "ID")
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("O.ID matched INNER's primary key — two columns sharing a leaf name " +
				"were treated as one, which is the whole bug class RFC-197 exists to end")
		}
	})

	t.Run("declines a same-DOMAIN same-ordinal column of another quantifier", func(t *testing.T) {
		t.Parallel()
		// A self-join: baked against INNER's OWN layout, so the domain and the
		// ordinal both agree with the key, and only the correlation says this
		// reads a DIFFERENT INNER row. The `O.ID` case above cannot isolate the
		// correlation because OUTER's domain differs too and the frontier gate
		// rejects it first — measured: dropping the correlation check alone
		// left every other case in this suite green.
		otherLeg := nljBakedRef(t, "INNER", values.NamedCorrelationIdentifier("I2"), "ID")
		if readsKeyColumn(otherLeg, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("a reference to ANOTHER INNER row matched this leg's primary key — " +
				"ordinal 0 of two quantifiers are different columns, which is the " +
				"element a pair of (name, ordinal) can never carry")
		}
	})

	t.Run("declines a same-ordinal column of another DOMAIN, correlation held equal", func(t *testing.T) {
		t.Parallel()
		// Baked against SHADOW, whose "ID" is also ordinal 0, but stamped with
		// the INNER correlation. Correlation and ordinal both AGREE with the
		// key; only the domain differs. Dropping the domain check accepts this
		// and nothing else in the suite notices.
		ref := nljBakedRef(t, "SHADOW", innerCorr, "ID")
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("a SHADOW-domain ordinal 0 matched INNER's ordinal-0 primary key — " +
				"an ordinal compared across layouts is the same conflation as a name, " +
				"wearing a type that reads as authoritative")
		}
	})

	t.Run("declines a same-domain non-key column", func(t *testing.T) {
		t.Parallel()
		// I.OUTER_ID: right leg, right layout, WRONG column. Pins that the
		// comparison is to the key's ordinal and not merely to "some resolved
		// column of the inner leg".
		ref := nljBakedRef(t, "INNER", innerCorr, "OUTER_ID")
		if readsKeyColumn(ref, innerCorr, keyIdent, innerFrontier) {
			t.Fatal("I.OUTER_ID matched INNER's primary key ID — the probe would be built " +
				"on a non-key column and narrow the scan to the wrong records")
		}
	})

	t.Run("declines when the key was resolved in a DIFFERENT layout than the frontier", func(t *testing.T) {
		t.Parallel()
		// readsKeyColumn takes the frontier and the resolved key as two
		// INDEPENDENT arguments, and they are only meaningful together: a key
		// ordinal resolved in SHADOW says nothing about a comparand's ordinal
		// in INNER. The frontier gate inside CorrelatedIdentityIn cannot catch
		// this one — the comparand is a perfectly good INNER reference — so the
		// agreement between the two arguments is checked explicitly.
		//
		// This is the guard that keeps the per-site proof a predicate the
		// function checks rather than a comment the next caller does not read.
		shadowLayout := nljTestLayouts["SHADOW"]
		shadowKey, ok := values.OrdinalOfNameIn(shadowLayout, "ID")
		if !ok {
			t.Fatal("setup: SHADOW.ID must resolve against SHADOW's layout")
		}
		ref := nljBakedRef(t, "INNER", innerCorr, "ID")
		if readsKeyColumn(ref, innerCorr, shadowKey, innerFrontier) {
			t.Fatal("a key resolved in SHADOW's layout was matched against a comparand's " +
				"ordinal in INNER's — two ordinals from different layouts are not comparable, " +
				"and agreeing by accident is what the domain element exists to prevent")
		}
	})

	t.Run("rejects an untyped reference before key matching", func(t *testing.T) {
		t.Parallel()
		// RFC-232 makes the old lazy I.ID shape unpublishable. Pin the boundary,
		// then retain a direct non-field decline so the matcher cannot fall back
		// to an arbitrary value's display text.
		if root, err := values.NewQuantifiedObjectValue(innerCorr, values.UnknownType); err == nil || root != nil {
			t.Fatalf("untyped QOV = (%#v, %v), want constructor rejection", root, err)
		}
	})
}

// TestOrdinalOfNameIn_KeyResolutionIsCaseInsensitiveAndFailsClosed pins the
// boundary itself. The metadata layer names its columns and that name is
// resolved ONCE, here; if this resolution silently declined, every caller
// downstream would decline too and the fast path would vanish without a
// failing test — the quiet-regression shape RFC-197 warns about.
func TestOrdinalOfNameIn_KeyResolutionIsCaseInsensitiveAndFailsClosed(t *testing.T) {
	t.Parallel()

	innerLayout := nljTestLayouts["INNER"]

	if id, ok := values.OrdinalOfNameIn(innerLayout, "id"); !ok || id.Ordinal != 0 {
		t.Fatalf(`OrdinalOfNameIn(INNER, "id") = (%v, %v), want ordinal 0 — `+
			"metadata column lists do not agree with a record type's spelling on case", id, ok)
	}
	if id, ok := values.OrdinalOfNameIn(innerLayout, "NO_SUCH_COLUMN"); ok {
		t.Fatalf("OrdinalOfNameIn resolved a column INNER does not declare: %v", id)
	}
	if id, ok := values.OrdinalOfNameIn(values.UnknownType, "ID"); ok {
		t.Fatalf("OrdinalOfNameIn resolved against a layout with no declared column order: %v", id)
	}
}

// TestCorrelatedFastPathOperand_DeclinesLazyOuterRef pins a NEGATIVE result,
// and states what re-arms if it changes.
//
// The lazy arm used to rebuild the outer comparand as a bare
// `QOV(outer).<name>`. That is not a weaker operand, it is an UNEVALUABLE one:
// FieldValue.evaluateOrdinal has no runtime name-resolution fallback and
// returns OrdinalResolutionError{Ordinal: -1} for any unbaked reference
// (values.go:789-793). So the arm could only ever have produced a plan that
// fails loud at execution, which is why nothing in the corpus reaches it —
// not because the shape cannot occur, but because a query that took it would
// not have survived.
//
// If a runtime name read is ever introduced, this test keeps failing until
// someone decides deliberately whether the fast path may build one. It must
// not be "fixed" by re-adding the lazy construction.
func TestCorrelatedFastPathOperand_DeclinesLazyOuterRef(t *testing.T) {
	t.Parallel()

	outerCorr := values.NamedCorrelationIdentifier("O")
	if root, err := values.NewQuantifiedObjectValue(outerCorr, values.UnknownType); err == nil || root != nil {
		t.Fatalf("untyped outer QOV = (%#v, %v), want constructor rejection", root, err)
	}

	// Accept direction: a source-relative bake still transfers, carrying its
	// ordinal. Without this the decline above could be satisfied by refusing
	// everything.
	baked := nljBakedRef(t, "OUTER", outerCorr, "ID")
	operand, ok := correlatedFastPathOperand(baked, outerCorr)
	if !ok {
		t.Fatal("correlatedFastPathOperand declined a SOURCE-RELATIVE baked outer reference — " +
			"that is the arm the whole fast path runs on")
	}
	built, isFV := values.AsFieldValue(operand)
	if !isFV || built.Path() == nil || built.Path().Len() != 1 {
		t.Fatalf("rebuilt operand %#v lost its ordinal — the ordinal IS the identity here; "+
			"the display name beside it decides nothing", operand)
	}
	accessor, ok := built.Path().Accessor(0)
	if !ok || accessor.Ordinal() != 0 {
		t.Fatalf("rebuilt operand path %#v, want ordinal 0", built.Path().Ordinals())
	}
}

// nljTestLayouts are the declared column orders the EXISTS-shortcut scenarios
// resolve against. Stating them is the point of RFC-197: an ordinal means
// nothing without the layout it indexes, and the inner leg's layout is where
// the inner table's primary-key NAME is resolved exactly once.
//
// OUTER and INNER deliberately BOTH declare a column named "ID" at ordinal 0.
// That is the dimension every name-keyed proof in this file was blind to: with
// the leaf name as the key the two are one column, and with the ordinal alone
// (no domain, no correlation) they are still one column. Only the full triple
// tells them apart.
var nljTestLayouts = map[string]*values.RecordType{
	"OUTER": nljTestLayout("OUTER", "ID", "CATEGORY"),
	"INNER": nljTestLayout("INNER", "ID", "OUTER_ID"),
	// SHADOW has "ID" at ordinal 0 exactly as INNER does, so an operand baked
	// against it is ordinal-equal and correlation-equal to a real INNER.ID
	// reference and differs ONLY in the domain. It is what isolates the domain
	// check from the correlation check under mutation.
	"SHADOW": nljTestLayout("SHADOW", "ID", "NOTE"),
	// The inner leg of the secondary-index shortcut scenario, whose join key
	// is a foreign key rather than the primary key.
	"INNERFK": nljTestLayout("INNER", "FK", "STATUS", "TAGS"),
}

func nljTestLayout(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NullableLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// nljBakedRef builds the production shape of a resolved column reference:
// correlated to alias, carrying the ordinal the column has in rt's declared
// column order, and stamped with rt's domain (the SQL resolver's
// sourceColumnOrdinal derives ordinal and domain in one breath). A column rt
// does not declare stays LAZY, which is what a reference outside that row
// really looks like and what the identity proofs decline.
func nljBakedRef(t *testing.T, rt string, alias values.CorrelationIdentifier, field string) values.FieldValue {
	t.Helper()
	layout, known := nljTestLayouts[rt]
	if !known {
		t.Fatalf("setup: no layout registered for %q", rt)
	}
	ord, found := layout.FieldIndexUnique(field)
	if !found {
		t.Fatalf("setup: layout %q does not declare field %q", rt, field)
		return nil
	}
	root := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, layout))
	return mustNLJField(mustNLJConstruct(values.ResolveFieldOrdinals(root, []int{ord})))
}

// buildExistsPKShortcutScenario assembles `SELECT * FROM OUTER O WHERE
// EXISTS (SELECT 1 FROM INNER I WHERE <innerOperand> = O.ID)` — the shape
// tryExistsFlatMap's PK-shortcut branch tries to rewrite into a correlated
// PK-narrowed scan. The inner table's declared primary key is the flat
// top-level column "ID" (via pkGateTestCtx).
//
// Both leaf scans flow their table's declared row type. A leaf that flowed
// UnknownType would have no domain, and the whole fast path fails closed on
// that — so an untyped scenario could not distinguish "declined because the
// identity says no" from "declined because there was no layout to ask", which
// is precisely the confusion the mutation checks have to avoid.
func buildExistsPKShortcutScenario(t *testing.T, innerOperand values.FieldValue) []expressions.RelationalExpression {
	t.Helper()
	return buildExistsPKShortcutScenarioWithOuter(t, innerOperand, nil)
}

// buildExistsPKShortcutScenarioWithOuter is buildExistsPKShortcutScenario with
// the OUTER-side comparand supplied too; nil means the ordinary baked O.ID.
func buildExistsPKShortcutScenarioWithOuter(
	t *testing.T,
	innerOperand values.FieldValue,
	outerOperand values.FieldValue,
) []expressions.RelationalExpression {
	t.Helper()

	outerAlias := values.NamedCorrelationIdentifier("O")
	innerAlias := values.NamedCorrelationIdentifier("I")

	outerRowType := values.Type(nljTestLayouts["OUTER"])
	innerRowType := values.Type(nljTestLayouts["INNER"])

	outerScan := mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, outerRowType))
	outerRef := expressions.InitialOf(outerScan)
	outerRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerRowType, false)))

	innerScan := mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"INNER"}, innerRowType))
	innerRef := expressions.InitialOf(innerScan)
	innerRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"INNER"}, innerRowType, false)))

	outerQ := expressions.NamedForEachQuantifier(outerAlias, outerRef)
	innerQ := expressions.NamedExistentialQuantifier(innerAlias, innerRef)

	if outerOperand == nil {
		outerOperand = nljBakedRef(t, "OUTER", outerAlias, "ID")
	}
	joinPredicate := predicates.NewComparisonPredicate(
		innerOperand,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerOperand},
	)
	selectExpr := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(
		nljFlowed(outerQ),
		[]expressions.Quantifier{outerQ, innerQ},
		[]predicates.QueryPredicate{
			joinPredicate,
			mustExistentialAlias(t, innerAlias),
		},
		[]string{"O", "I"},
	))

	ctx := &pkGateTestCtx{pk: []string{"ID"}}
	return mustFireExpressionRuleWithMemo(t,
		NewImplementNestedLoopJoinRule(),
		expressions.InitialOf(selectExpr),
		ctx,
		nil,
	)
}

// anyScanCarriesComparisons reports whether any RecordQueryScanPlan reachable
// from any produced alternative carries non-empty ScanComparisons — the
// marker that the EXISTS PK shortcut fired and narrowed the inner scan to a
// correlated PK probe (WithScanComparisons is the shortcut's only producer of
// scan comparisons in this scenario: both leaf scans start comparison-free).
func anyScanCarriesComparisons(t *testing.T, results []expressions.RelationalExpression) bool {
	t.Helper()
	found := false
	for _, r := range results {
		rp, ok := r.(plans.RecordQueryPlan)
		if !ok {
			continue
		}
		plans.Walk(rp, func(p plans.RecordQueryPlan) bool {
			if sp, ok := p.(*plans.RecordQueryScanPlan); ok && len(sp.GetScanComparisons()) > 0 {
				found = true
			}
			return true
		})
	}
	return found
}

// TestImplementNestedLoopJoin_ExistsPKShortcutDimensions carries the identity
// dimensions up to the RULE level, where the consequence is a plan rather than
// a boolean: the inner scan either gets narrowed to a correlated probe or it
// does not.
func TestImplementNestedLoopJoin_ExistsPKShortcutDimensions(t *testing.T) {
	t.Parallel()

	innerCorr := values.NamedCorrelationIdentifier("I")
	outerCorr := values.NamedCorrelationIdentifier("O")

	t.Run("declines a same-named column of the OUTER leg on the inner side", func(t *testing.T) {
		t.Parallel()
		// `O.ID = O.ID`: the "inner" side is really an OUTER reference that
		// merely spells its column the way INNER's primary key is spelled.
		// Under a leaf-name match this is a PK equi-join and the rule builds a
		// correlated probe of INNER on a value that never reads INNER at all.
		//
		// This case does NOT isolate the correlation: the operand is baked
		// against OUTER's layout, so the domain refuses it first and a
		// correlation-blind rule would still decline here. The sibling case
		// below is the one that removes that cover.
		results := buildExistsPKShortcutScenarioWithOuter(t,
			nljBakedRef(t, "OUTER", outerCorr, "ID"),
			nljBakedRef(t, "OUTER", outerCorr, "ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on an OUTER reference sharing the inner PK's " +
				"leaf name — the probe narrows INNER by a column of a different table")
		}
	})

	t.Run("declines an INNER-DOMAIN reference read off the OUTER leg", func(t *testing.T) {
		t.Parallel()
		// The correlation ISOLATED, at rule level. The "inner" operand is baked
		// against INNER's OWN layout at INNER's primary-key ordinal, so the
		// domain check and the ordinal check both pass; only the quantifier
		// says it reads the outer's row. This is the self-join shape — two legs
		// sharing one layout — and it is the only case in this suite a
		// correlation-blind rule reaches.
		//
		// Firing here builds a correlated PK-narrowed scan of INNER whose bound
		// value never reads INNER at all, which is a wrong-rows plan, not a
		// slower one.
		inner := nljBakedRef(t, "INNER", outerCorr, "ID")
		innerField, ok := values.AsFieldValue(inner)
		if !ok || innerField.Path().RootDomain() != values.OrdinalDomainOfType(nljTestLayouts["INNER"]) {
			t.Fatal("setup: the operand must carry INNER's own domain, or the domain check rejects it first")
		}
		results := buildExistsPKShortcutScenarioWithOuter(t, inner,
			nljBakedRef(t, "OUTER", outerCorr, "ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on a reference baked in INNER's layout but read " +
				"off the OUTER quantifier — ordinal 0 of two quantifiers are different columns, " +
				"and the probe would narrow INNER by a value that never reads it")
		}
	})

	t.Run("declines a same-named column of another DOMAIN under the inner alias", func(t *testing.T) {
		t.Parallel()
		// Correlated to I and ordinal 0, exactly like a real I.ID, but baked
		// against SHADOW's layout. Only the domain separates it from the
		// accepted case, so this is what a domain-dropped mutation reaches.
		results := buildExistsPKShortcutScenario(t, nljBakedRef(t, "SHADOW", innerCorr, "ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on an ordinal baked against a different layout — " +
				"the probe's slot was chosen in a row the inner leg does not flow")
		}
	})

	t.Run("declines a same-domain NON-key column", func(t *testing.T) {
		t.Parallel()
		// I.OUTER_ID is a genuine, correctly-baked inner column — it is simply
		// not the primary key. The shortcut may only narrow a scan by the key
		// it claims to be probing.
		results := buildExistsPKShortcutScenario(t, nljBakedRef(t, "INNER", innerCorr, "OUTER_ID"))
		if anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut fired on a non-key inner column — the correlated " +
				"scan would be narrowed by the wrong column")
		}
	})

	t.Run("fires on the real key column", func(t *testing.T) {
		t.Parallel()
		results := buildExistsPKShortcutScenario(t, nljBakedRef(t, "INNER", innerCorr, "ID"))
		if len(results) == 0 {
			t.Fatal("setup: expected at least one physical alternative")
		}
		if !anyScanCarriesComparisons(t, results) {
			t.Fatal("EXISTS PK shortcut did not fire on a genuine baked PK equi-join — " +
				"the identity conversion must not over-decline the case the shortcut exists for")
		}
	})
}

// TestImplementNestedLoopJoin_ExistsPKShortcutDeclinesFusedNestedSameLeafName
// pins the wrong-rows hazard the bare-column allowlist guards against at the
// RULE level, not just the matcher unit: an EXISTS join
// predicate whose INNER operand is a FUSED nested reference (i.address.id)
// sharing its LEAF name with the inner table's own top-level primary key
// ("ID") must NOT be mistaken for a PK equi-join and used to build a
// correlated PK-narrowed scan — that scan would filter on the top-level ID
// column while the query actually asked about a nested field, silently
// changing which rows the EXISTS reports as present.
func TestImplementNestedLoopJoin_ExistsPKShortcutDeclinesFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	innerNestedID := fusedNestedFieldValue(values.NamedCorrelationIdentifier("I"), "ADDRESS", "ID")
	results := buildExistsPKShortcutScenario(t, innerNestedID)
	if len(results) == 0 {
		t.Fatal("setup: expected at least one physical alternative from ImplementNestedLoopJoinRule")
	}
	if anyScanCarriesComparisons(t, results) {
		t.Fatal("EXISTS PK shortcut fired on a fused nested reference sharing the PK's leaf name — " +
			"built a correlated PK-narrowed scan that filters the WRONG column (wrong EXISTS rows)")
	}
}

// TestImplementNestedLoopJoin_ExistsPKShortcutFiresOnBareTopLevelColumn is the
// accept-direction companion at the rule level: a genuine bare top-level PK
// join (i.id = o.id) must still take the correlated PK-shortcut fast path —
// proving the fix declines ONLY the fused nested shape, not the ordinary case
// the shortcut exists to serve.
func TestImplementNestedLoopJoin_ExistsPKShortcutFiresOnBareTopLevelColumn(t *testing.T) {
	t.Parallel()

	innerID := nljBakedRef(t, "INNER", values.NamedCorrelationIdentifier("I"), "ID")
	results := buildExistsPKShortcutScenario(t, innerID)
	if len(results) == 0 {
		t.Fatal("setup: expected at least one physical alternative from ImplementNestedLoopJoinRule")
	}
	if !anyScanCarriesComparisons(t, results) {
		t.Fatal("EXISTS PK shortcut did not fire on a genuine bare top-level PK join — the fix must not over-decline")
	}
}

// TestBuriedLegOrdinalLayout_SkipsFusedNestedSameLeafName pins that a FUSED
// nested reference (leg.address.id, leaf "ID") never claims the slot the leg's
// GENUINE bare column owns. Under the retired leaf-name key both slots spelled
// "LEG.ID" and "first occurrence wins" let the fused slot's ordinal answer for
// the real column, misdirecting a buried-leg rebase to the wrong RC slot. The
// identity key removes the collision at the root — a fused multi-accessor path
// states no single-accessor identity, so it mints no key at all — and the bare
// column is the one owner of its own.
func TestBuriedLegOrdinalLayout_SkipsFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	leg := values.NamedCorrelationIdentifier("LEG")
	fused := fusedNestedFieldValue(leg, "ADDRESS", "ID")
	legType := nljTestLayout("LEG", "ADDRESS", "ID")
	legRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(leg, legType))
	bare := mustNLJField(mustNLJConstruct(values.ResolveFieldOrdinals(legRoot, []int{1})))

	rc := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "F0", Value: fused},
		values.RecordConstructorField{Name: "F1", Value: bare},
	)

	outer := nljPhysicalScan("OUTER")
	inner := nljPhysicalScan("INNER")
	fm := mustNLJConstruct(plans.NewRecordQueryFlatMapPlan(
		outer, inner, leg, values.NamedCorrelationIdentifier("i"), rc, false))

	layout := buriedLegOrdinalLayout(fm)
	if layout == nil {
		t.Fatal("setup: expected a non-nil layout")
	}
	id, stated := legSlotIdentity(bare)
	if !stated {
		t.Fatal("setup: the genuine bare column must state an identity")
	}
	ord, ok := layout[id]
	if !ok {
		t.Fatal("expected the genuine bare field's identity to be present, got missing")
	}
	if ord != 1 {
		t.Fatalf("layout[bare LEG.ID] = %d, want 1 (the genuine bare field at slot 1) — "+
			"the fused nested field at slot 0 must not have claimed this key first", ord)
	}
	if _, fusedStated := legSlotIdentity(fused); fusedStated {
		t.Fatal("a FUSED multi-accessor path must state no single-column identity, so it can " +
			"never mint a layout key")
	}
}

// TestRebaseOuterLegValue_DeclinesFusedNestedSameLeafName pins the same hole
// in rebaseOuterLegValue's own leg-match arm: a fused nested outer reference
// (leg.address.id) whose leaf name ("ID") collides with a plain column must
// not be rewritten into the qualified key "LEG.ID" — that would silently
// discard the ADDRESS accessor and re-anchor the predicate onto the WRONG
// column of the merged row. The node must come back unchanged (declined),
// exactly like any other shape this rebase does not recognize.
func TestRebaseOuterLegValue_DeclinesFusedNestedSameLeafName(t *testing.T) {
	t.Parallel()

	leg := "LEG"
	fused := fusedNestedFieldValue(values.NamedCorrelationIdentifier(leg), "ADDRESS", "ID")
	mergedCorr := values.NamedCorrelationIdentifier("MERGED")

	got := rebaseOuterLegValue(
		fused, []string{leg}, mergedCorr, nil, nil,
		map[values.CorrelationIdentifier]*values.RecordType{
			values.NamedCorrelationIdentifier(leg): nljTestLayout("LEG", "ADDRESS", "ID"),
		},
		legRebaseOrigin{Site: legRebaseSiteExists})
	if _, ok := values.AsFieldValue(got); !ok || got != fused {
		t.Fatalf("rebaseOuterLegValue(fused LEG.ADDRESS.ID, nil, legRebaseOrigin{Site: legRebaseSiteExists}) = %#v, want the ORIGINAL unrewritten node — "+
			"a nested reference must never be re-anchored onto a colliding leaf-name qualified key", got)
	}
}
