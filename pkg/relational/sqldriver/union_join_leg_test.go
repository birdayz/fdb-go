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
// The mismatched-alias case is deliberately untranslatable (a clean error), NOT
// silently-wrong rows: the executor does NOT remap an aggregate branch's
// differently-aliased column to the first branch's name (a pre-existing gap), so
// anchoring there would drop rows. This pins, end-to-end against real FDB, that
// (1) the common same-named union join works, and (2) a mismatched-alias union
// join errors cleanly rather than returning wrong rows.
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

	// (2) Mismatched-alias aggregate branches: the executor cannot remap the second
	// branch's column to the first branch's name, so anchoring would DROP rows.
	// This must error cleanly (untranslatable), NOT return the wrong [200].
	// a:2 rows, b:1 row → counts {2,1}; c has id 1,2 → a correct remap would yield
	// {100 (id 1), 200 (id 2)}, but the un-remappable second branch makes it unsafe.
	q := "WITH u AS (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) " +
		"SELECT c.w FROM u, c WHERE u.x = c.id"
	if _, err := db.QueryContext(ctx, q); err == nil {
		t.Errorf("mismatched-alias aggregate union join leg must error (untranslatable), not silently drop rows: %q", q)
	}
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
