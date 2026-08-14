package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPropertiesConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct plan-properties fixture: " + err.Error())
	}
	return value
}

func planPropertiesRowType() values.Type {
	return values.NewRecordType("PropertiesRow", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "name", FieldType: values.NotNullString, Ordinal: 1},
		{Name: "dept", FieldType: values.NotNullLong, Ordinal: 2},
	})
}

func planPropertiesScan(recordType string) *plans.RecordQueryScanPlan {
	return mustPropertiesConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, planPropertiesRowType(), false))
}

func planPropertiesField(name string, ordinal int) values.Value {
	root := mustPropertiesConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("properties_row"), planPropertiesRowType()))
	field := mustPropertiesConstruct(values.ResolveOrdinalSeedField(root, ordinal))
	view, ok := values.AsFieldValue(field)
	if !ok || view.DisplayName() != name {
		panic("construct plan-properties fixture: resolved field name mismatch")
	}
	return field
}

func planPropertiesAggregation(recordType string) *plans.RecordQueryStreamingAggregationPlan {
	return mustPropertiesConstruct(plans.NewRecordQueryStreamingAggregationPlan(
		planPropertiesScan(recordType),
		[]values.Value{planPropertiesField("dept", 2)},
		[]expressions.AggregateSpec{{
			Function: expressions.AggSum,
			Operand:  planPropertiesField("id", 0),
		}},
	))
}

func planPropertiesValues() *plans.RecordQueryValuesPlan {
	return mustPropertiesConstruct(plans.NewRecordQueryValuesPlan(nil))
}

// ---------------------------------------------------------------------------
// computeDistinctRecords
// ---------------------------------------------------------------------------

func TestComputeDistinctRecords_ScanIsTrue(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	wrapper := scan
	got := computeDistinctRecords(wrapper, scan)
	if !got {
		t.Fatal("scan should produce distinct records")
	}
}

// TestComputeDistinctRecords_IndexFanOutSignal pins RFC-188 finding 10 M4:
// DistinctRecords for an index scan follows Java DistinctRecordsProperty.visitIndexPlan
// — !matchCandidate.createsDuplicates(), INDEPENDENT of UNIQUE. A non-unique
// SCALAR index does not create duplicates and so IS distinct; only a fan-out
// index is not. Until the candidate signal is stamped (WithDistinctRecordsSignal)
// the property returns false — Java's empty-candidate default. The old code
// returned IsUnique(), missing DISTINCT elision over non-unique scalar indexes.
func TestComputeDistinctRecords_IndexFanOutSignal(t *testing.T) {
	t.Parallel()
	mk := func() *plans.RecordQueryIndexPlan {
		return mustPropertiesConstruct(plans.NewRecordQueryIndexPlan(
			"idx1", nil, []string{"T"}, planPropertiesRowType(), false))
	}

	// No candidate signal → false (Java empty-candidate default).
	unstamped := mk().WithIndexMetadata(nil, nil, false)
	if computeDistinctRecords(unstamped, unstamped) {
		t.Fatal("index without a stamped candidate signal must NOT be distinct (Java empty-candidate default)")
	}

	// Non-unique SCALAR index (does not create duplicates) → distinct=TRUE.
	// The M4 fix: the old IsUnique()-based code returned false here.
	scalarNonUnique := mk().WithIndexMetadata(nil, nil, false).WithDistinctRecordsSignal(false)
	if !computeDistinctRecords(scalarNonUnique, scalarNonUnique) {
		t.Fatal("non-unique SCALAR index does not create duplicates → should be distinct (M4)")
	}

	// Unique index (also does not create duplicates) → distinct=TRUE.
	unique := mk().WithIndexMetadata(nil, nil, true).WithDistinctRecordsSignal(false)
	if !computeDistinctRecords(unique, unique) {
		t.Fatal("unique index does not create duplicates → should be distinct")
	}

	// Fan-out index (creates duplicates) → distinct=FALSE.
	fanOut := mk().WithIndexMetadata(nil, nil, false).WithDistinctRecordsSignal(true)
	if computeDistinctRecords(fanOut, fanOut) {
		t.Fatal("fan-out index creates duplicates → must NOT be distinct")
	}
}

func TestComputeDistinctRecords_FilterInheritsFromChild(t *testing.T) {
	t.Parallel()
	// Build: predicates-filter over a bare scan expression (distinct=true).
	scan := planPropertiesScan("T")

	// Put the scan in a Reference and compute its properties.
	innerRef := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	innerRef.SetPlanProperties(pm)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterPlan := mustPropertiesConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		scan, []predicates.QueryPredicate{pred}))
	innerQ := expressions.ForEachQuantifier(innerRef)
	filterWrapper := mustWithQuantifiers(t, filterPlan, []expressions.Quantifier{innerQ}).(*plans.RecordQueryPredicatesFilterPlan)

	got := computeDistinctRecords(filterWrapper, filterPlan)
	if !got {
		t.Fatal("filter over distinct scan should inherit distinct=true")
	}
}

func TestComputeDistinctRecords_StreamingAggIsFalse(t *testing.T) {
	t.Parallel()
	aggPlan := planPropertiesAggregation("T")
	// Since RFC-184 W2 the memo holds the bare plan (no physicalStreamingAggWrapper).
	got := computeDistinctRecords(aggPlan, aggPlan)
	if got {
		t.Fatal("streaming aggregation should NOT produce distinct records")
	}
}

func TestComputeDistinctRecords_DistinctPlanIsTrue(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	dp := mustPropertiesConstruct(plans.NewRecordQueryDistinctPlan(scan))
	scanW := scan
	innerRef := expressions.InitialOf(scanW)
	innerQ := expressions.ForEachQuantifier(innerRef)
	dw := mustWithQuantifiers(t, dp, []expressions.Quantifier{innerQ}).(*plans.RecordQueryDistinctPlan)
	got := computeDistinctRecords(dw, dp)
	if !got {
		t.Fatal("distinct plan should produce distinct records")
	}
}

func TestComputeDistinctRecords_UnionPlanIsFalse(t *testing.T) {
	t.Parallel()
	// RecordQueryUnionPlan is Go's NO-DEDUP UNION ALL variant — it must report
	// non-distinct records, else an enclosing SELECT DISTINCT is wrongly elided
	// and duplicates leak through.
	scan := planPropertiesScan("T")
	up := mustPropertiesConstruct(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scan}))
	scanW := scan
	innerRef := expressions.InitialOf(scanW)
	qs := []expressions.Quantifier{expressions.ForEachQuantifier(innerRef)}
	uw := mustPropertiesConstruct(plans.NewRecordQueryUnionPlanFromQuantifiers(qs))
	got := computeDistinctRecords(uw, up)
	if got {
		t.Fatal("no-dedup UNION ALL plan must NOT report distinct records")
	}
}

// ---------------------------------------------------------------------------
// computeStoredRecord
// ---------------------------------------------------------------------------

func TestComputeStoredRecord_ScanIsTrue(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	if !computeStoredRecord(scan) {
		t.Fatal("scan should produce stored records")
	}
}

func TestComputeStoredRecord_IndexIsTrue(t *testing.T) {
	t.Parallel()
	idx := mustPropertiesConstruct(plans.NewRecordQueryIndexPlan(
		"idx1", nil, []string{"T"}, planPropertiesRowType(), false))
	if !computeStoredRecord(idx) {
		t.Fatal("index scan should produce stored records")
	}
}

func TestComputeStoredRecord_DistinctIsTrue(t *testing.T) {
	t.Parallel()
	dp := mustPropertiesConstruct(plans.NewRecordQueryDistinctPlan(planPropertiesScan("T")))
	if !computeStoredRecord(dp) {
		t.Fatal("distinct plan should produce stored records")
	}
}

func TestComputeStoredRecord_FilterInheritsFromScan(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	fp := mustPropertiesConstruct(plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{pred}, scan))
	if !computeStoredRecord(fp) {
		t.Fatal("filter over scan should inherit storedRecord=true")
	}
}

func TestComputeStoredRecord_StreamingAggIsFalse(t *testing.T) {
	t.Parallel()
	aggPlan := planPropertiesAggregation("T")
	if computeStoredRecord(aggPlan) {
		t.Fatal("streaming aggregation should NOT produce stored records")
	}
}

func TestComputeStoredRecord_UnorderedUnionOfScansIsTrue(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	uup := mustPropertiesConstruct(plans.NewRecordQueryUnorderedUnionPlan(
		[]plans.RecordQueryPlan{scan}))
	if !computeStoredRecord(uup) {
		t.Fatal("unordered union of scans should produce stored records (allChildren)")
	}
}

func TestComputeStoredRecord_UnionAllChildrenStored(t *testing.T) {
	t.Parallel()
	scanA := planPropertiesScan("A")
	scanB := planPropertiesScan("B")
	up := mustPropertiesConstruct(plans.NewRecordQueryUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}))
	if !computeStoredRecord(up) {
		t.Fatal("union of scans should produce stored records")
	}
}

// ---------------------------------------------------------------------------
// PlanPropertiesMap
// ---------------------------------------------------------------------------

func TestPlanPropertiesMap_AddAndRetrieve(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	wrapper := scan

	pm := NewPlanPropertiesMap()
	pm.Add(wrapper)

	props := pm.GetProperties(wrapper)
	if props == nil {
		t.Fatal("GetProperties returned nil for added wrapper")
	}
	// Scan should be distinct and stored.
	if !props.GetBool(properties.PropDistinctRecords) {
		t.Fatal("scan should have distinctRecords=true")
	}
	if !props.GetBool(properties.PropStoredRecord) {
		t.Fatal("scan should have storedRecord=true")
	}
}

func TestPlanPropertiesMap_Expressions(t *testing.T) {
	t.Parallel()
	scanA := planPropertiesScan("A")
	scanB := planPropertiesScan("B")
	wA := scanA
	wB := scanB

	pm := NewPlanPropertiesMap()
	pm.Add(wA)
	pm.Add(wB)

	exprs := pm.Expressions()
	if len(exprs) != 2 {
		t.Fatalf("Expressions() length = %d, want 2", len(exprs))
	}
}

func TestPlanPropertiesMap_GetProperties_Missing(t *testing.T) {
	t.Parallel()
	pm := NewPlanPropertiesMap()
	scan := planPropertiesScan("T")
	wrapper := scan
	props := pm.GetProperties(wrapper)
	if props != nil {
		t.Fatalf("GetProperties for non-added wrapper = %v, want nil", props)
	}
}

// ---------------------------------------------------------------------------
// computeRefPlanProperties
// ---------------------------------------------------------------------------

func TestComputeRefPlanProperties_StoresMapOnReference(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	wrapper := scan
	ref := expressions.InitialOf(wrapper)

	computeRefPlanProperties(ref)

	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		t.Fatal("GetRefPlanPropertiesMap returned nil after computeRefPlanProperties")
	}
	props := pm.GetProperties(wrapper)
	if props == nil {
		t.Fatal("properties not stored for wrapper")
	}
	if !props.GetBool(properties.PropDistinctRecords) {
		t.Fatal("scan should be distinct")
	}
}

func TestGetRefPlanPropertiesMap_NilRef(t *testing.T) {
	t.Parallel()
	if pm := GetRefPlanPropertiesMap(nil); pm != nil {
		t.Fatalf("GetRefPlanPropertiesMap(nil) = %v, want nil", pm)
	}
}

func TestGetRefPlanPropertiesMap_NoProperties(t *testing.T) {
	t.Parallel()
	logicalExpr := mustPropertiesConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, planPropertiesRowType()))
	ref := expressions.InitialOf(logicalExpr)
	if pm := GetRefPlanPropertiesMap(ref); pm != nil {
		t.Fatalf("GetRefPlanPropertiesMap on ref with no plan properties = %v, want nil", pm)
	}
}

func TestComputeRefPlanProperties_SkipsLogicalExpressions(t *testing.T) {
	t.Parallel()
	logicalExpr := mustPropertiesConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, planPropertiesRowType()))
	ref := expressions.InitialOf(logicalExpr)

	computeRefPlanProperties(ref)

	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		t.Fatal("plan properties map should still be stored")
	}
	if len(pm.All()) != 0 {
		t.Fatalf("expected empty map for logical-only ref, got %d entries", len(pm.All()))
	}
}

// ---------------------------------------------------------------------------
// New plan type property tests
// ---------------------------------------------------------------------------

func TestComputeDistinctRecords_MergeSortUnionIsTrue(t *testing.T) {
	t.Parallel()
	scanA := planPropertiesScan("A")
	scanB := planPropertiesScan("B")
	msu := mustPropertiesConstruct(plans.NewRecordQueryMergeSortUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}, nil, false, true))
	if !computeDistinctRecords(msu, msu) {
		t.Fatal("MergeSortUnion should be distinct")
	}
}

// TestComputeDistinctRecords_MergeSortUnionRemoveDuplicatesFalseIsFalse pins
// the ordered UNION ALL mode (removeDuplicates=false): mergeSortCursor
// (executor_new_plans.go) lets tied rows from non-leading children re-emit
// when the flag is false, so the property must report non-distinct. Before
// this fix computeDistinctRecords claimed every RecordQueryMergeSortUnionPlan
// was distinct regardless of the flag, which would have let
// ImplementDistinctFinalRule elide a needed dedup wrapper.
func TestComputeDistinctRecords_MergeSortUnionRemoveDuplicatesFalseIsFalse(t *testing.T) {
	t.Parallel()
	scanA := planPropertiesScan("A")
	scanB := planPropertiesScan("B")
	msu := mustPropertiesConstruct(plans.NewRecordQueryMergeSortUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}, nil, false, false))
	if computeDistinctRecords(msu, msu) {
		t.Fatal("MergeSortUnion with removeDuplicates=false should not claim distinct records")
	}
}

func TestComputeDistinctRecords_InUnionIsTrue(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	// The InUnion is its own physical expression now (RFC-184 W2) — it IS the
	// physicalPlanExpression computeDistinctRecords inspects.
	iup := mustPropertiesConstruct(plans.NewRecordQueryInUnionPlan(
		scan, []string{"b"}, nil, false))
	if !computeDistinctRecords(iup, iup) {
		t.Fatal("InUnion should be distinct")
	}
}

func TestComputeCardinalities_InUnionLiteralFanout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings []string
		sources  [][]any
		want     properties.Cardinalities
	}{
		{
			name:     "Cartesian product",
			bindings: []string{"a", "b"},
			sources:  [][]any{{1, 2}, {"x", "y", "z"}},
			want: properties.Cardinalities{
				Min: properties.OfCardinality(6),
				Max: properties.OfCardinality(6),
			},
		},
		{
			name:     "known empty",
			bindings: []string{"a"},
			sources:  [][]any{{}},
			want: properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(0),
			},
		},
		{
			name:     "unknown then known empty",
			bindings: []string{"a", "b"},
			sources:  [][]any{nil, {}},
			want: properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(0),
			},
		},
		{
			name:     "known empty then unknown",
			bindings: []string{"a", "b"},
			sources:  [][]any{{}, nil},
			want: properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(0),
			},
		},
		{
			name:     "unknown runtime source",
			bindings: []string{"a"},
			sources:  [][]any{nil},
			want:     properties.UnknownMaxCardinality(),
		},
		{
			name:     "single combination bypass",
			bindings: []string{"a"},
			sources:  [][]any{{1}},
			want:     properties.ExactlyOne(),
		},
		{
			name:     "absent outer sources with binding bypass",
			bindings: []string{"a"},
			sources:  nil,
			want:     properties.ExactlyOne(),
		},
		{
			name:     "empty outer sources with binding bypass",
			bindings: []string{"a"},
			sources:  [][]any{},
			want:     properties.ExactlyOne(),
		},
		{
			name: "zero dimensions bypass",
			want: properties.ExactlyOne(),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inUnion := mustPropertiesConstruct(plans.NewRecordQueryInUnionPlan(
				planPropertiesValues(),
				test.bindings,
				nil,
				false,
			))
			inUnion.SetInSources(test.sources)
			got := computeCardinalities(inUnion, inUnion)
			if !got.Equal(test.want) {
				t.Fatalf("computeCardinalities() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestComputeCardinalities_InUnionMultipliesChildAndDegradesOverflow(t *testing.T) {
	t.Parallel()

	child := planPropertiesValues()
	newInUnion := func(bindings []string, sources [][]any, childCardinality int64) *plans.RecordQueryInUnionPlan {
		inUnion := mustPropertiesConstruct(plans.NewRecordQueryInUnionPlan(
			child, bindings, nil, false))
		inUnion.SetInSources(sources)
		childRef := inUnion.GetInnerQuantifier().GetRangesOver()
		pm := NewPlanPropertiesMap()
		pm.props[child] = properties.PropertyMap{
			properties.PropCardinalities: properties.Cardinalities{
				Min: properties.OfCardinality(childCardinality),
				Max: properties.OfCardinality(childCardinality),
			},
		}
		childRef.SetPlanProperties(pm)
		return inUnion
	}

	t.Run("child times fanout", func(t *testing.T) {
		t.Parallel()
		inUnion := newInUnion(
			[]string{"a"},
			[][]any{{1, 2}},
			3,
		)
		want := properties.Cardinalities{
			Min: properties.OfCardinality(6),
			Max: properties.OfCardinality(6),
		}
		if got := computeCardinalities(inUnion, inUnion); !got.Equal(want) {
			t.Fatalf("computeCardinalities() = %+v, want child×fanout %+v", got, want)
		}
	})

	t.Run("child multiplication overflow", func(t *testing.T) {
		t.Parallel()
		const dimensions = 18 // 10^18 fits in int64; 10^18 * child(10) does not.
		bindings := make([]string, dimensions)
		sources := make([][]any, dimensions)
		for i := range bindings {
			bindings[i] = "binding"
			sources[i] = []any{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
		}
		inUnion := newInUnion(bindings, sources, 10)
		got := computeCardinalities(inUnion, inUnion)
		if !got.GetMinCardinality().IsUnknown() || !got.GetMaxCardinality().IsUnknown() {
			t.Fatalf("overflow cardinalities = %+v, want unknown bounds", got)
		}
	})
}

func TestComputeDistinctRecords_FirstOrDefaultIsTrue(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	fod := mustPropertiesConstruct(plans.NewRecordQueryFirstOrDefaultPlan(scan, nil))
	w := scan
	if !computeDistinctRecords(w, fod) {
		t.Fatal("FirstOrDefault should be distinct")
	}
}

func TestComputeStoredRecord_FirstOrDefaultIsFalse(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	fod := mustPropertiesConstruct(plans.NewRecordQueryFirstOrDefaultPlan(scan, nil))
	if computeStoredRecord(fod) {
		t.Fatal("FirstOrDefault should NOT produce stored records")
	}
}

func TestComputeStoredRecord_DefaultOnEmptyIsFalse(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	doe := mustPropertiesConstruct(plans.NewRecordQueryDefaultOnEmptyPlan(
		scan, values.NewNullValue(values.WithNullability(planPropertiesRowType(), true))))
	if computeStoredRecord(doe) {
		t.Fatal("DefaultOnEmpty should NOT produce stored records")
	}
}

func TestComputeProperties_InMemorySortIsTransparent(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	sorted := mustPropertiesConstruct(plans.NewRecordQueryInMemorySortPlan(scan, nil))
	if !computeStoredRecord(sorted) {
		t.Fatal("InMemorySort must preserve its child's stored-record property")
	}
	if !computeDistinctRecords(sorted, sorted) {
		t.Fatal("InMemorySort must preserve its child's distinct-record property")
	}
}

func TestComputeStoredRecord_InJoinInheritsFromScan(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	ijp := mustPropertiesConstruct(plans.NewRecordQueryInJoinPlan(
		scan, "b", false, false))
	if !computeStoredRecord(ijp) {
		t.Fatal("InJoin over scan should produce stored records")
	}
}

func TestComputeStoredRecord_MergeSortUnionAllStored(t *testing.T) {
	t.Parallel()
	scanA := planPropertiesScan("A")
	scanB := planPropertiesScan("B")
	msu := mustPropertiesConstruct(plans.NewRecordQueryMergeSortUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}, nil, false, true))
	if !computeStoredRecord(msu) {
		t.Fatal("MergeSortUnion of scans should produce stored records")
	}
}

func TestComputePrimaryKey_ScanWithPK(t *testing.T) {
	t.Parallel()
	pk := []values.Value{planPropertiesField("id", 0)}
	scan := planPropertiesScan("T").WithPrimaryKey(pk)
	result := computePrimaryKey(scan)
	if result == nil {
		t.Fatal("scan with PK should return non-nil PK")
	}
	pkResult := result.([]values.Value)
	if len(pkResult) != 1 {
		t.Fatalf("expected 1 PK value, got %d", len(pkResult))
	}
}

func TestComputePrimaryKey_ScanWithoutPK(t *testing.T) {
	t.Parallel()
	scan := planPropertiesScan("T")
	result := computePrimaryKey(scan)
	if result != nil {
		t.Fatal("scan without PK should return nil")
	}
}

func TestComputePrimaryKey_FilterInheritsFromScan(t *testing.T) {
	t.Parallel()
	pk := []values.Value{planPropertiesField("id", 0)}
	scan := planPropertiesScan("T").WithPrimaryKey(pk)
	filter := mustPropertiesConstruct(plans.NewRecordQueryFilterPlan(nil, scan))
	result := computePrimaryKey(filter)
	if result == nil {
		t.Fatal("filter should inherit PK from child scan")
	}
}

func TestComputePrimaryKey_UnionCommonPK(t *testing.T) {
	t.Parallel()
	pk := []values.Value{planPropertiesField("id", 0)}
	scanA := planPropertiesScan("A").WithPrimaryKey(pk)
	scanB := planPropertiesScan("B").WithPrimaryKey(pk)
	union := mustPropertiesConstruct(plans.NewRecordQueryUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}))
	result := computePrimaryKey(union)
	if result == nil {
		t.Fatal("union with common PK should return non-nil")
	}
}

func TestComputePrimaryKey_UnionDifferentPK(t *testing.T) {
	t.Parallel()
	pkA := []values.Value{planPropertiesField("id", 0)}
	pkB := []values.Value{planPropertiesField("name", 1)}
	scanA := planPropertiesScan("A").WithPrimaryKey(pkA)
	scanB := planPropertiesScan("B").WithPrimaryKey(pkB)
	union := mustPropertiesConstruct(plans.NewRecordQueryUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}))
	result := computePrimaryKey(union)
	if result != nil {
		t.Fatal("union with different PKs should return nil")
	}
}
