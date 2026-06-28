package sqldriver_test

// Probes NULL placement in ORDER BY (wire-relevant: FDB tuple encodes NULL as the
// smallest element, so an ascending sort puts NULLs first and a descending sort
// puts them last). A classic bug source (NULLS FIRST vs LAST). Also covers a
// secondary sort key and NULL ordering through an index.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NullOrderingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nullord")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nullord")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nullord "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nullord/s WITH TEMPLATE nullord")
	dsn := fmt.Sprintf("fdbsql:///testdb_nullord?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: id1=10, id2=NULL, id3=20, id4=NULL, id5=5
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 10), (3, 20), (5, 5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (2), (4)")

	// returns the ordered list of `a` values, with NULL rendered as -1 sentinel
	// (a is never -1 in the data) so order is observable.
	orderedA := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var a sql.NullInt64
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a.Valid {
				out = append(out, a.Int64)
			} else {
				out = append(out, -1)
			}
		}
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	t.Run("asc_nulls_first", func(t *testing.T) {
		// NULL is the smallest tuple element → NULLs first, then 5,10,20.
		got := orderedA("SELECT a FROM t ORDER BY a ASC")
		if !eq(got, []int64{-1, -1, 5, 10, 20}) {
			t.Errorf("ORDER BY a ASC = %v, want [NULL NULL 5 10 20]", got)
		}
	})
	t.Run("desc_nulls_last", func(t *testing.T) {
		// descending reverses → 20,10,5 then NULLs last.
		got := orderedA("SELECT a FROM t ORDER BY a DESC")
		if !eq(got, []int64{20, 10, 5, -1, -1}) {
			t.Errorf("ORDER BY a DESC = %v, want [20 10 5 NULL NULL]", got)
		}
	})
	t.Run("null_ordering_with_limit", func(t *testing.T) {
		// LIMIT 2 over ASC takes the two NULLs (smallest).
		got := orderedA("SELECT a FROM t ORDER BY a ASC LIMIT 2")
		if !eq(got, []int64{-1, -1}) {
			t.Errorf("ORDER BY a ASC LIMIT 2 = %v, want [NULL NULL]", got)
		}
	})
}
