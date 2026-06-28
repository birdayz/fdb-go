package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlanHash_Deterministic(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	h1 := PlanHash(scan)
	h2 := PlanHash(scan)
	if h1 != h2 {
		t.Fatalf("PlanHash not deterministic: %d vs %d", h1, h2)
	}
}

func TestPlanHash_DifferentPlansHaveDifferentHash(t *testing.T) {
	t.Parallel()
	scanA := NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	if PlanHash(scanA) == PlanHash(scanB) {
		t.Fatal("different scans should have different hashes")
	}
}

func TestPlanHash_TreeStructureMatters(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	filter := NewRecordQueryFilterPlan(nil, scan)
	distinct := NewRecordQueryDistinctPlan(scan)

	if PlanHash(filter) == PlanHash(distinct) {
		t.Fatal("filter and distinct over same scan should hash differently")
	}
}

func TestPlanHash_NilPlan(t *testing.T) {
	t.Parallel()
	if PlanHash(nil) != 0 {
		t.Fatal("nil plan should hash to 0")
	}
}

func TestPlanHash_DepthMatters(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	filter := NewRecordQueryFilterPlan(nil, scan)
	nested := NewRecordQueryFilterPlan(nil, filter)

	if PlanHash(filter) == PlanHash(nested) {
		t.Fatal("different depths should hash differently")
	}
}

func TestPlanHashEqual(t *testing.T) {
	t.Parallel()
	scanA := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanB := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if !PlanHashEqual(scanA, scanB) {
		t.Fatal("identical scans should be hash-equal")
	}
}
