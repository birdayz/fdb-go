package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_UnionAggregateColumnRemap is the RFC-078 / TODO 7.6-union-remap
// regression: a UNION whose branches are STREAMING AGGREGATES with mismatched
// output aliases, read downstream BY NAME, must return all rows — not silently
// drop the non-first branch's rows (which it did, returning NULL, on master).
//
// Two pre-existing executor defects combined: (1) executeUnorderedUnion concatenated
// branch cursors with NO column normalization (unlike the ordered RecordQueryUnionPlan),
// and (2) planColumnNamesWithMD descended through a StreamingAgg to its input scan and
// returned the SCAN's columns, so even the ordered path's position-remap saw both
// branches as identical and never fired. Now planColumnNamesWithMD reports the
// aggregate's output schema (group keys + alias) and executeUnorderedUnion remaps later
// branches to the first branch's names.
func TestFDB_UnionAggregateColumnRemap(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_union_aggremap")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_union_aggremap")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE union_aggremap_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, g BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_union_aggremap/s WITH TEMPLATE union_aggremap_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_union_aggremap?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 0), (2, 0)")            // count(a) = 2
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 0), (20, 0), (30, 0)") // count(b) = 3

	// (1) Derived-table union of mismatched-alias scalar aggregates, projected by the
	// first branch's name → both counts, no NULL (the core regression).
	assertInt64Set(t, db, ctx,
		"SELECT u.x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u",
		[]int64{2, 3})

	// (2) ORDER BY the first-branch column over the same union — same correctness, and
	// the sort key must resolve to a real value on every branch (not NULL).
	assertInt64Ordered(t, db, ctx,
		"SELECT x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u ORDER BY x",
		[]int64{2, 3})

	// (3) Same-named aggregate branches still work (the remap is a no-op here) — no
	// regression to the common case.
	assertInt64Set(t, db, ctx,
		"SELECT u.c FROM (SELECT COUNT(*) AS c FROM a UNION ALL SELECT COUNT(*) AS c FROM b) u",
		[]int64{2, 3})

	// (4) Plain (non-aggregate) union still streams unchanged — no regression.
	assertInt64Set(t, db, ctx,
		"SELECT id FROM a UNION ALL SELECT id FROM b",
		[]int64{1, 2, 10, 20, 30})

	// (5) GROUP BY aggregate branches with mismatched aggregate aliases, projected by
	// the first branch's names → the grouping key + aggregate both flow on every branch.
	assertInt64Set(t, db, ctx,
		"SELECT u.cnt FROM (SELECT g, COUNT(*) AS cnt FROM a GROUP BY g UNION ALL SELECT g, COUNT(*) AS n FROM b GROUP BY g) u",
		[]int64{2, 3})
}

// assertInt64Ordered asserts the single-column int64 results equal want in EXACTLY
// that order (the query has an ORDER BY). It does NOT sort — the order is part of
// the assertion.
func assertInt64Ordered(t *testing.T, db *sql.DB, ctx context.Context, q string, want []int64) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var v int64 // a NULL (the bug) would fail the scan into int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q (a NULL here is the dropped-row bug): %v", q, err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("query %q: got %v, want %v (exact order)", q, got, want)
	}
}
