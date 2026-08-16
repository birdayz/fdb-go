package embedded

import (
	"strings"
	"testing"
)

// limitCoveringDDL is order_by_elimination.yaml's `rp` fixture, reproduced here
// so this pin runs without the FDB conformance harness: a composite secondary
// index whose entry already carries the projected column, which is what makes
// the projection coverable.
const limitCoveringDDL = `CREATE TABLE rp (id BIGINT, region STRING, plan BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_region_plan ON rp(region, plan)`

// TestLimitOverProjectionKeepsTheCoveringRewrite pins the reachability of the
// covering plan under a LIMIT, on the axis that a whole-plan text assertion in
// the yamsql corpus cannot state: WHY the covering member is in the memo group
// at all.
//
// `SELECT id FROM rp WHERE region = 'eu' ORDER BY plan DESC LIMIT 1` is
// coverable — IDX_REGION_PLAN's entry carries ID, so the projection needs no
// record fetch. Go used to emit the NON-covering plan
// `Project([_current.ID#0], Limit(1, IndexScan(IDX_REGION_PLAN, [=, *]) REVERSE))`
// because a Go-only PushLimitThroughProjectionRule rewrote `Limit(Project(X))`
// into `Project(Limit(X))` during REWRITING. Partition retention is gated to
// PLANNING, so REWRITING pruned to the single pushed survivor and the original
// shape — whose inner group holds the covering winner — never reached the
// phase where the covering rewrite runs. The cost model was never consulted:
// the better member was absent, not outranked.
//
// Java has no such rule. It expresses a row limit as
// ExecuteProperties.setReturnedRowLimit() at execution, not as a planner
// rewrite, so there is nothing to push and nothing to prune against. Deleting
// the rule restores the covering plan and matches Java's structure.
//
// IF THIS TEST FAILS, a rewrite is again pruning the un-pushed `Limit(Project)`
// shape out of REWRITING before PLANNING can offer it the covering
// alternative. Look for a new expression rule that rebuilds a LIMIT, not for a
// cost-model change: the two plans have never been costed against each other.
func TestLimitOverProjectionKeepsTheCoveringRewrite(t *testing.T) {
	t.Parallel()
	const sql = `SELECT id FROM rp WHERE region = 'eu' ORDER BY plan DESC LIMIT 1`

	got := explainWithOptions(t, sql, limitCoveringDDL, nil)
	const want = "Limit(1, Project([_current.ID#0], IndexScan(IDX_REGION_PLAN, [=, *] COVERING) REVERSE))"
	if got != want {
		t.Errorf("plan = %q, want %q", got, want)
	}
	// Stated separately from the whole-text compare above so a cosmetic
	// rendering change to the plan printer cannot quietly turn this pin into a
	// test of the printer while the fetch it exists to forbid comes back.
	if !strings.Contains(got, "COVERING") {
		t.Error("plan is not covering: the projection is served by a record fetch the index entry already satisfies")
	}
}
