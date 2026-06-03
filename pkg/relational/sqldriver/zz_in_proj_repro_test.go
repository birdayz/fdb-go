package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_INProj_OuterProjectionOverInJoin pins RFC-070: `SELECT id FROM t
// WHERE a IN (1,7)` over a table with an index on `a` must return exactly
// column [ID], whether or not the IN drives an index InJoin.
//
// The regression: the indexed plan was `InJoin(IndexScan(IDX_A,[=]))` with
// NO outer `Project([ID])`, so it returned columns [ID, A] (and `rows.Scan(&id)`
// failed with "expected 2 destination arguments"). Root cause:
// MergeProjectionAndFetchRule's fallback dropped the projection when the
// fetch's child was an InJoin (not a directly-coverable index scan), leaking
// a bare InJoin into the projection group; and physicalProjectionWrapper's
// extraction did not relink a compound-join inner. Fixed in RFC-070 so the
// plan is `Project([ID], InJoin(IndexScan(IDX_A,[=])))`.
//
// Compares the indexed table (InJoin path) against an unindexed copy
// (PredicatesFilter scan path): both must return exactly one column [ID]
// with the same id values, and the indexed plan must actually use the InJoin
// (proving the optimization fires, not a silent full-scan fallback).
func TestFDB_INProj_OuterProjectionOverInJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inproj")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inproj")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inproj_tmpl "+
			"CREATE TABLE ti (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE tu (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON ti (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inproj/s WITH TEMPLATE inproj_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_inproj?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	for i := 1; i <= 8; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO ti VALUES (%d, %d)", i, i))
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO tu VALUES (%d, %d)", i, i))
	}

	explain := mwjoExplainer(t, db, ctx)

	// Indexed table: the IN drives an index InJoin.
	idxPlan := explain("SELECT id FROM ti WHERE a IN (1, 7)")
	t.Logf("indexed PLAN: %s", idxPlan)
	up := strings.ToUpper(idxPlan)
	if !strings.Contains(up, "INJOIN") {
		t.Errorf("expected the indexed plan to use an InJoin (optimization must fire), got: %s", idxPlan)
	}
	// The outer projection must cap the plan — a bare InJoin (the regression)
	// would have no Project and emit [ID, A].
	if !strings.HasPrefix(up, "PROJECT(") {
		t.Errorf("expected the plan to be capped by Project(...) (not a bare InJoin), got: %s", idxPlan)
	}
	assertSingleIDColumn(t, db, ctx, "ti")

	// Unindexed table: the IN is a PredicatesFilter over a full scan.
	uPlan := explain("SELECT id FROM tu WHERE a IN (1, 7)")
	t.Logf("unindexed PLAN: %s", uPlan)
	assertSingleIDColumn(t, db, ctx, "tu")

	// Related projection shapes over the same IN-driven InJoin, to guard
	// the broader projection-extraction fix (RFC-070 defect 2): a
	// multi-column projection and an expression projection must each yield
	// exactly their declared columns.
	t.Run("multi_column", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id, a FROM ti WHERE a IN (1, 7)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) != 2 {
			t.Errorf("SELECT id, a returned %d columns %v, want 2", len(cols), cols)
		}
		n := 0
		for rows.Next() {
			var id, a int64
			if err := rows.Scan(&id, &a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if id != a {
				t.Errorf("row id=%d a=%d: expected id==a", id, a)
			}
			n++
		}
		if n != 2 {
			t.Errorf("got %d rows, want 2", n)
		}
	})
	t.Run("order_by_over_in", func(t *testing.T) {
		// Project + InMemorySort + InJoin stacked: exercises the projection
		// relink (RFC-070) together with the in-memory-sort relink (RFC-069).
		rows, err := db.QueryContext(ctx, "SELECT id FROM ti WHERE a IN (3, 5, 1) ORDER BY id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) != 1 {
			t.Errorf("ORDER BY over IN returned %d columns %v, want 1", len(cols), cols)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		if fmt.Sprint(got) != fmt.Sprint([]int64{1, 3, 5}) {
			t.Errorf("got %v, want [1 3 5] (sorted)", got)
		}
	})
	t.Run("expression_projection", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id + 100 FROM ti WHERE a IN (1, 7)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) != 1 {
			t.Errorf("SELECT id+100 returned %d columns %v, want 1", len(cols), cols)
		}
		got := map[int64]bool{}
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[v] = true
		}
		if len(got) != 2 || !got[101] || !got[107] {
			t.Errorf("got %v, want {101, 107}", got)
		}
	})
}

// assertSingleIDColumn runs `SELECT id FROM <table> WHERE a IN (1,7)` and
// verifies it returns exactly one column [ID] with id values {1,7}.
func assertSingleIDColumn(t *testing.T, db *sql.DB, ctx context.Context, table string) {
	t.Helper()
	q := fmt.Sprintf("SELECT id FROM %s WHERE a IN (1, 7)", table)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: query: %v", table, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if len(cols) != 1 {
		t.Errorf("%s: SELECT id returned %d columns %v, want 1 ([ID])", table, len(cols), cols)
	}
	got := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("%s: scan: %v (the IN-projection bug)", table, err)
		}
		got[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows.Err: %v", table, err)
	}
	if len(got) != 2 || !got[1] || !got[7] {
		t.Errorf("%s: got ids %v, want {1, 7}", table, got)
	}
}
