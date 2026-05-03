package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// computeDistinctRecords
// ---------------------------------------------------------------------------

func TestComputeDistinctRecords_ScanIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}
	got := computeDistinctRecords(wrapper, scan)
	if !got {
		t.Fatal("scan should produce distinct records")
	}
}

func TestComputeDistinctRecords_UniqueIndexIsTrue(t *testing.T) {
	t.Parallel()
	idx := plans.NewRecordQueryIndexPlan("idx1", nil, []string{"T"}, values.UnknownType, false)
	wrapper := &physicalIndexScanWrapper{plan: idx, unique: true}
	got := computeDistinctRecords(wrapper, idx)
	if !got {
		t.Fatal("unique index scan should produce distinct records")
	}
}

func TestComputeDistinctRecords_NonUniqueIndexIsFalse(t *testing.T) {
	t.Parallel()
	idx := plans.NewRecordQueryIndexPlan("idx1", nil, []string{"T"}, values.UnknownType, false)
	wrapper := &physicalIndexScanWrapper{plan: idx, unique: false}
	got := computeDistinctRecords(wrapper, idx)
	if got {
		t.Fatal("non-unique index scan should NOT produce distinct records")
	}
}

func TestComputeDistinctRecords_FilterInheritsFromChild(t *testing.T) {
	t.Parallel()
	// Build: physicalFilterWrapper over physicalScanWrapper (distinct=true).
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanWrapper := &physicalScanWrapper{plan: scan}

	// Put the scan wrapper in a Reference and compute its properties.
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{scanWrapper})
	pm := NewPlanPropertiesMap()
	pm.Add(scanWrapper)
	innerRef.SetPlanProperties(pm)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterPlan := plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	innerQ := expressions.ForEachQuantifier(innerRef)
	filterWrapper := NewPhysicalFilterWrapper(filterPlan, innerQ)

	got := computeDistinctRecords(filterWrapper, filterPlan)
	if !got {
		t.Fatal("filter over distinct scan should inherit distinct=true")
	}
}

func TestComputeDistinctRecords_HashAggIsFalse(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggPlan := plans.NewRecordQueryHashAggregationPlan(nil, keys, nil)
	wrapper := &physicalHashAggWrapper{plan: aggPlan}
	got := computeDistinctRecords(wrapper, aggPlan)
	if got {
		t.Fatal("hash aggregation should NOT produce distinct records")
	}
}

func TestComputeDistinctRecords_DistinctPlanIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	dp := plans.NewRecordQueryDistinctPlan(scan)
	scanW := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{scanW})
	innerQ := expressions.ForEachQuantifier(innerRef)
	dw := NewPhysicalDistinctWrapper(dp, innerQ)
	got := computeDistinctRecords(dw, dp)
	if !got {
		t.Fatal("distinct plan should produce distinct records")
	}
}

func TestComputeDistinctRecords_UnionPlanIsTrue(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	up := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scan})
	scanW := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{scanW})
	qs := []expressions.Quantifier{expressions.ForEachQuantifier(innerRef)}
	uw := NewPhysicalUnionWrapper(up, qs)
	got := computeDistinctRecords(uw, up)
	if !got {
		t.Fatal("union plan should produce distinct records")
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

func TestComputeStoredRecord_HashAggIsFalse(t *testing.T) {
	t.Parallel()
	aggPlan := plans.NewRecordQueryHashAggregationPlan(nil, nil, nil)
	if computeStoredRecord(aggPlan) {
		t.Fatal("hash aggregation should NOT produce stored records")
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
	wrapper := &physicalScanWrapper{plan: scan}

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
	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}

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
	wrapper := &physicalScanWrapper{plan: scan}
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
	wrapper := &physicalScanWrapper{plan: scan}
	ref := expressions.NewFinalReference([]expressions.RelationalExpression{wrapper})

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
	// Reference with only logical members — no physical wrappers.
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
