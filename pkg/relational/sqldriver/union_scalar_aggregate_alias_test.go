package sqldriver_test

import (
	"context"
	"strings"
	"testing"
)

// TestFDB_UnionScalarAggregateAlias is the RFC-080 (RFC-078 follow-up a+c) e2e
// regression: a UNION whose branches are BARE-SCALAR aggregates with mismatched
// output aliases, read downstream BY NAME, must return the aggregate values — not
// error / drop rows.
//
// RFC-078 taught the executor to remap STREAMING-aggregate union branches but the
// unionBranchNormalizable gate stayed shut for ALL LogicalAggregate branches. RFC-080
// opens it for the safe, reachable sub-shape: an UNGROUPED bare aggregate. An ungrouped
// aggregate produces NO match candidate for an aggregate index (tryAggregateIndexCandidate
// returns nil when groupingCount == 0), so it ALWAYS plans as StreamingAgg — which flows
// every aggregate under its alias (RFC-078) for any arity. (A GROUPED bare aggregate CAN
// plan as AggregateIndex, whose names are not reported; it stays gated — see
// TestFDB_UnionGroupedAggregateStillGated.)
//
// This pins, end-to-end against real FDB: (a) the load-bearing invariant — an ungrouped
// scalar aggregate does NOT plan as AggregateIndex even when an ungrouped index exists
// (still StreamingAgg), which is WHY the gate can open for ungrouped branches; and (b) that
// ungrouped single- AND multi-aggregate unions return correct rows when read by name.
// (These derived-table-in-FROM unions exercise the executor's column remap, not the
// translator gate itself — the gate is the union-as-JOIN-LEG path; its red→green is
// TestFDB_UnionJoinLeg case 3 (ungrouped, opens) and TestFDB_UnionGroupedAggregateStillGated
// (grouped, stays gated).)
func TestFDB_UnionScalarAggregateAlias(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// `withidx` even HAS an ungrouped COUNT(*) index — used to prove the index is NOT
	// engaged for a scalar aggregate (the load-bearing fact behind the gate relax).
	db := setupPlanShapeDB(t, "usaa",
		"CREATE TABLE a (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE withidx (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_withidx AS SELECT COUNT(*) FROM withidx")

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 10), (2, 20)")     // count=2, sum=30
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (3, 30)")              // count=1, sum=30
	mwjoMustExec(t, db, ctx, "INSERT INTO withidx VALUES (1, 1), (2, 2)") // count=2

	// The load-bearing fact: even WITH an ungrouped COUNT(*) index, a scalar COUNT(*)
	// plans as StreamingAgg, NOT AggregateIndex — so the AggregateIndex realization
	// (whose cursor drops the alias) cannot arise as a bare union branch. If this ever
	// flips, the gate relax must be re-examined.
	if plan := planExplainVia(t, ctx, db, "SELECT COUNT(*) AS x FROM withidx"); strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("ungrouped scalar COUNT(*) must NOT plan as AggregateIndex (gate-relax invariant), got: %s", plan)
	}

	// (1) SINGLE-aggregate bare-scalar branches, mismatched aliases, read by the first
	// branch's name → both counts. count(a)=2, count(b)=1.
	assertInt64Set(t, db, ctx,
		"SELECT u.x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u",
		[]int64{2, 1})

	// (2) MULTI-aggregate bare-scalar branches, mismatched aliases, read the first
	// column by name → both sums. Both plan as StreamingAgg (no ungrouped candidate),
	// which dual-keys every aggregate under its alias, so the position-remap normalizes
	// the second branch. sum(a)=30, sum(b)=30.
	assertInt64Set(t, db, ctx,
		"SELECT u.s FROM (SELECT SUM(v) AS s, COUNT(*) AS c FROM a UNION ALL SELECT SUM(v) AS s2, COUNT(*) AS c2 FROM b) u",
		[]int64{30, 30})
	// ...and the second column of the same multi-aggregate union resolves too.
	assertInt64Set(t, db, ctx,
		"SELECT u.c FROM (SELECT SUM(v) AS s, COUNT(*) AS c FROM a UNION ALL SELECT SUM(v) AS s2, COUNT(*) AS c2 FROM b) u",
		[]int64{2, 1})

	// (3) Same-named single-aggregate branches still work (remap is a no-op) — no
	// regression to the common case.
	assertInt64Set(t, db, ctx,
		"SELECT u.c FROM (SELECT COUNT(*) AS c FROM a UNION ALL SELECT COUNT(*) AS c FROM b) u",
		[]int64{2, 1})

	// (4) ORDER BY over the scalar-aggregate union — the sort key resolves to a real
	// value on every branch (not NULL).
	assertInt64Ordered(t, db, ctx,
		"SELECT x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u ORDER BY x",
		[]int64{1, 2})
}

// TestFDB_UnionGroupedAggregateStillGated is the RFC-080 safety boundary (Graefe): a
// UNION of bare GROUPED aggregate branches used as a JOIN LEG stays UNTRANSLATABLE (clean
// error, never wrong rows) — even though it plans as AggregateIndex. The gate
// (unionBranchNormalizable, exercised by the join-leg column-anchoring path) is
// `>= 1 aggregate AND 0 group keys`, so a grouped bare aggregate is rejected: a grouped
// aggregate CAN plan as AggregateIndex, whose cursor names outputs canonically and which
// planColumnNamesWithMD does NOT report — so the position-remap could not normalize a
// grouped branch in the join-leg anchoring. (The real RFC-078 follow-up (a): teach the
// AggregateIndex/MultiIntersection path to carry + report the output names, then open it.)
//
// NB: the gate is hit by the union-as-JOIN-LEG / CTE-body-in-join path, NOT a standalone
// derived table in FROM (which the executor handles directly). So this uses the join form.
func TestFDB_UnionGroupedAggregateStillGated(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "ugag",
		"CREATE TABLE ga (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM ga GROUP BY g "+
			"CREATE TABLE gb (id BIGINT NOT NULL, h BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_h AS SELECT COUNT(*) FROM gb GROUP BY h "+
			"CREATE TABLE c (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO ga VALUES (1, 100), (2, 100), (3, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO gb VALUES (10, 100), (20, 300)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 1), (200, 2), (300, 3)")

	// A bare (unaliased, all-visible) grouped aggregate that DOES plan as AggregateIndex
	// — the realization the gate must keep out of the join-leg union normalizer.
	if plan := planExplainVia(t, ctx, db, "SELECT g, COUNT(*) FROM ga GROUP BY g"); !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("grouped bare aggregate must plan as AggregateIndex (gate-boundary premise), got: %s", plan)
	}

	// Grouped bare aggregate UNION as a JOIN LEG (joined with c on the group key): hits the
	// column-anchoring gate, which excludes grouped → untranslatable. Must NOT silently
	// mis-resolve via an unreportable AggregateIndex branch.
	q := "WITH u AS (SELECT g, COUNT(*) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(*) FROM gb GROUP BY h) " +
		"SELECT c.w FROM u, c WHERE u.g = c.id"
	if _, err := db.QueryContext(ctx, q); err == nil {
		t.Errorf("GROUPED aggregate union JOIN LEG must stay untranslatable (gate excludes grouped), not run: %q", q)
	}
}
