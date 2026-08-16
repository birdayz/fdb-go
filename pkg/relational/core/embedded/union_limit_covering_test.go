package embedded

import (
	"strings"
	"testing"
)

// unionLimitCoveringDDL gives each UNION branch its own composite index whose
// entry already carries the projected column, so each branch is independently
// coverable. Two tables rather than one so a single index cannot satisfy both
// and accidentally make the second branch's outcome a copy of the first's.
const unionLimitCoveringDDL = `CREATE TABLE rp (id BIGINT, region STRING, plan BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_region_plan ON rp(region, plan)
CREATE TABLE rq (id BIGINT, region STRING, plan BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_region_plan_q ON rq(region, plan)`

// TestUnionUnderLimitKeepsCoveringInEveryBranch is the sibling check for the
// rule that survived when PushLimitThroughProjectionRule was deleted.
//
// The deleted rule cost every covering rewrite under a LIMIT: it reordered
// Limit(Project(X)) into Project(Limit(X)) during REWRITING, where partition
// retention does not run, so REWRITING pruned to the pushed survivor and the
// un-pushed shape — whose inner group held the covering winner — never reached
// PLANNING. PushLimitThroughUnionRule sits in the SAME rule set, in the same
// phase, and also rewrites a LIMIT on a physical-cost argument the phase cannot
// consult. So the obvious question is whether the deletion fixed one instance
// of a class while leaving its twin in place.
//
// It did not, and the reason is structural rather than lucky. The union rule
// WRAPS instead of reordering: it yields Limit(Union(Limit(q1), Limit(q2)))
// while reusing the ORIGINAL branch quantifiers, so each branch's own reference
// stays a child and still reaches PLANNING with its covering alternative
// intact. The projection rule reordered two operators and orphaned the group in
// between; nothing here is orphaned.
//
// This pins the OUTCOME rather than the argument, because the argument is the
// part that rots. If a future change gives the union rule the reordering shape
// — rebuilding branch references instead of reusing them — this reddens, and
// the fix is the same as it was for the projection rule.
func TestUnionUnderLimitKeepsCoveringInEveryBranch(t *testing.T) {
	t.Parallel()

	const sql = `SELECT id FROM rp WHERE region = 'eu'
		UNION ALL
		SELECT id FROM rq WHERE region = 'eu'
		LIMIT 2`

	got, err := PlanQueryForTest(sql, unionLimitCoveringDDL, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("plan: %s", got)

	// Both branches, named individually. A single "contains COVERING" check
	// would pass with one branch covering and the other fetching, which is the
	// asymmetric regression this is most likely to see.
	for _, want := range []string{
		"IndexScan(IDX_REGION_PLAN, [=, *] COVERING)",
		"IndexScan(IDX_REGION_PLAN_Q, [=, *] COVERING)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("branch scan %q missing — a LIMIT over the union cost that branch its covering "+
				"rewrite, which is the defect deleting PushLimitThroughProjectionRule fixed for the "+
				"projection shape:\n%s", want, got)
		}
	}

	// Vacuity guard: the assertions above say nothing unless the LIMIT is
	// actually present and actually above the union. A parse that dropped it,
	// or a rewrite that sank it into the branches, would leave two covering
	// scans and a test that had stopped asking its question.
	if !strings.HasPrefix(got, "Limit(2, UnorderedUnion(") {
		t.Fatalf("plan does not start with a LIMIT directly over the union, so this test is not "+
			"exercising a limit-over-union at all:\n%s", got)
	}
}
