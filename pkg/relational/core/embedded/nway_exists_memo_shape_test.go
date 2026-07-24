package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// The N-way projected-EXISTS shape (`SELECT a.v, EXISTS(…) FROM a, b, c WHERE …`,
// N>2 ForEach legs plus a trailing Existential in one flattened Select) used to
// reach ImplementNestedLoopJoinRule's ordinal N-way arm, which built a FlatMap
// whose outer child ranged over a group that could only produce `Scan(NA)` — the
// optimizer costing a single table scan for a three-way join — and which never
// executed at all ("multi-leg row cannot serve a source-relative ordinal").
//
// RFC-190 190.1 retired that arm (ImplementNestedLoopJoinRule's `len(quants)-1 >
// 2` branch no longer calls it) and replaced its coverage with a direct-emit
// translation (buildExistentialJoinSelect dissolves the N-way cluster into a
// flat NAME-model select) that PartitionSelectRule decomposes into ordinary
// 2-quantifier FlatMap/NLJ pairs — a shape that DOES execute (see the FDB-backed
// TestFDB_NWayCommaJoinProjectedExists / TestFDB_BuriedInnerJoinProjectedExists
// family in pkg/relational/sqldriver, which prove correct rows end-to-end).
//
// This test now pins the surviving invariant: the memo's costed plan for this
// shape is REACHABLE (RFC-183) — i.e. planning doesn't cost a plan the executor
// can't actually run — and that the fired plan is the expected FlatMap/
// FirstOrDefault existential-fold shape, not some degenerate fallback.
func TestNWayProjectedExists_OuterQuantifierMatchesExecutedPlan(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE na (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))
CREATE TABLE nb (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))
CREATE TABLE nc (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))
CREATE TABLE nd (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))`

	// A PROJECTED exists (in the select list) over >2 ForEach legs.
	const sql = `SELECT a.v, EXISTS (SELECT 1 FROM nd d WHERE d.id = a.id) AS has_d
	             FROM na a, nb b, nc c
	             WHERE a.id = b.id AND b.id = c.id`

	// The collector is OWNED BY THIS TEST and threaded into the planner, so
	// t.Parallel is safe: a tally shared across concurrent planners is not a
	// measurement (RFC-183, ReachabilityCollector).
	reach := cascades.NewReachabilityCollector()

	plan, err := PlanPhysicalForTestWithReachability(sql, schema, nil, reach)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Guard against the reachability assertion below passing vacuously because
	// the query fell back to some unrelated degenerate plan (e.g. a clean
	// decline folded into a trivial scan) instead of the expected existential
	// fold — the same false-pass hazard the old firing counter guarded against,
	// checked here against the plan SHAPE instead of a retired arm's counter.
	explain := plan.Explain()
	if !strings.Contains(explain, "FlatMap") || !strings.Contains(explain, "FirstOrDefault") {
		t.Fatalf("N-way projected-EXISTS %q did not reach the expected FlatMap/"+
			"FirstOrDefault existential-fold shape — this test no longer covers "+
			"what it claims\nplan: %s", sql, explain)
	}

	if n := reach.Count(); n != 0 {
		t.Errorf("%d unreachable edge(s): the plan's quantifiers range over a "+
			"group that cannot produce the chain it executes, so the memo is "+
			"costing a different plan than the one that runs\n\n%s",
			n, reach.Report(5))
	}
}
