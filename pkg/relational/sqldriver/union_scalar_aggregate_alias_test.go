package sqldriver_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestFDB_UnionScalarAggregateAlias is the RFC-080/RFC-078 end-to-end
// regression for scalar aggregate UNION branches with mismatched public
// aliases. CQ-5 now gives every SQL aggregate an exact SELECT-order Project,
// so the union consumes a stable positional public schema rather than private
// StreamingAgg/AggregateIndex names. The no-AggregateIndex assertion below
// remains a useful scalar-planning invariant; grouped and divergent-spelling
// positive coverage lives later in this file.
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
		t.Fatalf("grouped aggregate must plan as AggregateIndex (RFC-081 premise), got: %s", plan)
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

// TestFDB_UnionGroupedCountConstant pins COUNT(1) through a grouped aggregate
// UNION used as a join leg. The universal exact output Project makes each
// branch's public slots positional, so the private COUNT(*) index spelling can
// no longer diverge from the logical COUNT(1) label.
func TestFDB_UnionGroupedCountConstant(t *testing.T) {
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

	// COUNT(1) still matches the count-star index; correctness therefore proves
	// the Project boundary, rather than accidentally avoiding the divergent
	// physical realization.
	if plan := planExplainVia(t, ctx, db, "SELECT g, COUNT(1) FROM ga GROUP BY g"); !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("grouped COUNT(1) must match the count-star AggregateIndex (premise), got: %s", plan)
	}
	cc := "WITH u(k,n) AS (SELECT g, COUNT(1) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(1) FROM gb GROUP BY h) " +
		"SELECT u.k,u.n,c.w FROM u, c WHERE u.k = c.id ORDER BY u.k,u.n DESC"
	if got, want := collectRows(t, db, cc), [][]any{
		{int64(100), int64(2), int64(1)},
		{int64(100), int64(1), int64(1)},
		{int64(200), int64(1), int64(2)},
		{int64(300), int64(1), int64(3)},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("grouped COUNT(1) union rows:\n got  %v\n want %v", got, want)
	}

	// COUNT(*) grouped union JOIN LEG (no name divergence) remains normalizable → correct rows.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT g, COUNT(*) FROM ga GROUP BY g UNION ALL SELECT h, COUNT(*) FROM gb GROUP BY h) "+
			"SELECT c.w FROM u, c WHERE u.g = c.id",
		[]int64{1, 2, 1, 3})
}

// TestFDB_UnionQualifiedAggregate pins qualified aggregate operands through a
// grouped aggregate UNION join leg. SUM(GA.V) and SUM(GB.V) are public labels,
// while each exact output Project reads the native SUM(V) slot by ordinal.
func TestFDB_UnionQualifiedAggregate(t *testing.T) {
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

	qual := "WITH u(k,n) AS (SELECT g, SUM(ga.v) FROM ga GROUP BY g UNION ALL SELECT h, SUM(gb.v) FROM gb GROUP BY h) " +
		"SELECT u.k,u.n,c.w FROM u, c WHERE u.k = c.id ORDER BY u.k,u.n DESC"
	if got, want := collectRows(t, db, qual), [][]any{
		{int64(100), int64(12), int64(1)},
		{int64(100), int64(1), int64(1)},
		{int64(200), int64(9), int64(2)},
		{int64(300), int64(2), int64(3)},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("qualified aggregate union rows:\n got  %v\n want %v", got, want)
	}

	// UNQUALIFIED SUM(v): logical "SUM(V)" == physical "SUM(V)" → normalizable → correct rows.
	// u.g = {100,200} ∪ {100,300} = {100,200,100,300}; join c on u.g=c.id → w {1,2,1,3}.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT g, SUM(v) FROM ga GROUP BY g UNION ALL SELECT h, SUM(v) FROM gb GROUP BY h) "+
			"SELECT c.w FROM u, c WHERE u.g = c.id",
		[]int64{1, 2, 1, 3})
}
