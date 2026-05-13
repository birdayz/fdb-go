package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// PlanPartition accessors
// ---------------------------------------------------------------------------

func TestPlanPartition_GetPlans(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}

	pp := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true},
		map[expressions.RelationalExpression]properties.PropertyMap{
			wrapper: {properties.PropDistinctRecords: true},
		},
	)

	gotPlans := pp.GetPlans()
	if len(gotPlans) != 1 {
		t.Fatalf("GetPlans() length = %d, want 1", len(gotPlans))
	}
	if gotPlans[0] != scan {
		t.Fatal("GetPlans()[0] should be the original scan plan")
	}
}

func TestPlanPartition_GetExpressions(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}

	pp := NewPlanPartition(
		properties.PropertyMap{},
		map[expressions.RelationalExpression]properties.PropertyMap{
			wrapper: {},
		},
	)

	exprs := pp.GetExpressions()
	if len(exprs) != 1 {
		t.Fatalf("GetExpressions() length = %d, want 1", len(exprs))
	}
}

func TestPlanPartition_IsDistinct(t *testing.T) {
	t.Parallel()
	pp := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true},
		nil,
	)
	if !pp.IsDistinct() {
		t.Fatal("IsDistinct() should be true")
	}

	pp2 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: false},
		nil,
	)
	if pp2.IsDistinct() {
		t.Fatal("IsDistinct() should be false")
	}
}

func TestPlanPartition_IsStoredRecord(t *testing.T) {
	t.Parallel()
	pp := NewPlanPartition(
		properties.PropertyMap{properties.PropStoredRecord: true},
		nil,
	)
	if !pp.IsStoredRecord() {
		t.Fatal("IsStoredRecord() should be true")
	}

	pp2 := NewPlanPartition(
		properties.PropertyMap{properties.PropStoredRecord: false},
		nil,
	)
	if pp2.IsStoredRecord() {
		t.Fatal("IsStoredRecord() should be false")
	}
}

func TestPlanPartition_HasPrimaryKey(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	pp := NewPlanPartition(
		properties.PropertyMap{},
		map[expressions.RelationalExpression]properties.PropertyMap{
			sw: {properties.PropPrimaryKey: "pk-value"},
		},
	)
	if !pp.HasPrimaryKey() {
		t.Fatal("HasPrimaryKey() should be true when expression has PK")
	}

	pp2 := NewPlanPartition(
		properties.PropertyMap{},
		map[expressions.RelationalExpression]properties.PropertyMap{
			sw: {properties.PropPrimaryKey: nil},
		},
	)
	if pp2.HasPrimaryKey() {
		t.Fatal("HasPrimaryKey() should be false when PK is nil")
	}

	pp3 := NewPlanPartition(properties.PropertyMap{}, nil)
	if pp3.HasPrimaryKey() {
		t.Fatal("HasPrimaryKey() should be false when no expressions")
	}
}

func TestPlanPartition_GetOrdering(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	want := properties.Ordering{IsKnown: true}
	pp := NewPlanPartition(
		properties.PropertyMap{},
		map[expressions.RelationalExpression]properties.PropertyMap{
			sw: {properties.PropOrdering: want},
		},
	)
	got := pp.GetOrdering()
	if !got.IsKnown {
		t.Fatal("GetOrdering() should return expression's ordering")
	}

	pp2 := NewPlanPartition(properties.PropertyMap{}, nil)
	got2 := pp2.GetOrdering()
	if got2.IsKnown {
		t.Fatal("GetOrdering() should return zero ordering when no expressions")
	}
}

// ---------------------------------------------------------------------------
// ToPlanPartitions
// ---------------------------------------------------------------------------

func TestToPlanPartitions_WithPrecomputedPropertiesMap(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}

	// Pre-compute properties and set on reference.
	ref := expressions.NewFinalReference([]expressions.RelationalExpression{wA})
	pm := NewPlanPropertiesMap()
	pm.Add(wA)
	ref.SetPlanProperties(pm)

	partitions := ToPlanPartitions(ref)
	if len(partitions) == 0 {
		t.Fatal("ToPlanPartitions should return at least one partition")
	}

	// All expressions from all partitions should sum to 1.
	total := 0
	for _, p := range partitions {
		total += len(p.GetExpressions())
	}
	if total != 1 {
		t.Fatalf("total expressions across partitions = %d, want 1", total)
	}
}

func TestToPlanPartitions_GroupsByDistinctAndStored(t *testing.T) {
	t.Parallel()
	// Two wrappers with different properties should land in different partitions.
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}

	// Streaming agg has distinct=false, stored=false.
	aggPlan := plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	wB := &physicalStreamingAggWrapper{plan: aggPlan}

	ref := expressions.NewFinalReference([]expressions.RelationalExpression{wA, wB})
	pm := NewPlanPropertiesMap()
	pm.Add(wA)
	pm.Add(wB)
	ref.SetPlanProperties(pm)

	partitions := ToPlanPartitions(ref)
	if len(partitions) != 2 {
		t.Fatalf("expected 2 partitions (distinct vs non-distinct), got %d", len(partitions))
	}
}

func TestToPlanPartitions_FallbackWhenNoPropertiesMap(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}
	ref := expressions.NewFinalReference([]expressions.RelationalExpression{wrapper})
	// Don't set plan properties — triggers fallback.

	partitions := ToPlanPartitions(ref)
	if len(partitions) == 0 {
		t.Fatal("fallback should still produce partitions")
	}
}

func TestToPlanPartitions_NilRef(t *testing.T) {
	t.Parallel()
	// GetRefPlanPropertiesMap returns nil for nil ref, toPlanPartitionsFallback
	// also returns nil for nil ref.
	partitions := ToPlanPartitions(nil)
	if partitions != nil {
		t.Fatalf("ToPlanPartitions(nil) = %v, want nil", partitions)
	}
}

// ---------------------------------------------------------------------------
// RollUpPlanPartitions
// ---------------------------------------------------------------------------

func TestRollUpPlanPartitions_MergeAll(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}

	p1 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true, properties.PropStoredRecord: true},
		map[expressions.RelationalExpression]properties.PropertyMap{wA: {}},
	)
	p2 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: false, properties.PropStoredRecord: false},
		map[expressions.RelationalExpression]properties.PropertyMap{wB: {}},
	)

	// No interesting props — merge all partitions into one.
	rolled := RollUpPlanPartitions([]*PlanPartition{p1, p2})
	if len(rolled) != 1 {
		t.Fatalf("RollUpPlanPartitions with no interesting props: got %d partitions, want 1", len(rolled))
	}
	if len(rolled[0].GetExpressions()) != 2 {
		t.Fatalf("merged partition should have 2 expressions, got %d", len(rolled[0].GetExpressions()))
	}
}

func TestRollUpPlanPartitions_RetainSingleProperty(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	scanC := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}
	wC := &physicalScanWrapper{plan: scanC}

	p1 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true, properties.PropStoredRecord: true},
		map[expressions.RelationalExpression]properties.PropertyMap{wA: {}},
	)
	p2 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true, properties.PropStoredRecord: false},
		map[expressions.RelationalExpression]properties.PropertyMap{wB: {}},
	)
	p3 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: false, properties.PropStoredRecord: true},
		map[expressions.RelationalExpression]properties.PropertyMap{wC: {}},
	)

	// Retain only PropDistinctRecords — p1 and p2 should merge (both distinct=true).
	rolled := RollUpPlanPartitions([]*PlanPartition{p1, p2, p3}, properties.PropDistinctRecords)
	if len(rolled) != 2 {
		t.Fatalf("RollUpPlanPartitions retaining distinctRecords: got %d partitions, want 2", len(rolled))
	}

	// Find the distinct=true partition and verify it has 2 expressions.
	for _, rp := range rolled {
		if rp.IsDistinct() {
			if len(rp.GetExpressions()) != 2 {
				t.Fatalf("distinct=true partition should have 2 expressions, got %d", len(rp.GetExpressions()))
			}
		} else {
			if len(rp.GetExpressions()) != 1 {
				t.Fatalf("distinct=false partition should have 1 expression, got %d", len(rp.GetExpressions()))
			}
		}
	}
}

func TestRollUpPlanPartitions_Empty(t *testing.T) {
	t.Parallel()
	rolled := RollUpPlanPartitions(nil)
	if rolled != nil {
		t.Fatalf("RollUpPlanPartitions(nil) = %v, want nil", rolled)
	}
}

// ---------------------------------------------------------------------------
// AllAttributesExcept
// ---------------------------------------------------------------------------

func TestAllAttributesExcept_ExcludeOne(t *testing.T) {
	t.Parallel()
	result := AllAttributesExcept(properties.PropOrdering)
	if len(result) != len(properties.AllPlanProperties)-1 {
		t.Fatalf("AllAttributesExcept(ordering) length = %d, want %d",
			len(result), len(properties.AllPlanProperties)-1)
	}
	for _, p := range result {
		if p == properties.PropOrdering {
			t.Fatal("AllAttributesExcept should not include excluded property")
		}
	}
}

func TestAllAttributesExcept_ExcludeNone(t *testing.T) {
	t.Parallel()
	result := AllAttributesExcept()
	if len(result) != len(properties.AllPlanProperties) {
		t.Fatalf("AllAttributesExcept() length = %d, want %d",
			len(result), len(properties.AllPlanProperties))
	}
}

func TestAllAttributesExcept_ExcludeAll(t *testing.T) {
	t.Parallel()
	result := AllAttributesExcept(properties.AllPlanProperties...)
	if len(result) != 0 {
		t.Fatalf("AllAttributesExcept(all) length = %d, want 0", len(result))
	}
}

func TestAllAttributesExcept_ExcludeTwo(t *testing.T) {
	t.Parallel()
	result := AllAttributesExcept(properties.PropOrdering, properties.PropPrimaryKey)
	if len(result) != len(properties.AllPlanProperties)-2 {
		t.Fatalf("AllAttributesExcept(ordering,primaryKey) length = %d, want %d",
			len(result), len(properties.AllPlanProperties)-2)
	}
	for _, p := range result {
		if p == properties.PropOrdering || p == properties.PropPrimaryKey {
			t.Fatalf("AllAttributesExcept should not include %s", p)
		}
	}
}

func TestFilterPlanPartitions(t *testing.T) {
	t.Parallel()

	p1 := &PlanPartition{
		partitionProps: properties.PropertyMap{
			properties.PropDistinctRecords: true,
			properties.PropStoredRecord:    false,
		},
		exprProps: make(map[expressions.RelationalExpression]properties.PropertyMap),
	}
	p2 := &PlanPartition{
		partitionProps: properties.PropertyMap{
			properties.PropDistinctRecords: false,
			properties.PropStoredRecord:    true,
		},
		exprProps: make(map[expressions.RelationalExpression]properties.PropertyMap),
	}
	p3 := &PlanPartition{
		partitionProps: properties.PropertyMap{
			properties.PropDistinctRecords: true,
			properties.PropStoredRecord:    true,
		},
		exprProps: make(map[expressions.RelationalExpression]properties.PropertyMap),
	}

	all := []*PlanPartition{p1, p2, p3}

	distinct := WhereDistinct(all)
	if len(distinct) != 2 {
		t.Fatalf("expected 2 distinct partitions, got %d", len(distinct))
	}

	stored := WhereStored(all)
	if len(stored) != 2 {
		t.Fatalf("expected 2 stored partitions, got %d", len(stored))
	}

	custom := FilterPlanPartitions(all, func(p *PlanPartition) bool {
		return p.IsDistinct() && p.IsStoredRecord()
	})
	if len(custom) != 1 {
		t.Fatalf("expected 1 distinct+stored partition, got %d", len(custom))
	}

	empty := FilterPlanPartitions(nil, func(p *PlanPartition) bool { return true })
	if len(empty) != 0 {
		t.Fatalf("filtering nil should return empty, got %d", len(empty))
	}
}

func TestSelectMinCostPartition_Empty(t *testing.T) {
	t.Parallel()
	result := SelectMinCostPartition(nil)
	if result != nil {
		t.Fatal("expected nil for empty partitions")
	}
}

func TestSelectMinCostPartition_Single(t *testing.T) {
	t.Parallel()
	p := &PlanPartition{
		partitionProps: properties.PropertyMap{},
		exprProps:      make(map[expressions.RelationalExpression]properties.PropertyMap),
	}
	result := SelectMinCostPartition([]*PlanPartition{p})
	if result != p {
		t.Fatal("single partition should be returned")
	}
}
