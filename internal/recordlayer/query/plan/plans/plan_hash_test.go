package plans

import (
	"testing"
)

func TestPlanHash_Deterministic(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	h1 := PlanHash(scan)
	h2 := PlanHash(scan)
	if h1 != h2 {
		t.Fatalf("PlanHash not deterministic: %d vs %d", h1, h2)
	}
}

func TestPlanHash_DifferentPlansHaveDifferentHash(t *testing.T) {
	t.Parallel()
	scanA := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"A"}, exactTestRecordType(), false)
	})
	scanB := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"B"}, exactTestRecordType(), false)
	})
	if PlanHash(scanA) == PlanHash(scanB) {
		t.Fatal("different scans should have different hashes")
	}
}

func TestPlanHash_TreeStructureMatters(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	filter := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan(nil, scan)
	})
	distinct := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(scan)
	})

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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	filter := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan(nil, scan)
	})
	nested := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan(nil, filter)
	})

	if PlanHash(filter) == PlanHash(nested) {
		t.Fatal("different depths should hash differently")
	}
}

func TestPlanHash_IdenticalScansAgree(t *testing.T) {
	t.Parallel()
	scanA := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	scanB := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	// Hash AGREEMENT only. Deliberately not spelled as an equality helper:
	// plan equality is now finer than PlanHash (RFC-183 §15 compares scan
	// children structurally while the hash stays node-local), so a
	// "PlanHashEqual" predicate would answer a question nobody should ask.
	// Equal plans still hash equal; unequal plans MAY collide.
	if PlanHash(scanA) != PlanHash(scanB) {
		t.Fatal("identical scans should hash the same")
	}
}
