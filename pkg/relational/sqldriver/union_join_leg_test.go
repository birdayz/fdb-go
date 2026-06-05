package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// TestFDB_UnionJoinLeg is the codex regression (RFC-077 7.6): a CTE/derived-table
// whose body is a UNION, used as a JOIN LEG, must derive its leg columns (the
// retired opaque fallback masked this — derivedOutputColumns had no LogicalUnion
// case, so the leg derived nil → untranslatable). It now anchors to the union's
// output schema, but ONLY when all branches agree on names — the case the
// executor's position-remap handles unambiguously.
//
// This pins, end-to-end against real FDB, that (1) the common same-named union join
// works, (2) a mismatched-alias PROJECTION union join remaps by position, and (3) a
// mismatched-alias UNGROUPED-AGGREGATE union join now returns correct rows (RFC-080 —
// the gate allows ungrouped aggregate branches because an ungrouped aggregate never
// plans as AggregateIndex (groupingCount==0 → no candidate), so it is always
// StreamingAgg, which flows every aggregate under its alias; grouped aggregate branches
// stay gated).
func TestFDB_UnionJoinLeg(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_union_join")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_union_join")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE union_join_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_union_join/s WITH TEMPLATE union_join_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_union_join?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (3, 30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (1, 100), (2, 200), (3, 300)")

	// (1) Same-named branches: u.id = {1,2,3}; join c on id → w {100,200,300}.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT id FROM a UNION ALL SELECT id FROM b) "+
			"SELECT c.w FROM u, c WHERE u.id = c.id",
		[]int64{100, 200, 300})

	// (2) Mismatched-alias PROJECTION branches: the union exposes the FIRST branch's
	// name `x`; the executor remaps the projection-topped second branch (SELECT v AS y)
	// to it by POSITION, so u.x = {1,2,30}. Join c on u.x = c.id → matches {1,2} →
	// w {100,200}. (Projection branches ARE remappable, so this must work, not error.)
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT id AS x FROM a UNION ALL SELECT v AS y FROM b) "+
			"SELECT c.w FROM u, c WHERE u.x = c.id",
		[]int64{100, 200})

	// (3) Mismatched-alias UNGROUPED-AGGREGATE branches as a JOIN LEG now return CORRECT
	// rows (RFC-080 flipped this from the prior conservative clean-error). The gate
	// (unionBranchNormalizable) now allows bare UNGROUPED aggregate branches: an ungrouped
	// aggregate produces no aggregate-index candidate (groupingCount==0 → nil), so it
	// always plans as StreamingAgg, which flows every aggregate under its alias (RFC-078),
	// so the executor's position-remap normalizes the mismatched-alias second branch.
	// (A GROUPED bare aggregate stays gated — it may plan as AggregateIndex; see
	// TestFDB_UnionScalarAggregateAlias, which pins both the ungrouped-works and the
	// grouped-stays-gated cases.) count(a)={2}, count(b)={1} → u.x={2,1}; join c on
	// u.x=c.id → w {200,100}.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) "+
			"SELECT c.w FROM u, c WHERE u.x = c.id",
		[]int64{200, 100})
}

// assertInt64Set runs q and asserts the single-column int64 results equal want as
// a set (order-independent).
func assertInt64Set(t *testing.T, db *sql.DB, ctx context.Context, q string, want []int64) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("query %q: got %v, want %v", q, got, want)
	}
}
