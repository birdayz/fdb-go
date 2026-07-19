package embedded

// Extraction-relink pins for IN predicates over a compensated pk-intersection
// (found by the RFC-182 rowdiff harness once its generator learned IN/LIMIT).
//
// Before the fix these shapes extracted `PredicatesFilter(<nil>)` /
// `InUnion(PredicatesFilter(<nil>))` and failed the XX000 plan invariant; on
// the tree BEFORE compensated intersections existed, the very same queries
// silently DROPPED the residual instead (wrong rows). Three defects combined:
//   - a set operation was missing from isLeafReplaceable, so a filter over an
//     intersection refused to relink and kept its nil-inner snapshot;
//   - the IN-union wrapper had NO relink at all, so it held the stale
//     pre-relink snapshot of its (correctly relinked) child;
//   - the nil-inner shell guard was Fetch-only, so nil-inner filter shells
//     were picked as if they were valid plans.

import (
	"strings"
	"testing"
)

const inRelinkSchema = `
CREATE TABLE T_RD (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, PRIMARY KEY (id))
CREATE INDEX idx_c ON T_RD (c)
CREATE INDEX idx_b ON T_RD (b)`

func TestInOverIntersection_RelinksResidual(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
		preds string // expected residual count marker in the filter
	}{
		{
			name:  "in_residual",
			query: "SELECT * FROM t_rd WHERE (b = 1) AND (c = 9) AND (a IN (1, 3, 7))",
			preds: "[1 preds]",
		},
		{
			name:  "in_plus_scalar_residual",
			query: "SELECT * FROM t_rd WHERE (b = 1) AND (c = 9) AND (s = 'x') AND (a IN (1, 3))",
			preds: "[2 preds]",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.query, inRelinkSchema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if strings.Contains(plan, "<nil>") {
				t.Fatalf("relink dropped an inner: %s", plan)
			}
			if !strings.Contains(plan, "Intersection(") {
				t.Errorf("want the pk-intersection retained, got: %s", plan)
			}
			if !strings.Contains(plan, "PredicatesFilter(") || !strings.Contains(plan, tc.preds) {
				t.Errorf("want the residual reapplied as %s, got: %s", tc.preds, plan)
			}
		})
	}
}

// TestNestedIn_OverIntersection pins the shape that used to extract
// `InJoin(<nil>)` and fail the XX000 plan invariant: TWO IN predicates plus
// an indexed equality. It began life as a GATE PIN asserting that loud
// decline; the gap is now closed, so it asserts the real plan instead.
//
// Two pieces closed it. (1) A wrapper holding a MALFORMED inner now always
// relinks: the isLeafReplaceable gate exists to stop a meaningful child
// being swapped, and a plan with no child is not meaningful — previously an
// `InUnion` holding a nil-inner `InJoin` refused every relink because the
// SHELL's type is not leaf-replaceable. (2) A reference whose members are
// ALL shells is completed recursively (completeShellPlan), rebuilding
// through each plan's WithInner rather than re-entering WithChildren —
// which is what makes its depth bound real.
func TestNestedIn_OverIntersection(t *testing.T) {
	t.Parallel()
	const q = "SELECT * FROM t_rd WHERE (b IN (7,5)) AND (c = 5) AND (a IN (5,9,3,10)) LIMIT 12"
	plan, err := PlanQueryForTest(q, inRelinkSchema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if strings.Contains(plan, "<nil>") {
		t.Fatalf("relink left a nil inner: %s", plan)
	}
	// Every IN level must carry a real child; the innermost access is the
	// index scan for the equality.
	for _, want := range []string{"InJoin(", "IndexScan("} {
		if !strings.Contains(plan, want) {
			t.Errorf("want %s in the plan, got: %s", want, plan)
		}
	}
}
