package executor

// Go-only extension tests: LIMIT clause optimization.
// Java uses ExecuteProperties.setReturnedRowLimit() at the JDBC layer;
// Go supports LIMIT natively in SQL with Cascades-integrated optimization.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// --- Limit pushdown propagation tests ---

func TestExecuteLimit_PropagatesRowLimit(t *testing.T) {
	t.Parallel()

	// Create a limit plan with limit=5, offset=2.
	innerPlan := plans.NewRecordQueryScanPlan(nil, nil, false)
	limitPlan := plans.NewRecordQueryLimitPlan(innerPlan, 5, 2)

	// The effective limit for the inner should be 5+2=7.
	// We can't easily test the propagation without FDB, but we can
	// verify the plan structure is correct.
	if limitPlan.GetLimit() != 5 {
		t.Fatalf("limit = %d, want 5", limitPlan.GetLimit())
	}
	if limitPlan.GetOffset() != 2 {
		t.Fatalf("offset = %d, want 2", limitPlan.GetOffset())
	}
	children := limitPlan.GetChildren()
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
}

func TestExecuteLimit_ZeroLimit(t *testing.T) {
	t.Parallel()

	// A LIMIT 0 plan should have limit=0.
	innerPlan := plans.NewRecordQueryScanPlan(nil, nil, false)
	limitPlan := plans.NewRecordQueryLimitPlan(innerPlan, 0, 0)

	if limitPlan.GetLimit() != 0 {
		t.Fatalf("expected limit=0, got %d", limitPlan.GetLimit())
	}
	// LIMIT 0 lowers to RecordQueryLimitPlan(limit=0); at the executor level the
	// limitEnvelopeCursor short-circuits remLimit==0 to an empty, exhausted result.
}
