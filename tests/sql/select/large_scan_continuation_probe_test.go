package sqltest

// Probes a large result-set scan through the SQL driver, exercising internal cursor
// continuation/chaining (FDB scans are bounded per batch; the driver must chain
// continuations to return the full result). 3000 rows: full ordered scan returns
// all in order, COUNT(*) is exact, a mid-range filter returns the right slice, and
// LIMIT after a large scan truncates correctly.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_LargeScanContinuationProbe(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_lsc")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_lsc")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE lsc CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_lsc/s WITH TEMPLATE lsc")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_LSC?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	const N = 3000
	// Bound seed transactions without reducing the continuation workload.
	testkit.MwjoInsertRange(t, db, ctx, "t (id, a)", 1, N, func(id int) string {
		return fmt.Sprintf("(%d,%d)", id, id*2)
	})

	t.Run("count_exact", func(t *testing.T) {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != N {
			t.Errorf("COUNT(*) = %d, want %d", n, N)
		}
	})
	t.Run("full_ordered_scan_continuation", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id, a FROM t ORDER BY id")
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		defer rows.Close()
		expect := int64(1)
		for rows.Next() {
			var id, a int64
			if err := rows.Scan(&id, &a); err != nil {
				t.Fatalf("row scan: %v", err)
			}
			if id != expect {
				t.Fatalf("row %d: got id=%d (gap/dup/disorder in continuation)", expect, id)
			}
			if a != id*2 {
				t.Fatalf("id=%d a=%d, want %d", id, a, id*2)
			}
			expect++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if expect-1 != N {
			t.Errorf("scanned %d rows, want %d", expect-1, N)
		}
	})
	t.Run("mid_range_filter", func(t *testing.T) {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id > 1000 AND id <= 2000").Scan(&n); err != nil {
			t.Fatalf("range count: %v", err)
		}
		if n != 1000 {
			t.Errorf("range (1000,2000] count = %d, want 1000", n)
		}
	})
	t.Run("limit_after_large_scan", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY id LIMIT 5")
		if err != nil {
			t.Fatalf("limit: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if n != 5 {
			t.Errorf("LIMIT 5 returned %d", n)
		}
	})
}
