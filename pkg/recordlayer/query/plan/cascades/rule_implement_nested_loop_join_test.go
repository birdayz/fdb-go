package cascades

import (
	"strings"
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
	if !merged.Complete() {
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
		call,
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

func TestTranslateExistentialWholeRowAccessPrograms(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("X")
	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "X", FieldType: values.NotNullInt},
	})
	declaration := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, rowType))
	replacement := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.UniqueCorrelationIdentifier(), rowType))
	field := mustNLJConstruct(values.ResolveFieldOrdinals(declaration, []int{0}))
	scalar := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, values.NotNullInt))
	foreign := mustNLJConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN"), rowType))
	wrong := mustNLJConstruct(values.NewQuantifiedObjectValue(alias, values.NotNullLong))
	rangeOf := func(comparisons ...*predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		rangeValue := predicates.EmptyComparisonRange()
		for _, comparison := range comparisons {
			merged := rangeValue.Merge(comparison)
			if !merged.Complete() {
				t.Fatal("construct comparison range")
			}
			rangeValue = merged.Range
		}
		return rangeValue
	}
	ranges := []*predicates.ComparisonRange{
		rangeOf(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: field}),
		rangeOf(&predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: field},
			&predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: field}),
	}
	for _, value := range []values.Value{scalar, foreign, wrong} {
		ranges = append(ranges, rangeOf(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: value}))
	}
	scan := nljPhysicalScan("INNER").WithScanComparisons(ranges)
	index := mustNLJConstruct(plans.NewRecordQueryIndexPlan(
		"IDX", ranges, []string{"INNER"}, nljSimpleRowType("INNER"), true))
	covering := mustNLJConstruct(plans.NewRecordQueryCoveringIndexPlan(index))
	filterAlias := values.NamedCorrelationIdentifier("INNER_EDGE")
	filter := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		covering, []predicates.QueryPredicate{predicates.NewComparisonPredicate(
			field, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: scalar})}, filterAlias))
	fetch := mustNLJConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		filter, nil, nljSimpleRowType("INNER"), plans.FetchIndexRecordsPrimaryKey))
	legacy := mustNLJConstruct(plans.NewRecordQueryFilterPlan(filter.GetPredicates(), scan))
	typeFilter := mustNLJConstruct(plans.NewRecordQueryTypeFilterPlan([]string{"INNER"}, legacy))
	projection := mustNLJConstruct(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{field, scalar}, []string{"OUTER_ID", "ELEMENT"}, typeFilter))
	strict := mustNLJConstruct(plans.NewRecordQueryFirstOrDefaultPlanStrict(
		scan, values.NewNullValue(values.WithNullability(scan.GetResultType(), true))))

	for _, test := range []struct {
		name string
		plan plans.RecordQueryPlan
	}{
		{name: "scan", plan: scan},
		{name: "index", plan: index},
		{name: "fetch_filter_covering", plan: fetch},
		{name: "type_filter_legacy_filter", plan: typeFilter},
		{name: "projection", plan: projection},
		{name: "strict_first_or_default", plan: strict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			translated := mustNLJConstruct(translateExistentialWholeRowPlanPredicates(
				test.plan, declaration, replacement))
			if translated == test.plan {
				t.Fatal("whole-row scan operand was not translated")
			}
			var check func(plans.RecordQueryPlan, plans.RecordQueryPlan)
			leaves := 0
			check = func(before, after plans.RecordQueryPlan) {
				t.Helper()
				oldQuants, newQuants := before.GetQuantifiers(), after.GetQuantifiers()
				if len(oldQuants) != len(newQuants) {
					t.Fatalf("quantifier arity changed for %T", before)
				}
				for i, quantifier := range oldQuants {
					if quantifier.GetAlias() != newQuants[i].GetAlias() || quantifier.Kind() != newQuants[i].Kind() ||
						quantifier.GetRangesOver().Stage() != newQuants[i].GetRangesOver().Stage() {
						t.Fatalf("child identity changed for %T edge %d", before, i)
					}
				}
				if probe, ok := after.(interface {
					GetScanComparisons() []*predicates.ComparisonRange
				}); ok {
					leaves++
					got := probe.GetScanComparisons()
					for _, comparison := range append([]*predicates.Comparison{got[0].GetEqualityComparison()}, got[1].GetInequalityComparisons()...) {
						operand, ok := values.AsFieldValue(comparison.Operand)
						if !ok || operand.ChildValue() != replacement {
							t.Fatalf("scan operand = %v, want exact replacement root", comparison.Operand)
						}
					}
					for i := 2; i < len(ranges); i++ {
						if got[i] != ranges[i] {
							t.Fatalf("unrelated range %d was rebuilt", i)
						}
					}
				}
				if residual, ok := after.(interface {
					GetPredicates() []predicates.QueryPredicate
				}); ok {
					comparison := residual.GetPredicates()[0].(*predicates.ComparisonPredicate)
					operand, ok := values.AsFieldValue(comparison.Operand)
					if !ok || operand.ChildValue() != replacement || comparison.Comparison.Operand != scalar {
						t.Fatalf("residual lost whole-row/scalar distinction: %v", comparison)
					}
				}
				if afterFilter, ok := after.(*plans.RecordQueryPredicatesFilterPlan); ok && afterFilter.GetInnerAlias() != filterAlias {
					t.Fatal("filter binding alias changed")
				}
				if projected, ok := after.(*plans.RecordQueryProjectionPlan); ok {
					operand, isField := values.AsFieldValue(projected.GetProjections()[0])
					if !isField || operand.ChildValue() != replacement || projected.GetProjections()[1] != scalar {
						t.Fatal("projection lost whole-row/scalar distinction")
					}
				}
				if first, ok := after.(*plans.RecordQueryFirstOrDefaultPlan); ok &&
					(!first.IsStrict() || first.GetDefaultValue() != strict.GetDefaultValue()) {
					t.Fatal("first-or-default lost its strict/default contract")
				}
				oldChildren, newChildren := before.GetChildren(), after.GetChildren()
				if len(oldChildren) != len(newChildren) {
					t.Fatalf("child arity changed for %T", before)
				}
				for i := range oldChildren {
					check(oldChildren[i], newChildren[i])
				}
			}
			check(test.plan, translated)
			if leaves != 1 {
				t.Fatalf("checked %d scan leaves, want 1", leaves)
			}
			originalField, ok := values.AsFieldValue(field)
			if !ok || originalField.ChildValue() != declaration || scan.GetScanComparisons()[0] != ranges[0] ||
				index.GetScanComparisons()[1] != ranges[1] || filter.GetInner() != covering || fetch.GetInner() != filter {
				t.Fatal("translation mutated the source access path")
			}
			unchanged := mustNLJConstruct(translateExistentialWholeRowPlanPredicates(
				test.plan, foreign, foreign))
			if unchanged != test.plan {
				t.Fatal("identity translation rebuilt an access path")
			}
			if result, err := translateExistentialWholeRowPlanPredicates(test.plan, declaration, wrong); result != nil || err == nil {
				t.Fatalf("incompatible scan translation = (%v, %v), want nil,error", result, err)
			}
		})
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
		logicalProjection := mustNLJConstruct(newBlockSelectForTest(
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
// guard must continue to decline the MATERIALIZED join. What it yields instead
// is Java's only binary implementation, a FlatMap, which is valid exactly
// where D is bound and says so: it reports D upward, so it can never be
// chosen where D is unbound (two lateral siblings correlated to one earlier
// leg, `FROM w, (… w.f …) AS d, (… w.f …) AS e`, plan only through it).
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
		logicalProjection := mustNLJConstruct(newBlockSelectForTest(
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
	sharedSelect := func(joinType expressions.JoinType) *expressions.SelectExpression {
		return mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "LEFT_D", Value: nljField(leftQ, 0)},
				values.RecordConstructorField{Name: "RIGHT_D", Value: nljField(rightQ, 0)},
			),
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			joinType,
		))
	}
	selectExpr := sharedSelect(expressions.JoinCross)

	// A LEFT OUTER select over the same legs keeps the left leg as the outer
	// and null-extends the right: its FlatMap's inner is the DefaultOnEmpty.
	// (It projects the preserved leg; a read of the null-supplying leg would be
	// typed nullable, as the translator's seed types it.)
	leftOuter := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "LEFT_D", Value: nljField(leftQ, 0)},
		),
		[]expressions.Quantifier{leftQ, rightQ},
		nil,
		[]string{"L", "R"},
		expressions.JoinLeftOuter,
	))
	leftOuterResults := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(leftOuter))
	if len(leftOuterResults) == 0 {
		t.Fatal("a LEFT OUTER over a shared named external sibling yielded nothing; the FlatMap is its only implementation")
	}
	for _, result := range leftOuterResults {
		flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
		if !isFlatMap {
			t.Fatalf("a LEFT OUTER over a shared named external sibling yielded %T; only a FlatMap is safe", result)
		}
		if _, wrapped := flatMap.GetInner().(*plans.RecordQueryDefaultOnEmptyPlan); !wrapped || flatMap.GetOuterAlias().Name() != "L" {
			t.Fatalf("a LEFT OUTER's FlatMap must keep L the outer and null-extend R: %s", flatMap.Explain())
		}
	}

	// A null-on-empty leg of an INNER select over the same legs (a lower that
	// separated a rewritten null-on-empty leg from its partner, on either side)
	// is extended where it sits: the INNER select yields both orientations, and
	// in each exactly the flagged leg is wrapped in DefaultOnEmpty, as outer or
	// as inner.
	for _, nullOnEmptyLeft := range []bool{true, false} {
		left, right := leftQ, rightQ
		if nullOnEmptyLeft {
			left = expressions.NamedForEachNullOnEmptyQuantifier(leftQ.GetAlias(), leftQ.GetRangesOver())
		} else {
			right = expressions.NamedForEachNullOnEmptyQuantifier(rightQ.GetAlias(), rightQ.GetRangesOver())
		}
		sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "LEFT_D", Value: nljField(left, 0)},
				values.RecordConstructorField{Name: "RIGHT_D", Value: nljField(right, 0)},
			),
			[]expressions.Quantifier{left, right},
			nil,
			[]string{"L", "R"},
			expressions.JoinInner,
		))
		nullOnEmptyResults := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
		if len(nullOnEmptyResults) == 0 {
			t.Fatalf("null-on-empty left=%v over a shared named external sibling yielded nothing", nullOnEmptyLeft)
		}
		flagged := "R"
		if nullOnEmptyLeft {
			flagged = "L"
		}
		outers := map[string]bool{}
		for _, result := range nullOnEmptyResults {
			flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
			if !isFlatMap {
				t.Fatalf("null-on-empty %s yielded %T; only a FlatMap is safe", flagged, result)
			}
			outer := flatMap.GetOuterAlias().Name()
			outers[outer] = true
			_, outerWrapped := flatMap.GetOuter().(*plans.RecordQueryDefaultOnEmptyPlan)
			_, innerWrapped := flatMap.GetInner().(*plans.RecordQueryDefaultOnEmptyPlan)
			if outerWrapped != (outer == flagged) || innerWrapped == (outer == flagged) {
				t.Fatalf("null-on-empty %s with %s the outer: outer wrapped %v, inner wrapped %v; want exactly "+
					"the flagged leg wrapped, wherever it sits: %s", flagged, outer, outerWrapped, innerWrapped, flatMap.Explain())
			}
		}
		if !outers["L"] || !outers["R"] {
			t.Fatalf("null-on-empty %s: outers %v, want both orientations of the INNER select", flagged, outers)
		}
	}

	results := mustFireExpressionRule(
		t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(selectExpr))
	if len(results) == 0 {
		t.Fatal("shared named external sibling yielded nothing; the correlated FlatMap is its implementation")
	}
	for _, result := range results {
		flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
		if !isFlatMap {
			t.Fatalf("shared named external sibling yielded %T; only a FlatMap is safe", result)
		}
		correlatedTo := expressions.InitialOf(flatMap).GetCorrelatedTo()
		if _, reported := correlatedTo[externalRoot.Correlation()]; !reported {
			t.Fatalf("the FlatMap does not report its correlation to D (%v), so it could be "+
				"chosen where D is unbound", correlatedTo)
		}
	}
}

func TestImplementExistentialJoinHonorsDependencyDirection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                       string
		outerDepends, innerDepends bool
	}{
		{"independent", false, false},
		{"inner_depends_on_outer", false, true},
		{"outer_depends_on_inner", true, false},
		{"cycle", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outerAlias, innerAlias := values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier()
			outerScan, innerScan := nljPhysicalScan("OUTER"), nljPhysicalScan("INNER")
			var outer, inner plans.RecordQueryPlan = outerScan, innerScan
			withDependency := func(plan, dependency plans.RecordQueryPlan, own, free values.CorrelationIdentifier) plans.RecordQueryPlan {
				qov := mustNLJConstruct(values.NewQuantifiedObjectValue(free, dependency.GetResultType()))
				return mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(plan,
					[]predicates.QueryPredicate{predicates.NewComparisonPredicate(qov, predicates.Comparison{Type: predicates.ComparisonIsNotNull})}, own))
			}
			if tc.outerDepends {
				outer = withDependency(outer, innerScan, outerAlias, innerAlias)
			}
			if tc.innerDepends {
				inner = withDependency(inner, outerScan, innerAlias, outerAlias)
			}
			outerQ := expressions.NamedForEachQuantifier(outerAlias, expressions.FinalOf(outer))
			innerQ := expressions.NamedExistentialQuantifier(innerAlias, expressions.FinalOf(inner))
			sel := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(nljFlowed(outerQ),
				[]expressions.Quantifier{outerQ, innerQ}, []predicates.QueryPredicate{mustExistentialAlias(t, innerAlias)},
				[]string{outerAlias.Name(), innerAlias.Name()}))
			results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
			if tc.outerDepends && tc.innerDepends {
				if len(results) != 0 {
					t.Fatalf("yielded %d plans with a dependency cycle", len(results))
				}
			} else if len(results) == 0 {
				t.Fatal("a valid existential dependency direction yielded no implementation")
			}
			if tc.outerDepends && !tc.innerDepends {
				for _, result := range results {
					flatMap, ok := result.(*plans.RecordQueryFlatMapPlan)
					if !ok || flatMap.GetOuterAlias() != innerAlias || flatMap.GetInnerAlias() != outerAlias {
						t.Fatalf("dependent ForEach must execute after the existential witness: %T", result)
					}
					if _, wrapped := flatMap.GetOuter().(*plans.RecordQueryFirstOrDefaultPlan); !wrapped {
						t.Fatalf("existential outer lacks FirstOrDefault: %s", flatMap.Explain())
					}
					if flatMap.InheritOuterRecordProperties() {
						t.Fatal("ordinary inner must not inherit existential cardinality")
					}
				}
			}
		})
	}
}

// A source alias is display metadata. When the existential inner reads the
// outer quantifier, binding the outer under a different source name would leave
// the inner's correlation unbound at execution.
func TestImplementExistentialJoinKeepsTheOuterBindingTheInnerReads(t *testing.T) {
	t.Parallel()
	outerAlias, innerAlias := values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier()
	outerScan, innerScan := nljPhysicalScan("OUTER"), nljPhysicalScan("INNER")
	qov := mustNLJConstruct(values.NewQuantifiedObjectValue(outerAlias, outerScan.GetResultType()))
	inner := mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(innerScan,
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(qov, predicates.Comparison{Type: predicates.ComparisonIsNotNull})}, innerAlias))
	outerQ := expressions.NamedForEachQuantifier(outerAlias, expressions.FinalOf(outerScan))
	innerQ := expressions.NamedExistentialQuantifier(innerAlias, expressions.FinalOf(inner))
	sel := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(nljFlowed(outerQ),
		[]expressions.Quantifier{outerQ, innerQ}, []predicates.QueryPredicate{mustExistentialAlias(t, innerAlias)},
		[]string{"DERIVED_NAME", innerAlias.Name()}))
	results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	if len(results) == 0 {
		t.Fatal("no implementation")
	}
	for _, result := range results {
		flatMap, ok := result.(*plans.RecordQueryFlatMapPlan)
		if !ok || flatMap.GetOuterAlias() != outerAlias {
			t.Fatalf("outer bound as %v, but the inner reads %v: %T", flatMap.GetOuterAlias(), outerAlias, result)
		}
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
	// Java's ImplementNestedLoopJoinRule implements any binary select; an
	// uncorrelated scalar Explode (an IN list, a FROM-less singleton) is joined
	// like any leg, and ImplementInJoinRule's InJoin competes on cost.
	if got := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(scalarSelect)); len(got) == 0 {
		t.Fatal("scalar uncorrelated Explode yielded no NLJ alternative")
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
		assertProducerPhysicalQuantifiers(t, result)
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

// A partitioned value can carry several conjuncts, or several alternative
// ranges. Only a conjunctive equality may become a probe, and consuming it
// must leave every other constraint below FirstOrDefault.
func TestImplementNestedLoopJoin_ExistsPartitionedRanges(t *testing.T) {
	t.Parallel()
	for _, access := range []string{"primary", "secondary"} {
		for _, shape := range []string{"conjunction", "disjunction"} {
			for _, mode := range []string{"exists", "not_exists"} {
				t.Run(access+"/"+shape+"/"+mode, func(t *testing.T) {
					t.Parallel()
					o, i := values.NamedCorrelationIdentifier("O"), values.NamedCorrelationIdentifier("I")
					outerType, innerType := nljTestLayouts["OUTER"], nljTestLayouts["INNER"]
					outerRef := expressions.InitialOf(mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, outerType)))
					innerRef := expressions.InitialOf(mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"INNER"}, innerType)))
					outerRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerType, false)))
					innerRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"INNER"}, innerType, false)))
					oq, iq := expressions.NamedForEachQuantifier(o, outerRef), expressions.NamedExistentialQuantifier(i, innerRef)
					innerID, outerID := nljBakedRef(t, "INNER", i, "ID"), nljBakedRef(t, "OUTER", o, "ID")
					equality := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerID}
					bound := predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(10))}
					regular := []predicates.QueryPredicate{
						predicates.NewComparisonPredicate(innerID, equality),
						predicates.NewComparisonPredicate(innerID, bound),
					}
					if shape == "disjunction" {
						regular = []predicates.QueryPredicate{predicates.NewPredicateWithValueAndRanges(innerID, []*predicates.RangeConstraints{
							predicates.NewRangeConstraints(nil, []predicates.Comparison{equality}),
							predicates.NewRangeConstraints([]predicates.Comparison{bound}, nil),
						})}
					}
					var exists predicates.QueryPredicate = mustExistentialAlias(t, i)
					wantPresence := predicates.ComparisonIsNotNull
					if mode == "not_exists" {
						exists = predicates.NewNot(exists)
						wantPresence = predicates.ComparisonIsNull
					}
					outerBound := predicates.NewComparisonPredicate(nljBakedRef(t, "OUTER", o, "CATEGORY"),
						predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))})
					sel := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(nljFlowed(oq),
						[]expressions.Quantifier{oq, iq}, append(regular, exists, outerBound), []string{"O", "I"}))
					partitionedInner := false
					for _, predicate := range sel.GetPredicates() {
						pvr, ok := predicate.(*predicates.PredicateWithValueAndRanges)
						if !ok || !values.SemanticEqualsUnderAliasMap(pvr.GetValue(), innerID, nil) {
							continue
						}
						partitionedInner = true
						wantRanges := 1
						if shape == "disjunction" {
							wantRanges = 2
						}
						if len(pvr.GetRanges()) != wantRanges || len(pvr.GetComparisons()) != 2 {
							t.Fatalf("fixture lost the partitioned range shape: %s", pvr.Explain())
						}
					}
					if !partitionedInner {
						t.Fatal("fixture did not partition the inner predicates into PVR")
					}
					ctx := nljPrimaryKeyPlanContext{PlanContext: NewPlanContextFromMatchCandidates(nil), primaryKey: []string{"ID"}}
					if access == "secondary" {
						scalar := false
						candidate := NewValueIndexScanMatchCandidateWithFunctions("INNER$ID", []string{"INNER"}, []string{"ID"}, nil,
							[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}, innerType, false, nil, &scalar).
							WithKeyComponentTypes([]values.Type{values.NullableLong})
						ctx.PlanContext = NewPlanContextFromMatchCandidates([]MatchCandidate{candidate})
						ctx.primaryKey = nil
					}
					results := mustFireExpressionRuleWithMemo(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel), ctx, nil)
					if len(results) == 0 {
						t.Fatal("partitioned existential yielded no plan")
					}
					for _, result := range results {
						assertProducerPhysicalQuantifiers(t, result)
						flatMap, ok := result.(*plans.RecordQueryFlatMapPlan)
						if !ok {
							t.Fatalf("existential implementation = %T, want FlatMap", result)
						}
						outerFilter, ok := flatMap.GetOuter().(*plans.RecordQueryPredicatesFilterPlan)
						if !ok || len(outerFilter.GetPredicates()) != 1 {
							t.Fatalf("outer constraint was lost or moved below FirstOrDefault: %s", flatMap.Explain())
						}
						presence, ok := flatMap.GetInner().(*plans.RecordQueryPredicatesFilterPlan)
						if !ok || len(presence.GetPredicates()) != 1 {
							t.Fatalf("missing existential residual: %s", flatMap.Explain())
						}
						presencePredicate, ok := presence.GetPredicates()[0].(*predicates.ComparisonPredicate)
						if !ok || presencePredicate.Comparison.Type != wantPresence {
							t.Fatalf("existential polarity changed: %s", presence.Explain())
						}
						fod, ok := presence.GetInner().(*plans.RecordQueryFirstOrDefaultPlan)
						if !ok {
							t.Fatalf("missing FirstOrDefault below presence test: %s", presence.Explain())
						}
						filter, ok := fod.GetInner().(*plans.RecordQueryPredicatesFilterPlan)
						if !ok || len(filter.GetPredicates()) != 1 {
							t.Fatalf("remaining range constraint must filter below FirstOrDefault: %s", fod.Explain())
						}
						residual := filter.GetPredicates()[0]
						if shape == "conjunction" {
							comparison, ok := residual.(*predicates.ComparisonPredicate)
							if !ok || comparison.Comparison.Type != bound.Type || !values.SemanticEqualsUnderAliasMap(comparison.Comparison.Operand, bound.Operand, nil) {
								t.Fatalf("probe consumed the additional range bound: %s", residual.Explain())
							}
						} else if disjunction, ok := residual.(*predicates.OrPredicate); !ok || len(disjunction.SubPredicates) != 2 {
							t.Fatalf("alternative ranges were not preserved as OR: %s", residual.Explain())
						}
						var comparisons []*predicates.ComparisonRange
						switch scan := filter.GetInner().(type) {
						case *plans.RecordQueryScanPlan:
							if shape == "conjunction" && access == "secondary" {
								t.Fatal("secondary shortcut did not use the index")
							}
							comparisons = scan.GetScanComparisons()
						case *plans.RecordQueryIndexPlan:
							if shape != "conjunction" || access != "secondary" {
								t.Fatal("unexpected secondary probe")
							}
							comparisons = scan.GetScanComparisons()
						default:
							t.Fatalf("unexpected filtered access plan: %T", filter.GetInner())
						}
						if shape == "conjunction" {
							if len(comparisons) != 1 || !comparisons[0].IsEquality() {
								t.Fatalf("partitioned equality did not become a point probe: %v", comparisons)
							}
						} else if len(comparisons) != 0 {
							t.Fatalf("one disjunct incorrectly narrowed the entire inner: %v", comparisons)
						}
					}
				})
			}
		}
	}
}

func TestImplementNestedLoopJoin_MaterializedPartitionedPredicates(t *testing.T) {
	t.Parallel()
	for name, kind := range map[string]expressions.JoinType{"inner": expressions.JoinInner, "full_outer": expressions.JoinFullOuter} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			leftRef, rightRef := expressions.InitialOf(nljLogicalScan("L")), expressions.InitialOf(nljLogicalScan("R"))
			leftRef.InsertFinal(nljPhysicalScan("L"))
			rightRef.InsertFinal(nljPhysicalScan("R"))
			leftQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("L"), leftRef)
			rightQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("R"), rightRef)
			leftID, rightID := nljField(leftQ, 0), nljField(rightQ, 0)
			sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(nljFlowed(leftQ),
				[]expressions.Quantifier{leftQ, rightQ}, []predicates.QueryPredicate{
					predicates.NewComparisonPredicate(leftID, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: rightID}),
					predicates.NewComparisonPredicate(leftID, predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(10))}),
				}, []string{"L", "R"}, kind))
			if len(sel.GetPredicates()) != 1 {
				t.Fatalf("fixture did not combine the same-value constraints: %v", sel.GetPredicates())
			}
			if _, ok := sel.GetPredicates()[0].(*predicates.PredicateWithValueAndRanges); !ok {
				t.Fatalf("fixture predicate = %T, want PVR", sel.GetPredicates()[0])
			}
			results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
			if len(results) == 0 {
				t.Fatal("materialized join yielded no plan")
			}
			for _, result := range results {
				assertProducerPhysicalQuantifiers(t, result)
				join, ok := result.(*plans.RecordQueryNestedLoopJoinPlan)
				if !ok {
					t.Fatalf("join implementation = %T, want materialized join", result)
				}
				for _, predicate := range join.GetPredicates() {
					if bad, structural := predicates.FindStructuralPredicate(predicate); structural {
						t.Fatalf("materialized join still carries structural %T: %s", bad, bad.Explain())
					}
				}
				for _, row := range []struct {
					left, right int64
					want        predicates.TriBool
				}{
					{11, 11, predicates.TriTrue},
					{5, 5, predicates.TriFalse},
					{11, 12, predicates.TriFalse},
				} {
					program := predicates.ReplaceValues(predicates.NewAnd(join.GetPredicates()...), func(v values.Value) values.Value {
						switch {
						case values.SemanticEqualsUnderAliasMap(v, leftID, nil):
							return values.LiteralValue(row.left)
						case values.SemanticEqualsUnderAliasMap(v, rightID, nil):
							return values.LiteralValue(row.right)
						default:
							return v
						}
					})
					if got, err := program.Eval(nil); err != nil || got != row.want {
						t.Fatalf("join predicates at (%d, %d) = (%v, %v), want %v", row.left, row.right, got, err, row.want)
					}
				}
			}
		})
	}
}

func TestExistsShortcutPreservesPriorScanBounds(t *testing.T) {
	t.Parallel()
	for _, access := range []string{"primary", "secondary"} {
		t.Run(access, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name  string
				kinds []predicates.ComparisonType
			}{
				{"unbounded", nil},
				{"equality", []predicates.ComparisonType{predicates.ComparisonEquals}},
				{"inequality", []predicates.ComparisonType{predicates.ComparisonGreaterThan}},
				{"composite", []predicates.ComparisonType{predicates.ComparisonEquals, predicates.ComparisonGreaterThan}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					o, i := values.NamedCorrelationIdentifier("O"), values.NamedCorrelationIdentifier("I")
					outerType, innerType := nljTestLayouts["OUTER"], nljTestLayouts["INNER"]
					outer := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerType, false))
					inner := mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"INNER"}, innerType, false)).WithPrimaryKey([]values.Value{
						nljBakedRef(t, "INNER", i, "ID"), nljBakedRef(t, "INNER", i, "OUTER_ID"),
					})
					var bounds []*predicates.ComparisonRange
					for n, kind := range tc.kinds {
						merged := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: kind, Operand: &values.ConstantValue{Value: int64(n + 1), Typ: values.NotNullLong}})
						if !merged.Complete() {
							t.Fatal("fixture bound did not merge")
						}
						bounds = append(bounds, merged.Range)
					}
					inner = inner.WithScanComparisons(bounds).WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullLong})
					outerRef := expressions.InitialOf(mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"OUTER"}, outerType)))
					innerRef := expressions.InitialOf(mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"INNER"}, innerType)))
					outerRef.InsertFinal(outer)
					innerRef.InsertFinal(inner)
					oq, iq := expressions.NamedForEachQuantifier(o, outerRef), expressions.NamedExistentialQuantifier(i, innerRef)
					pred := predicates.NewComparisonPredicate(nljBakedRef(t, "INNER", i, "ID"), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: nljBakedRef(t, "OUTER", o, "ID")})
					sel := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(nljFlowed(oq), []expressions.Quantifier{oq, iq}, []predicates.QueryPredicate{pred, mustExistentialAlias(t, i)}, []string{"O", "I"}))
					ctx := nljPrimaryKeyPlanContext{PlanContext: NewPlanContextFromMatchCandidates(nil), primaryKey: []string{"ID", "OUTER_ID"}}
					if access == "secondary" {
						scalar := false
						candidate := NewValueIndexScanMatchCandidateWithFunctions("INNER$ID", []string{"INNER"}, []string{"ID"}, nil, []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}, innerType, false, nil, &scalar).WithKeyComponentTypes([]values.Type{values.NotNullLong})
						ctx.PlanContext = NewPlanContextFromMatchCandidates([]MatchCandidate{candidate})
						ctx.primaryKey = nil
					}
					alternatives := mustFireExpressionRuleWithMemo(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel), ctx, nil)
					if len(alternatives) == 0 {
						t.Fatal("no implemented existential; bound preservation cannot be vacuous")
					}
					for _, alternative := range alternatives {
						plan, ok := alternative.(plans.RecordQueryPlan)
						if !ok {
							t.Fatalf("not a physical plan: %T", alternative)
						}
						scans, indexes, residuals := 0, 0, 0
						plans.Walk(plan, func(p plans.RecordQueryPlan) bool {
							if _, ok := p.(*plans.RecordQueryPredicatesFilterPlan); ok {
								residuals++
							}
							if _, ok := p.(*plans.RecordQueryIndexPlan); ok {
								indexes++
							}
							scan, ok := p.(*plans.RecordQueryScanPlan)
							if !ok || len(scan.GetRecordTypes()) != 1 || scan.GetRecordTypes()[0] != "INNER" {
								return true
							}
							scans++
							got := scan.GetScanComparisons()
							if len(tc.kinds) == 0 {
								if len(got) != 1 {
									t.Errorf("unbounded primary control did not become a correlated point probe: %v", got)
								}
								return true
							}
							if len(got) != len(tc.kinds) {
								t.Errorf("lost bound vector: got %d positions, want %d", len(got), len(tc.kinds))
								return true
							}
							for pos, r := range got {
								comparisons := r.GetComparisons()
								if len(comparisons) != 1 {
									t.Errorf("position %d comparisons=%v", pos, comparisons)
									continue
								}
								literal, isLiteral := comparisons[0].Operand.(*values.ConstantValue)
								if comparisons[0].Type != tc.kinds[pos] || !isLiteral || literal.Value != int64(pos+1) {
									t.Errorf("position %d lost prior %v %d: %#v", pos, tc.kinds[pos], pos+1, comparisons[0])
								}
							}
							return true
						})
						if len(tc.kinds) > 0 && (scans != 1 || indexes != 0 || residuals == 0) {
							t.Errorf("bounded inner must survive with residual: scans=%d indexes=%d filters=%d", scans, indexes, residuals)
						}
						if len(tc.kinds) == 0 && access == "secondary" && indexes != 1 {
							t.Errorf("unbounded secondary control did not select index: %d", indexes)
						}
						if len(tc.kinds) == 0 && access == "primary" && scans != 1 {
							t.Errorf("unbounded primary control has %d inner scans", scans)
						}
					}
				})
			}
		})
	}
}

// A join leg that FLOWS a scalar is never materialized: the materialized NLJ
// binds each leg as a record and would read the scalar as its one-slot row, so
// the rule yields only the FlatMap, which binds the quantifier to the flowed
// value itself. Each arm of the choice is driven here, over an Explode of a
// collection an enclosing row owns (a correlated array as a block's FROM
// item; an uncorrelated constant list is the IN-join rule's and declines
// earlier): the scalar leg on the right, on the left (swapped inside), under a
// filter (the flowed type, not the Explode member, is the property), and as a
// LEFT OUTER's preserved leg, which cannot move inside and stays the outer.
func TestImplementNestedLoopJoin_ScalarFlowingLegIsNeverMaterialized(t *testing.T) {
	t.Parallel()
	enclosingType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullLong)},
	}}
	enclosing := mustNLJConstruct(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTER"), enclosingType))
	collection := mustNLJConstruct(values.ResolveFieldOrdinals(enclosing, []int{1}))
	explodeRef := func(t *testing.T) *expressions.Reference {
		t.Helper()
		ref := expressions.InitialOf(mustNLJConstruct(expressions.NewExplodeExpression(collection)))
		ref.InsertFinal(mustNLJConstruct(plans.NewRecordQueryExplodePlan(collection)))
		return ref
	}
	filteredExplodeRef := func(t *testing.T) *expressions.Reference {
		t.Helper()
		inner := expressions.ForEachQuantifier(explodeRef(t))
		pred := predicates.NewComparisonPredicate(nljFlowed(inner),
			predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(1))})
		ref := expressions.InitialOf(mustNLJConstruct(expressions.NewSelectExpression(
			nljFlowed(inner), []expressions.Quantifier{inner}, []predicates.QueryPredicate{pred})))
		ref.InsertFinal(mustNLJConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(inner, []predicates.QueryPredicate{pred})))
		return ref
	}
	for _, tc := range []struct {
		name        string
		scalarFirst bool
		scalar      func(*testing.T) *expressions.Reference
		joinType    expressions.JoinType
	}{
		{"scalar right", false, explodeRef, expressions.JoinCross},
		{"scalar left swaps inside", true, explodeRef, expressions.JoinCross},
		{"filter over a scalar explode", false, filteredExplodeRef, expressions.JoinCross},
		{"left outer preserved scalar stays outer", true, explodeRef, expressions.JoinLeftOuter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scanRef := expressions.InitialOf(nljLogicalScan("A"))
			scanRef.InsertFinal(nljPhysicalScan("A"))
			scanQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), scanRef)
			scalarQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("V"), tc.scalar(t))
			quants, aliases := []expressions.Quantifier{scanQ, scalarQ}, []string{"A", "V"}
			if tc.scalarFirst {
				quants, aliases = []expressions.Quantifier{scalarQ, scanQ}, []string{"V", "A"}
			}
			// A LEFT OUTER's null-supplied leg flows a null-extended row, so
			// the select projects its preserved leg.
			result := nljFlowed(scanQ)
			if tc.joinType == expressions.JoinLeftOuter {
				result = nljFlowed(scalarQ)
			}
			sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
				result, quants, nil, aliases, tc.joinType))
			results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
			flatMaps := 0
			for _, result := range results {
				switch plan := result.(type) {
				case *plans.RecordQueryNestedLoopJoinPlan:
					t.Fatalf("a scalar-flowing leg was materialized: %T", plan)
				case *plans.RecordQueryFlatMapPlan:
					flatMaps++
				}
			}
			if flatMaps == 0 {
				t.Fatalf("no FlatMap implements the join; results=%d", len(results))
			}
		})
	}
}

// TestImplementNestedLoopJoin_LeftOuterInputFiltersOnConjunctsBelowTheWrap
// pins the lowering of a LEFT-OUTER-typed select — RewriteOuterJoinRule's
// input, whose predicates are all ON conjuncts. A scalar preserved leg forces
// the correlated FlatMap orientation (the preserved leg stays the outer), and
// the lowering filtered an ON conjunct reading the inner ABOVE the
// DefaultOnEmpty (a preserved row whose inner is non-empty but matches nothing
// dropped) and one reading only the preserved leg on the preserved rows. Every
// ON conjunct must filter the inner below the wrap, which then null-extends a
// preserved row whose filtered inner is empty.
func TestImplementNestedLoopJoin_LeftOuterInputFiltersOnConjunctsBelowTheWrap(t *testing.T) {
	t.Parallel()
	enclosingType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullLong)},
	}}
	enclosing := mustNLJConstruct(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTER"), enclosingType))
	collection := mustNLJConstruct(values.ResolveFieldOrdinals(enclosing, []int{1}))
	explodeRef := expressions.InitialOf(mustNLJConstruct(expressions.NewExplodeExpression(collection)))
	explodeRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryExplodePlan(collection)))
	scalarQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("V"), explodeRef)
	scanRef := expressions.InitialOf(nljLogicalScan("A"))
	scanRef.InsertFinal(nljPhysicalScan("A"))
	scanQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), scanRef)
	readsInner := predicates.NewComparisonPredicate(nljField(scanQ, 0),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: nljFlowed(scalarQ)})
	readsPreserved := predicates.NewComparisonPredicate(nljFlowed(scalarQ),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(1))})
	for _, tc := range []struct {
		name  string
		preds []predicates.QueryPredicate
	}{
		{"an ON conjunct reading the inner", []predicates.QueryPredicate{readsInner}},
		{"an ON conjunct reading only the preserved leg", []predicates.QueryPredicate{readsPreserved}},
		{"both", []predicates.QueryPredicate{readsInner, readsPreserved}},
		{"no ON conjunct", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
				nljFlowed(scalarQ), []expressions.Quantifier{scalarQ, scanQ}, tc.preds,
				[]string{"V", "A"}, expressions.JoinLeftOuter))
			flatMaps := 0
			for _, result := range mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel)) {
				assertProducerPhysicalQuantifiers(t, result)
				flatMap, ok := result.(*plans.RecordQueryFlatMapPlan)
				if !ok {
					continue
				}
				flatMaps++
				if _, filteredOuter := flatMap.GetOuter().(*plans.RecordQueryPredicatesFilterPlan); filteredOuter {
					t.Fatalf("an ON conjunct filtered the preserved rows: %s", flatMap.Explain())
				}
				doe, wrapped := flatMap.GetInner().(*plans.RecordQueryDefaultOnEmptyPlan)
				if !wrapped {
					t.Fatalf("the inner is not null-extended (an ON conjunct above the wrap?): %s", flatMap.Explain())
				}
				filter, filtered := doe.GetInner().(*plans.RecordQueryPredicatesFilterPlan)
				if filtered != (len(tc.preds) > 0) {
					t.Fatalf("under the wrap filtered=%v, want %v: %s", filtered, len(tc.preds) > 0, flatMap.Explain())
				}
				if filtered && len(filter.GetPredicates()) != len(tc.preds) {
					t.Fatalf("under the wrap %d predicate(s), want every ON conjunct (%d): %s",
						len(filter.GetPredicates()), len(tc.preds), flatMap.Explain())
				}
			}
			if flatMaps == 0 {
				t.Fatal("the LEFT OUTER input yielded no FlatMap")
			}
		})
	}
}

// TestImplementNestedLoopJoin_UncorrelatedNullOnEmptyLegIsExtended pins
// RewriteOuterJoinRule's output for a leg correlated only to an enclosing
// query: no leg reads the other, yet the null-on-empty leg still needs its
// DefaultOnEmpty, which only the FlatMap lowering supplies (Java's single
// implementation of a binary select).
func TestImplementNestedLoopJoin_UncorrelatedNullOnEmptyLegIsExtended(t *testing.T) {
	t.Parallel()
	newScanRef := func(name string) *expressions.Reference {
		ref := expressions.InitialOf(nljLogicalScan(name))
		ref.InsertFinal(nljPhysicalScan(name))
		return ref
	}
	leftQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("L"), newScanRef("LEFT"))
	rightQ := expressions.NamedForEachNullOnEmptyQuantifier(values.NamedCorrelationIdentifier("R"), newScanRef("RIGHT"))
	sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "L_ID", Value: nljField(leftQ, 0)},
			values.RecordConstructorField{Name: "R_ID", Value: nljField(rightQ, 0)},
		),
		[]expressions.Quantifier{leftQ, rightQ}, nil, []string{"L", "R"}, expressions.JoinInner,
	))
	results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
	if len(results) == 0 {
		t.Fatal("an uncorrelated null-on-empty leg yielded no implementation")
	}
	for _, result := range results {
		flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
		if !isFlatMap {
			t.Fatalf("yielded %T; a null-on-empty leg needs the FlatMap lowering", result)
		}
		extended := false
		for _, leg := range []plans.RecordQueryPlan{flatMap.GetOuter(), flatMap.GetInner()} {
			if _, ok := leg.(*plans.RecordQueryDefaultOnEmptyPlan); ok {
				extended = true
			}
		}
		if !extended {
			t.Fatalf("the null-on-empty leg is not extended: %s", flatMap.Explain())
		}
	}
}

// TestImplementNestedLoopJoin_NullOnEmptyOuterIsExtended pins Java's
// planPartitionToPhysical on the OUTER leg: a null-on-empty quantifier that
// partitioning made the outer of a correlated pair (a LEFT OUTER's
// null-supplying leg peeled into a lower with the leg that reads it) is wrapped
// in DefaultOnEmpty, and the FlatMap states the outer's presence. Ignoring the
// flag there dropped every null-extended row.
func TestImplementNestedLoopJoin_NullOnEmptyOuterIsExtended(t *testing.T) {
	t.Parallel()

	newScanRef := func(name string) *expressions.Reference {
		ref := expressions.InitialOf(nljLogicalScan(name))
		ref.InsertFinal(nljPhysicalScan(name))
		return ref
	}
	outerAlias := values.NamedCorrelationIdentifier("E")
	outerQ := expressions.NamedForEachNullOnEmptyQuantifier(outerAlias, newScanRef("EMP"))
	outerRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(outerAlias, nljSimpleRowType("EMP")))
	outerID := mustNLJConstruct(values.ResolveFieldOrdinals(outerRoot, []int{0}))

	t.Run("correlated ForEach inner", func(t *testing.T) {
		scanQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("BADGE"), expressions.InitialOf(nljLogicalScan("BADGE")))
		legRef := expressions.InitialOf(mustNLJConstruct(newBlockSelectForTest(
			[]values.Value{outerID}, scanQ)))
		if !legRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{outerID}, nljPhysicalScan("BADGE")))) {
			t.Fatal("insert the correlated leg's physical projection")
		}
		innerQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("F"), legRef)
		sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "E_ID", Value: nljField(outerQ, 0)},
				values.RecordConstructorField{Name: "F_ID", Value: nljField(innerQ, 0)},
			),
			[]expressions.Quantifier{outerQ, innerQ}, nil, []string{"E", "F"}, expressions.JoinInner,
		))
		results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
		if len(results) == 0 {
			t.Fatal("a null-on-empty outer with a leg correlated to it yielded no implementation")
		}
		for _, result := range results {
			assertProducerPhysicalQuantifiers(t, result)
			flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
			if !isFlatMap {
				t.Fatalf("yielded %T; a correlated pair is a FlatMap", result)
			}
			if flatMap.GetOuterAlias() != outerAlias {
				t.Fatalf("FlatMap outer = %v, want the null-on-empty E (the leg reading it is the inner)",
					flatMap.GetOuterAlias())
			}
			if _, extended := flatMap.GetOuter().(*plans.RecordQueryDefaultOnEmptyPlan); !extended ||
				!flatMap.NullSupplyingOuter() {
				t.Fatalf("null-on-empty outer not extended: outer %T, NullSupplyingOuter=%t\n%s",
					flatMap.GetOuter(), flatMap.NullSupplyingOuter(), flatMap.Explain())
			}
		}
	})

	// The wrap comes BEFORE the inner is normalized against the outer's
	// physical row: the inner's correlated program must read the row the
	// FlatMap binds, which is the DefaultOnEmpty's null-extended carrier, not
	// the bare scan's. A correlated Explode's collection is one such program.
	t.Run("correlated Explode inner reads the carrier", func(t *testing.T) {
		arrRow := values.NewRecordType("EMPA", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullLong)},
		})
		arrAlias := values.NamedCorrelationIdentifier("EA")
		scanRef := expressions.InitialOf(mustNLJConstruct(expressions.NewFullUnorderedScanExpression([]string{"EMPA"}, arrRow)))
		scanRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryScanPlan([]string{"EMPA"}, arrRow, false)))
		arrOuterQ := expressions.NamedForEachNullOnEmptyQuantifier(arrAlias, scanRef)
		// The read is typed as translation types it: through the quantifier's
		// flowed row, which a null-on-empty edge widens to nullable.
		logicalRead := nljFlowed(arrOuterQ)
		collection := mustNLJConstruct(values.ResolveFieldOrdinals(logicalRead, []int{1}))
		explodeRef := expressions.InitialOf(mustNLJConstruct(expressions.NewExplodeExpression(collection)))
		explodeRef.InsertFinal(mustNLJConstruct(plans.NewRecordQueryExplodePlan(collection)))
		innerQ := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("V"), explodeRef)
		sel := mustNLJConstruct(expressions.NewSelectExpressionWithJoinType(
			values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "E_ID", Value: nljField(arrOuterQ, 0)},
				values.RecordConstructorField{Name: "V", Value: nljFlowed(innerQ)},
			),
			[]expressions.Quantifier{arrOuterQ, innerQ}, nil, []string{"EA", "V"}, expressions.JoinInner,
		))
		results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
		if len(results) == 0 {
			t.Fatal("a null-on-empty outer with an Explode of its array yielded no implementation")
		}
		for _, result := range results {
			assertProducerPhysicalQuantifiers(t, result)
			flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
			if !isFlatMap {
				t.Fatalf("yielded %T; a correlated pair is a FlatMap", result)
			}
			doe, extended := flatMap.GetOuter().(*plans.RecordQueryDefaultOnEmptyPlan)
			if !extended {
				t.Fatalf("null-on-empty outer not extended: %s", flatMap.Explain())
			}
			carrier, err := values.ExactRelationOf(doe.GetResultType())
			if err != nil {
				t.Fatal(err)
			}
			carrierRow, _ := carrier.RelationInner()
			explode, isExplode := flatMap.GetInner().(*plans.RecordQueryExplodePlan)
			if !isExplode {
				t.Fatalf("inner %T, want the Explode: %s", flatMap.GetInner(), flatMap.Explain())
			}
			// The nullable read matches the null-extended carrier exactly, so
			// the normalization re-roots it onto the row the FlatMap binds. Had
			// the wrap come after, the carrier would be the bare scan's NOT
			// NULL row, the read would not match it, and the inner would keep
			// the logical read.
			var reads []values.QuantifiedObjectValue
			values.WalkValue(explode.GetCollectionValue(), func(n values.Value) bool {
				if qov, ok := values.AsQuantifiedObjectValue(n); ok && qov.Correlation() == arrAlias {
					reads = append(reads, qov)
				}
				return true
			})
			if len(reads) != 1 {
				t.Fatalf("the Explode reads EA %d time(s), want 1: %s", len(reads), flatMap.Explain())
			}
			if !reads[0].FlowedType().Equals(carrierRow.Type()) {
				t.Fatalf("the inner reads EA as %v, want the null-extended carrier %v", reads[0].FlowedType(), carrierRow.Type())
			}
			if values.Value(reads[0]) == logicalRead {
				t.Fatalf("the inner still reads EA through the logical read, not the carrier the FlatMap binds: %s", flatMap.Explain())
			}
		}
	})

	// PartitionSelect leaves a LEFT JOIN's null-supplied leg and an EXISTS read
	// through it as one binary select; without the wrap it has no plan at all.
	for _, tc := range []struct {
		name   string
		result func(existQ expressions.Quantifier) values.Value
		preds  func(existAlias values.CorrelationIdentifier) []predicates.QueryPredicate
	}{
		{
			"existential inner", func(expressions.Quantifier) values.Value { return nljFlowed(outerQ) },
			func(a values.CorrelationIdentifier) []predicates.QueryPredicate {
				return []predicates.QueryPredicate{mustExistentialAlias(t, a)}
			},
		},
		{
			"existential inner read by the result", func(existQ expressions.Quantifier) values.Value { return nljFlowed(existQ) },
			func(values.CorrelationIdentifier) []predicates.QueryPredicate { return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existAlias := values.NamedCorrelationIdentifier("X")
			existQ := expressions.NamedExistentialQuantifier(existAlias, newScanRef("BADGE"))
			sel := mustNLJConstruct(expressions.NewSelectExpressionWithAliases(
				tc.result(existQ),
				[]expressions.Quantifier{outerQ, existQ},
				tc.preds(existAlias),
				[]string{"E", "X"},
			))
			results := mustFireExpressionRule(t, NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
			if len(results) == 0 {
				t.Fatal("a null-on-empty outer beside an existential yielded no implementation")
			}
			for _, result := range results {
				flatMap, isFlatMap := result.(*plans.RecordQueryFlatMapPlan)
				if !isFlatMap {
					t.Fatalf("yielded %T, want a FlatMap", result)
				}
				if _, extended := flatMap.GetOuter().(*plans.RecordQueryDefaultOnEmptyPlan); !extended ||
					!flatMap.NullSupplyingOuter() {
					t.Fatalf("null-on-empty outer not extended: outer %T, NullSupplyingOuter=%t\n%s",
						flatMap.GetOuter(), flatMap.NullSupplyingOuter(), flatMap.Explain())
				}
				if !strings.Contains(flatMap.GetInner().Explain(), "FirstOrDefault(") {
					t.Fatalf("inner is %s, want the existential under a FirstOrDefault", flatMap.GetInner().Explain())
				}
			}
		})
	}
}

// The join's memo edges must declare its runtime bindings, so a dissolved
// LEFT box provides D/E without leaking discarded edge aliases.
func TestPhysicalProvidedAliasesIncludeAJoinsExecutableAliases(t *testing.T) {
	t.Parallel()
	d, e := values.NamedCorrelationIdentifier("D"), values.NamedCorrelationIdentifier("E")
	leftQ := expressions.ForEachQuantifier(expressions.FinalOf(nljPhysicalScan("DEPT")))
	rightQ := expressions.ForEachQuantifier(expressions.FinalOf(nljPhysicalScan("EMP")))
	if leftQ.GetAlias() == d || rightQ.GetAlias() == e {
		t.Fatal("setup: the memo quantifiers must not already be named D/E")
	}
	dRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(d, nljSimpleRowType("DEPT")))
	eRoot := mustNLJConstruct(values.NewQuantifiedObjectValue(e, nljSimpleRowType("EMP")))
	join := mustNLJConstruct(plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
		leftQ, rightQ, nil, plans.JoinLeftOuter, d, e,
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "D_ID", Value: mustNLJConstruct(values.ResolveFieldOrdinals(dRoot, []int{0}))},
			values.RecordConstructorField{Name: "E_ID", Value: mustNLJConstruct(values.ResolveFieldOrdinals(eRoot, []int{0}))},
		)))
	box := values.NamedCorrelationIdentifier("E$BOX")
	provided := physicalProvidedAliases(join, box)
	if qs := join.GetQuantifiers(); qs[0].GetAlias() != d || qs[1].GetAlias() != e {
		t.Fatal("join edges disagree with the runtime bindings")
	}
	for _, stale := range []values.CorrelationIdentifier{leftQ.GetAlias(), rightQ.GetAlias()} {
		if _, ok := provided[stale]; ok {
			t.Errorf("physicalProvidedAliases advertises discarded edge %v", stale)
		}
	}
	for _, want := range []values.CorrelationIdentifier{box, d, e} {
		if _, ok := provided[want]; !ok {
			t.Errorf("physicalProvidedAliases lacks %v: %v", want, provided)
		}
	}
}
