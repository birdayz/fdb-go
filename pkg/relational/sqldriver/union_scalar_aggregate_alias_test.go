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

// TestFDB_UnionGroupedAggregate is the RFC-081 regression: a UNION of bare GROUPED
// aggregate branches with mismatched group-key names, used as a JOIN LEG, now returns
// CORRECT rows (RFC-080 left this gated as a clean error; RFC-081 opens it). The gate
// (unionBranchNormalizable, exercised by the join-leg column-anchoring path) now allows
// grouped aggregate branches because planColumnNamesWithMD reports the AggregateIndex /
// MultiIntersection output schema, so the executor's position-remap normalizes the
// mismatched-name second branch.
//
// NB: the gate is hit by the union-as-JOIN-LEG / CTE-body-in-join path, NOT a standalone
// derived table in FROM (which the executor handles directly). So this uses the join form.
func TestFDB_UnionGroupedAggregate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "ugag",
		"CREATE TABLE ga (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM ga GROUP BY g "+
			"CREATE INDEX sum_by_g AS SELECT SUM(v) FROM ga GROUP BY g "+
			"CREATE TABLE gb (id BIGINT NOT NULL, h BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_h AS SELECT COUNT(*) FROM gb GROUP BY h "+
			"CREATE INDEX sum_by_h AS SELECT SUM(v) FROM gb GROUP BY h "+
			"CREATE TABLE c (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO ga VALUES (1, 100, 5), (2, 100, 7), (3, 200, 9)")
	mwjoMustExec(t, db, ctx, "INSERT INTO gb VALUES (10, 100, 1), (20, 300, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 1), (200, 2), (300, 3)")

	// The single-aggregate grouped branch DOES plan as AggregateIndex — the realization
	// RFC-081 teaches planColumnNamesWithMD to report (the gate-open premise).
	if plan := planExplainVia(t, ctx, db, "SELECT g, COUNT(*) FROM ga GROUP BY g"); !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("grouped bare aggregate must plan as AggregateIndex (RFC-081 premise), got: %s", plan)
	}

	// (1) Grouped SINGLE-aggregate union as a JOIN LEG, mismatched group-key names (g vs h),
	// joined with c on the group key. ga groups g={100,200}; gb groups h={100,300}; the second
	// branch's h is remapped to the first branch's g → u.g={100,200,100,300}. Join c on
	// u.g=c.id → w {1,2,1,3}. (Branches plan as AggregateIndex.)
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT g, COUNT(*) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(*) FROM gb GROUP BY h) "+
			"SELECT c.w FROM u, c WHERE u.g = c.id",
		[]int64{1, 2, 1, 3})

	// (2) Grouped MULTI-aggregate union as a JOIN LEG, FILTERED on the group key so each branch
	// plans as MultiIntersection (the WHERE-equality bounds the aggregate-index scan, beating
	// the full-scan+sort StreamingAgg) — this exercises the MultiIntersection reporting arm.
	// EXPLAIN-pin that realization, then assert correct rows: both branches group=100 →
	// u.g={100,100}; join c on u.g=c.id=100 → w {1,1}.
	miQuery := "WITH u AS (SELECT g, COUNT(*), SUM(v) FROM ga WHERE g = 100 GROUP BY g " +
		"UNION ALL SELECT h, COUNT(*), SUM(v) FROM gb WHERE h = 100 GROUP BY h) " +
		"SELECT c.w FROM u, c WHERE u.g = c.id"
	if plan := planExplainVia(t, ctx, db, miQuery); !strings.Contains(plan, "MultiIntersection") {
		t.Fatalf("filtered grouped multi-aggregate branch must plan as MultiIntersection (exercises the MI arm), got: %s", plan)
	}
	assertInt64Set(t, db, ctx, miQuery, []int64{1, 1})
}

// TestFDB_UnionGroupedCountConstantGated pins the RFC-081 review P2 boundary: a GROUPED
// COUNT(<constant>) (e.g. COUNT(1)) union branch stays UNTRANSLATABLE (clean error, never
// wrong rows). COUNT(1) matches a count-star aggregate index, so its AggregateIndex
// realization reports the canonical "COUNT(*)" while the logical schema keeps "COUNT(1)";
// that name mismatch would make the union remap read a missing key → NULL count. The gate
// conservatively rejects a grouped branch with a constant aggregate operand until the
// AggregateIndex plan carries the logical output name (follow-up). COUNT(*) and COUNT(col)
// grouped branches (no name divergence) remain normalizable.
func TestFDB_UnionGroupedCountConstantGated(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "ugcc",
		"CREATE TABLE ga (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM ga GROUP BY g "+
			"CREATE TABLE gb (id BIGINT NOT NULL, h BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_h AS SELECT COUNT(*) FROM gb GROUP BY h "+
			"CREATE TABLE c (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO ga VALUES (1, 100), (2, 100), (3, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO gb VALUES (10, 100), (20, 300)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 1), (200, 2), (300, 3)")

	// COUNT(1) matches the count-star index — confirm the realization, then assert the
	// grouped COUNT(1) union JOIN LEG stays untranslatable (would otherwise NULL the count).
	if plan := planExplainVia(t, ctx, db, "SELECT g, COUNT(1) FROM ga GROUP BY g"); !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("grouped COUNT(1) must match the count-star AggregateIndex (premise), got: %s", plan)
	}
	cc := "WITH u AS (SELECT g, COUNT(1) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(1) FROM gb GROUP BY h) " +
		"SELECT u.* FROM u, c WHERE u.g = c.id"
	if _, err := db.QueryContext(ctx, cc); err == nil {
		t.Errorf("grouped COUNT(1) union JOIN LEG must stay gated (clean error), not NULL the count: %q", cc)
	}

	// COUNT(*) grouped union JOIN LEG (no name divergence) remains normalizable → correct rows.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT g, COUNT(*) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(*) FROM gb GROUP BY h) "+
			"SELECT c.w FROM u, c WHERE u.g = c.id",
		[]int64{1, 2, 1, 3})
}

// TestFDB_UnionQualifiedAggregateGated pins the RFC-081 review finding: a bare aggregate union
// branch whose aggregate name DIVERGES between the logical leg schema (aggregateOutputColumns,
// raw text e.g. SUM(GA.V)) and the physical row key (StreamingAgg/AggregateIndex canonical, e.g.
// SUM(V)) stays UNTRANSLATABLE (clean error, never wrong rows). A QUALIFIED operand is the case
// the constant-only gate missed and review flagged. An UNQUALIFIED operand (SUM(v)) is stable and
// remains normalizable. The gate decides at translation, so a join on the group key suffices to
// exercise it (no SELECT u.* needed — star expansion over aggregate unions is a separate issue).
func TestFDB_UnionQualifiedAggregateGated(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "uqag",
		"CREATE TABLE ga (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE gb (id BIGINT NOT NULL, h BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO ga VALUES (1, 100, 5), (2, 100, 7), (3, 200, 9)")
	mwjoMustExec(t, db, ctx, "INSERT INTO gb VALUES (10, 100, 1), (20, 300, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 1), (200, 2), (300, 3)")

	// QUALIFIED SUM(ga.v): logical "SUM(GA.V)" vs physical "SUM(V)" → gated (clean error).
	qual := "WITH u AS (SELECT g, SUM(ga.v) FROM ga GROUP BY g UNION ALL SELECT h, SUM(gb.v) FROM gb GROUP BY h) " +
		"SELECT c.w FROM u, c WHERE u.g = c.id"
	if _, err := db.QueryContext(ctx, qual); err == nil {
		t.Errorf("qualified-operand aggregate union join leg must be gated (name divergence), not run: %q", qual)
	}

	// UNQUALIFIED SUM(v): logical "SUM(V)" == physical "SUM(V)" → normalizable → correct rows.
	// u.g = {100,200} ∪ {100,300} = {100,200,100,300}; join c on u.g=c.id → w {1,2,1,3}.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT g, SUM(v) FROM ga GROUP BY g UNION ALL SELECT h, SUM(v) FROM gb GROUP BY h) "+
			"SELECT c.w FROM u, c WHERE u.g = c.id",
		[]int64{1, 2, 1, 3})
}
