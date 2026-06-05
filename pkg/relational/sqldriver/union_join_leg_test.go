package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// TestFDB_UnionJoinLeg is the codex P2-1/P2-2 regression (RFC-077 7.6): a
// CTE/derived-table whose body is a UNION, used as a JOIN LEG, must plan AND
// return correct rows. Retiring the opaque-merge fallback exposed two gaps in the
// anchored-RC leg-column derivation:
//   - derivedOutputColumns had no LogicalUnion case → the union-bodied CTE leg
//     derived nil columns → the join became untranslatable (it planned on master
//     via the opaque fallback);
//   - the union schema must take the FIRST branch's column NAMES (SQL standard);
//     requiring all branches to share names wrongly rejected a valid
//     `SELECT id AS x … UNION ALL SELECT v AS y …`.
//
// Both are pinned end-to-end against real FDB here (translation alone is not
// enough — the executor must union later branches by POSITION under the first
// branch's names, which is exactly what the anchored leg columns assume).
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

	// (2) Different branch aliases: the union exposes the FIRST branch's name `x`;
	// the second branch (SELECT v AS y) unions by POSITION, so u.x = {1,2,30}.
	// Join c on u.x = c.id → c.id in {1,2,30} matches {1,2} → w {100,200}.
	assertInt64Set(t, db, ctx,
		"WITH u AS (SELECT id AS x FROM a UNION ALL SELECT v AS y FROM b) "+
			"SELECT c.w FROM u, c WHERE u.x = c.id",
		[]int64{100, 200})
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
