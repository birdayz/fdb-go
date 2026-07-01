package embedded

// RFC-164 COST-SELECTIVITY: when an equality predicate and a range predicate are
// each served by a different single-column index, the planner must pick the
// EQUALITY index — a point match is more selective than a 1/3-domain range. The
// original cost model costed equality at FilterSelectivity=0.5 > RangeSelectivity=0.33,
// so the range index looked cheaper and won. This pins the corrected choice.

import (
	"strings"
	"testing"
)

func TestCostSelectivity_PrefersEqualityIndex(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_a ON T(a)
CREATE INDEX idx_b ON T(b)`

	// a = 5 (equality, idx_a) AND b > 10 (range, idx_b). The equality index is
	// more selective → it must be the chosen scan, with b>10 as a residual filter.
	plan, err := PlanQueryForTest("SELECT id FROM t WHERE a = 5 AND b > 10", schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "IDX_A") {
		t.Errorf("equality predicate should drive the scan via idx_a, got: %s", plan)
	}
	if strings.Contains(plan, "IDX_B") {
		t.Errorf("range index idx_b should NOT be chosen over the equality index idx_a, got: %s", plan)
	}
}
