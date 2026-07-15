package sqldriver_test

// End-to-end regression pins for two DISTINCT correctness defects.
//
// F7 (wrong-rows: DISTINCT wrongly eliminated over a PK EXPRESSION): the
// planner elided the dedup for SELECT DISTINCT id/3 because it credited the
// PK-covering FieldValue buried inside the arithmetic. f(pk) is not injective,
// so eliding dedup returned duplicates. id/3 over ids 1..6 must collapse to the
// 3 distinct quotients {0,1,2}; SELECT DISTINCT id (a BARE PK projection) must
// still elide legally and return all 6 rows.
//
// F8 (wrong-rows: distinctKey delimiter injection): the dedup key joined slot
// values with '|' and no escaping, so a string value spelling the inter-column
// boundary made two different (a,b) rows key identically and the second was
// dropped. Tuple packing is unambiguous, so both rows survive.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// TestFDB_DistinctOverComputedPKExpr pins F7.
func TestFDB_DistinctOverComputedPKExpr(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_distinct_pkexpr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_distinct_pkexpr")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE distinct_pkexpr "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_distinct_pkexpr/s WITH TEMPLATE distinct_pkexpr")
	dsn := fmt.Sprintf("fdbsql:///testdb_distinct_pkexpr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, v) VALUES (1,1),(2,2),(3,3),(4,4),(5,5),(6,6)")

	scanInts := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var a int64
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}

	// F7: id/3 is many-to-one over the PK, so DISTINCT must NOT be elided.
	// 1/3=0 2/3=0 3/3=1 4/3=1 5/3=1 6/3=2 -> {0,1,2}.
	got := scanInts(t, "SELECT DISTINCT id / 3 FROM t")
	want := []int64{0, 1, 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("SELECT DISTINCT id/3: got %v, want %v (dedup wrongly elided over f(pk))", got, want)
	}

	// Guard against overcorrection: a BARE PK projection is injective, so the
	// bare-PK elision stays legal and every row survives.
	gotID := scanInts(t, "SELECT DISTINCT id FROM t")
	wantID := []int64{1, 2, 3, 4, 5, 6}
	if fmt.Sprint(gotID) != fmt.Sprint(wantID) {
		t.Fatalf("SELECT DISTINCT id: got %v, want %v (bare-PK elision must stay correct)", gotID, wantID)
	}
}

// TestFDB_DistinctDelimiterInjection pins F8.
func TestFDB_DistinctDelimiterInjection(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_distinct_delim")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_distinct_delim")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE distinct_delim "+
			"CREATE TABLE t (id BIGINT NOT NULL, a STRING, b STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_distinct_delim/s WITH TEMPLATE distinct_delim")
	dsn := fmt.Sprintf("fdbsql:///testdb_distinct_delim?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Row 1: a="x",              b="y|B=string:z"
	// Row 2: a="x|B=string:y",   b="z"
	// Under the old '|'-joined key both serialize to the identical string, so
	// DISTINCT dropped the second. The two (a,b) tuples are genuinely distinct.
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, a, b) VALUES "+
			"(1, 'x', 'y|B=string:z'), (2, 'x|B=string:y', 'z')")

	rows, err := db.QueryContext(ctx, "SELECT DISTINCT a, b FROM t")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("a=%q b=%q", a, b))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 {
		t.Fatalf("SELECT DISTINCT a,b: got %d rows %v, want 2 (delimiter injection dropped a distinct row)",
			len(got), got)
	}
}
