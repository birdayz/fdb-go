package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// The N-way projected-EXISTS arm of ImplementNestedLoopJoinRule used to build a
// FlatMap whose outer child is the whole N-way chain
//
//	PredicatesFilter(NLJ(NLJ(Scan(NA), Scan(NB)), Scan(NC)), [2 preds])
//
// over a quantifier whose group could only produce `Scan(NA)` — the optimizer
// costing a single table scan for a three-way join.
//
// This pins the MEMO SHAPE rather than the rows, deliberately. The arm's plans
// do not execute at all: every query reaching it dies with "multi-leg row
// cannot serve a source-relative ordinal — (planner/executor bug)", which is
// PRE-EXISTING (reverting the memo fix reproduces it identically). A yamsql
// scenario was written, confirmed to fire the arm, and then removed, because
// pinning its rows would mean pinning an error string and promoting a defect to
// expected behaviour.
//
// So this is the only coverage the fix has, and without it nothing detects
// either the arm firing or the repair regressing — the reachability corpus
// ratchet cannot help, since no corpus query reaches this arm at all.
func TestNWayProjectedExists_OuterQuantifierMatchesExecutedPlan(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE na (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))
CREATE TABLE nb (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))
CREATE TABLE nc (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))
CREATE TABLE nd (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))`

	// A PROJECTED exists (in the select list) over >2 ForEach legs. A
	// WHERE-EXISTS does NOT reach this arm — it needs N>2 ForEach quantifiers
	// plus a trailing Existential in one flattened Select.
	const sql = `SELECT a.v, EXISTS (SELECT 1 FROM nd d WHERE d.id = a.id) AS has_d
	             FROM na a, nb b, nc c
	             WHERE a.id = b.id AND b.id = c.id`

	// The collector is OWNED BY THIS TEST and threaded into the planner, so
	// t.Parallel is safe: a tally shared across concurrent planners is not a
	// measurement (RFC-183, ReachabilityCollector).
	reach := cascades.NewReachabilityCollector()

	firedBefore := cascades.NWayOuterYieldCount()
	if _, err := PlanPhysicalForTestWithReachability(sql, schema, nil, reach); err != nil {
		t.Fatalf("plan: %v", err)
	}
	fired := cascades.NWayOuterYieldCount() - firedBefore

	// Guard against the assertion below passing because the arm never ran —
	// which is exactly how this divergence stayed invisible for so long. If a
	// planner change stops routing this shape here, that is a real signal and
	// should fail loudly rather than silently voiding the check.
	//
	// NWayOuterYieldCount is still a process-global counter, i.e. the same shape
	// as the shared-tally bug the ReachabilityCollector threading just deleted.
	// It is safe HERE, and only because of how it is used: a concurrent test
	// planning this shape could only inflate `fired`, and this guard fails on
	// ZERO. Inflation cannot produce a false pass. The assertion that actually
	// decides the test — reach.Count() — reads a collector owned by this test.
	// Do not reuse this counter for anything that fails on a HIGH value without
	// scoping it first.
	if fired == 0 {
		t.Fatalf("the N-way projected-EXISTS arm did not fire for %q — "+
			"this test no longer covers what it claims", sql)
	}

	if n := reach.Count(); n != 0 {
		t.Errorf("%d unreachable edge(s): the FlatMap's outer quantifier ranges over a "+
			"group that cannot produce the N-way chain it executes, so the memo is "+
			"costing a different plan than the one that runs\n\n%s",
			n, reach.Report(5))
	}
}
