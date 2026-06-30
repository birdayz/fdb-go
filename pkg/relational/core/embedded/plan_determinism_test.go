package embedded

// RFC-164 NONDETERMINISM: ranging Go maps in the planner made equal-cost ties
// resolve by Go's randomised map-iteration order → the SAME query produced
// distinct plans across runs. Java uses insertion-ordered LinkedHashMultimap and
// a stable index order. This pins the two DOCUMENTED map-iteration sources, now
// fixed:
//   1. Reference.partialMatchMap was ranged directly (GetAllPartialMatches /
//      GetPartialMatchCandidates) → now iterated in first-insertion order
//      (mirrors LinkedHashMultimap).
//   2. metadataPlanContext.GetMatchCandidates ranged RecordMetaData.GetAllIndexes
//      (a Go map) → now iterated in index-name-sorted order.
//
// The test plans one query many times in a single process (Go randomises map
// order per range, so repeated in-process plannings expose order-dependence) and
// requires ONE distinct plan.
//
// KNOWN-REMAINING (tracked in TODO.md as a multi-shift item, NOT closed here):
// a multi-equality tie over several single-column indexes (e.g.
// `WHERE a=5 AND b=7 AND c=9` with idx_a/idx_b/idx_c) is still nondeterministic.
// Root cause is deeper and architectural: nil-inner "shell" wrappers (Fetch and
// PredicatesFilter push-through templates) are costed without their eventual
// inner so they rank artificially cheap, and the extraction relink
// (findPhysicalPlan) resolves a shell's inner to the FIRST physical member of the
// child reference by member-iteration order — so on a cost tie the relinked index
// varies. The complete fix requires consistent shell handling across selection
// AND extraction with total-order tie resolution; rows are always correct (it is
// plan churn, not a wrong-rows bug), hence medium priority.

import (
	"testing"
)

func TestPlanDeterminism_EqualCostIndexTie(t *testing.T) {
	t.Parallel()
	// Two indexes on `a`: a `WHERE a = 5` scan costs the same on either, so which
	// is chosen is a pure tie — the partial-match / candidate map-order leak.
	const schema = `
CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))
CREATE INDEX idx1 ON T(a)
CREATE INDEX idx2 ON T(a)`

	assertSinglePlan(t, "SELECT id FROM t WHERE a = 5", schema, 200)
}

func assertSinglePlan(t *testing.T, sql, schema string, runs int) {
	t.Helper()
	seen := make(map[string]int)
	var first string
	for i := 0; i < runs; i++ {
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("run %d: plan: %v", i, err)
		}
		if first == "" {
			first = plan
		}
		seen[plan]++
	}
	if len(seen) != 1 {
		t.Errorf("plan is NONDETERMINISTIC over %d runs — got %d distinct plans:", runs, len(seen))
		for p, n := range seen {
			t.Errorf("  (%d×) %s", n, p)
		}
	}
}
