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

// TestNestedIn_OverIntersection_GatePin pins the one shape the relink fixes do
// NOT reach: TWO IN predicates plus an indexed equality. The inner IN level's
// wrapper is never handed to WithChildren at all, so its nil-inner snapshot
// survives — an instance of RFC-167's nil-inner-shell architecture (the shell
// is not a member of any reference the relink walks, so no amount of
// per-wrapper relinking reaches it).
//
// PRE-EXISTING and LOUD: identical failure on master before any of this
// branch's changes, and it surfaces as the XX000 plan invariant — never wrong
// rows. This is a GATE PIN, not an endorsement: it asserts the current loud
// decline so the day the planner learns the shape this test goes RED and the
// author replaces it with the real plan/row assertions.
func TestNestedIn_OverIntersection_GatePin(t *testing.T) {
	t.Parallel()
	const q = "SELECT * FROM t_rd WHERE (b IN (7,5)) AND (c = 5) AND (a IN (5,9,3,10)) LIMIT 12"
	plan, err := PlanQueryForTest(q, inRelinkSchema, nil)
	if err == nil {
		t.Fatalf("GATE PIN FIRED — nested IN over an intersection now plans as %s.\n"+
			"The RFC-167 shell gap is (partly) closed: verify the plan is correct, then replace\n"+
			"this gate pin with real plan-shape + FDB row assertions.", plan)
	}
	if !strings.Contains(err.Error(), "plan-invariant") {
		t.Fatalf("expected the loud plan-invariant decline, got a different error: %v", err)
	}
}
