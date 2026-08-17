package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestConcretePlanCounts_DefaultOnEmpty pins RFC-188 finding 10 M2: the cost
// model must COUNT RecordQueryDefaultOnEmptyPlan nodes so the "fewer ON EMPTY
// NULL wins" rung (Java PlanningCostModel, the last ordinal rung before the
// planHash tiebreak) can fire. Before M2 the count was never taken, so two
// plans differing only in ON-EMPTY-NULL count fell straight to the hash
// tiebreak.
func TestConcretePlanCounts_DefaultOnEmpty(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("default_on_empty_count_row", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
	})
	scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	if err != nil {
		t.Fatalf("construct exact scan: %v", err)
	}
	if got := concretePlanCounts(scan, nil).numDefaultOnEmpty; got != 0 {
		t.Fatalf("bare scan numDefaultOnEmpty = %d, want 0", got)
	}

	doe, err := plans.NewRecordQueryDefaultOnEmptyPlan(scan, values.NewNullValue(rowType))
	if err != nil {
		t.Fatalf("construct first DefaultOnEmpty: %v", err)
	}
	if got := concretePlanCounts(doe, nil).numDefaultOnEmpty; got != 1 {
		t.Fatalf("DefaultOnEmpty(scan) numDefaultOnEmpty = %d, want 1", got)
	}

	// Nested ON EMPTY NULL: both are counted.
	doe2, err := plans.NewRecordQueryDefaultOnEmptyPlan(doe, values.NewNullValue(rowType))
	if err != nil {
		t.Fatalf("construct nested DefaultOnEmpty: %v", err)
	}
	if got := concretePlanCounts(doe2, nil).numDefaultOnEmpty; got != 2 {
		t.Fatalf("DefaultOnEmpty(DefaultOnEmpty(scan)) numDefaultOnEmpty = %d, want 2", got)
	}
}
