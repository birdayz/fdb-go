package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// computeDistinctRecords
// ---------------------------------------------------------------------------

func TestComputeDistinctRecords_ScanIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
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
		return plans.NewRecordQueryIndexPlan("idx1", nil, []string{"T"}, values.UnknownType, false)
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
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	// Put the scan in a Reference and compute its properties.
	innerRef := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	innerRef.SetPlanProperties(pm)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterPlan := plans.NewRecordQueryPredicatesFilterPlan(scan, []predicates.QueryPredicate{pred})
	innerQ := expressions.ForEachQuantifier(innerRef)
	filterWrapper := filterPlan.WithQuantifiers([]expressions.Quantifier{innerQ}).(*plans.RecordQueryPredicatesFilterPlan)

	got := computeDistinctRecords(filterWrapper, filterPlan)
	if !got {
		t.Fatal("filter over distinct scan should inherit distinct=true")
	}
}

func TestComputeDistinctRecords_StreamingAggIsFalse(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggPlan := plans.NewRecordQueryStreamingAggregationPlan(nil, keys, nil)
	// Since RFC-184 W2 the memo holds the bare plan (no physicalStreamingAggWrapper).
	got := computeDistinctRecords(aggPlan, aggPlan)
	if got {
		t.Fatal("streaming aggregation should NOT produce distinct records")
	}
}

func TestComputeDistinctRecords_DistinctPlanIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	dp := plans.NewRecordQueryDistinctPlan(scan)
	scanW := scan
	innerRef := expressions.InitialOf(scanW)
	innerQ := expressions.ForEachQuantifier(innerRef)
	dw := dp.WithQuantifiers([]expressions.Quantifier{innerQ}).(*plans.RecordQueryDistinctPlan)
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
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	up := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scan})
	scanW := scan
	innerRef := expressions.InitialOf(scanW)
	qs := []expressions.Quantifier{expressions.ForEachQuantifier(innerRef)}
	uw := plans.NewRecordQueryUnionPlanFromQuantifiers(qs)
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
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if !computeStoredRecord(scan) {
		t.Fatal("scan should produce stored records")
	}
}

func TestComputeStoredRecord_IndexIsTrue(t *testing.T) {
	t.Parallel()
	idx := plans.NewRecordQueryIndexPlan("idx1", nil, []string{"T"}, values.UnknownType, false)
	if !computeStoredRecord(idx) {
		t.Fatal("index scan should produce stored records")
	}
}

func TestComputeStoredRecord_DistinctIsTrue(t *testing.T) {
	t.Parallel()
	dp := plans.NewRecordQueryDistinctPlan(nil)
	if !computeStoredRecord(dp) {
		t.Fatal("distinct plan should produce stored records")
	}
}

func TestComputeStoredRecord_FilterInheritsFromScan(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	fp := plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	if !computeStoredRecord(fp) {
		t.Fatal("filter over scan should inherit storedRecord=true")
	}
}

func TestComputeStoredRecord_StreamingAggIsFalse(t *testing.T) {
	t.Parallel()
	aggPlan := plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	if computeStoredRecord(aggPlan) {
		t.Fatal("streaming aggregation should NOT produce stored records")
	}
}

func TestComputeStoredRecord_UnorderedUnionOfScansIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	uup := plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{scan})
	if !computeStoredRecord(uup) {
		t.Fatal("unordered union of scans should produce stored records (allChildren)")
	}
}

func TestComputeStoredRecord_UnionAllChildrenStored(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	up := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scanA, scanB})
	if !computeStoredRecord(up) {
		t.Fatal("union of scans should produce stored records")
	}
}

// ---------------------------------------------------------------------------
// PlanPropertiesMap
// ---------------------------------------------------------------------------

func TestPlanPropertiesMap_AddAndRetrieve(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
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
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
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
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
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
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
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
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	if pm := GetRefPlanPropertiesMap(ref); pm != nil {
		t.Fatalf("GetRefPlanPropertiesMap on ref with no plan properties = %v, want nil", pm)
	}
}

func TestComputeRefPlanProperties_SkipsLogicalExpressions(t *testing.T) {
	t.Parallel()
	logicalExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
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
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	msu := plans.NewRecordQueryMergeSortUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}, nil, false, true)
	if !computeDistinctRecords(msu, msu) {
		t.Fatal("MergeSortUnion should be distinct")
	}
}

func TestComputeDistinctRecords_InUnionIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	// The InUnion is its own physical expression now (RFC-184 W2) — it IS the
	// physicalPlanExpression computeDistinctRecords inspects.
	iup := plans.NewRecordQueryInUnionPlan(scan, []string{"b"}, nil, false)
	if !computeDistinctRecords(iup, iup) {
		t.Fatal("InUnion should be distinct")
	}
}

func TestComputeDistinctRecords_FirstOrDefaultIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	fod := plans.NewRecordQueryFirstOrDefaultPlan(scan, nil)
	w := scan
	if !computeDistinctRecords(w, fod) {
		t.Fatal("FirstOrDefault should be distinct")
	}
}

func TestComputeStoredRecord_FirstOrDefaultIsFalse(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	fod := plans.NewRecordQueryFirstOrDefaultPlan(scan, nil)
	if computeStoredRecord(fod) {
		t.Fatal("FirstOrDefault should NOT produce stored records")
	}
}

func TestComputeStoredRecord_DefaultOnEmptyIsFalse(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	doe := plans.NewRecordQueryDefaultOnEmptyPlan(scan, nil)
	if computeStoredRecord(doe) {
		t.Fatal("DefaultOnEmpty should NOT produce stored records")
	}
}

func TestComputeStoredRecord_InJoinInheritsFromScan(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	ijp := plans.NewRecordQueryInJoinPlan(scan, "b", false, false)
	if !computeStoredRecord(ijp) {
		t.Fatal("InJoin over scan should produce stored records")
	}
}

func TestComputeStoredRecord_MergeSortUnionAllStored(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	msu := plans.NewRecordQueryMergeSortUnionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}, nil, false, true)
	if !computeStoredRecord(msu) {
		t.Fatal("MergeSortUnion of scans should produce stored records")
	}
}

func TestComputePrimaryKey_ScanWithPK(t *testing.T) {
	t.Parallel()
	pk := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).WithPrimaryKey(pk)
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
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	result := computePrimaryKey(scan)
	if result != nil {
		t.Fatal("scan without PK should return nil")
	}
}

func TestComputePrimaryKey_FilterInheritsFromScan(t *testing.T) {
	t.Parallel()
	pk := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).WithPrimaryKey(pk)
	filter := plans.NewRecordQueryFilterPlan(nil, scan)
	result := computePrimaryKey(filter)
	if result == nil {
		t.Fatal("filter should inherit PK from child scan")
	}
}

func TestComputePrimaryKey_UnionCommonPK(t *testing.T) {
	t.Parallel()
	pk := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false).WithPrimaryKey(pk)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false).WithPrimaryKey(pk)
	union := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scanA, scanB})
	result := computePrimaryKey(union)
	if result == nil {
		t.Fatal("union with common PK should return non-nil")
	}
}

func TestComputePrimaryKey_UnionDifferentPK(t *testing.T) {
	t.Parallel()
	pkA := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	pkB := []values.Value{&values.FieldValue{Field: "name", Typ: values.UnknownType}}
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false).WithPrimaryKey(pkA)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false).WithPrimaryKey(pkB)
	union := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scanA, scanB})
	result := computePrimaryKey(union)
	if result != nil {
		t.Fatal("union with different PKs should return nil")
	}
}
