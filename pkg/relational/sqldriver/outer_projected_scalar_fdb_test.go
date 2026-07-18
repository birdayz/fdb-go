package sqldriver_test

// An OUTER-scoped PLAIN column projected by a correlated scalar subquery
// (`SELECT (SELECT a.id FROM q AS z WHERE z.qid = 5) FROM p AS a`) must be
// MATERIALIZED as the inner's projected output and evaluated per outer row.
// The plain spelling has no expression context, so it bypassed the walked
// arm's scope classification and derived the seed key from TEXT — the seed's
// ofOrdinal(inner, 0) then served the INNER row's first column as the scalar
// (silent wrong rows: every row read 5, the inner qid). The parenthesized
// twin `(a.id)` was classified correctly all along. Pinned on the
// single-source outer, the multi-leg cluster outer, and the bare-column
// spelling (v exists only on the outer).

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func TestFDB_OuterProjectedScalarSubquery(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_opss")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_opss")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE opss CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
			" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_opss/s WITH TEMPLATE opss")
	dsn := fmt.Sprintf("fdbsql:///testdb_opss?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO q VALUES (5), (7), (9)")

	multiset := func(t *testing.T, q string, want map[int64]int) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("must ANSWER: %v\n  sql: %s", err, q)
		}
		defer rows.Close()
		got := map[int64]int{}
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !v.Valid {
				t.Fatalf("NULL scalar — the outer projection silently missed\n  sql: %s", q)
			}
			got[v.Int64]++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scalar multiset = %v, want %v (the inner row's column served instead of the outer projection?)\n  sql: %s", got, want, q)
		}
	}

	// Single-source outer, qualified outer column: scalar = each p row's id.
	t.Run("single_source_qualified", func(t *testing.T) {
		multiset(t, "SELECT (SELECT a.id FROM q AS z WHERE z.qid = 5) FROM p AS a",
			map[int64]int{1: 1, 2: 1})
	})
	// Single-source outer, BARE outer column (v exists only on p — resolves
	// through the scope's outer fallthrough, never as an inner row key).
	t.Run("single_source_bare", func(t *testing.T) {
		multiset(t, "SELECT (SELECT a.v FROM q AS z WHERE z.qid = 5) FROM p AS a",
			map[int64]int{10: 1, 20: 1})
	})
	// Multi-leg cluster outer: the materialized outer reference is pull-up
	// baked onto the level-2 concat. p(2) × q(3) = 6 rows.
	t.Run("cluster_outer_qualified", func(t *testing.T) {
		multiset(t, "SELECT (SELECT a.id FROM q AS z WHERE z.qid = 5) FROM p AS a, q AS b",
			map[int64]int{1: 3, 2: 3})
	})
	// Inner-computed control: stays on the uncorrelated pre-eval path.
	t.Run("inner_computed_control", func(t *testing.T) {
		multiset(t, "SELECT (SELECT z.qid + 100 FROM q AS z WHERE z.qid = 5) FROM p AS a",
			map[int64]int{105: 2})
	})
	// WHERE-correlated + outer-projected: the FlatMap inner carries the
	// materialized Project([A.ID]) — id=1 finds its q row (qid never equals
	// an id here, so use qid = a.id + 4: 5 for id=1, 6 (absent) for id=2 →
	// NULL). Asserted via nullable scan.
	t.Run("where_correlated_outer_projected", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT (SELECT a.id FROM q AS z WHERE z.qid = a.id + 4) FROM p AS a")
		if err != nil {
			t.Fatalf("must ANSWER: %v", err)
		}
		defer rows.Close()
		got := map[int64]int{}
		nulls := 0
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !v.Valid {
				nulls++
				continue
			}
			got[v.Int64]++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if !reflect.DeepEqual(got, map[int64]int{1: 1}) || nulls != 1 {
			t.Errorf("= %v (+%d NULLs), want map[1:1] (+1 NULL)", got, nulls)
		}
	})
}
