package sqldriver_test

// Regression: `LIMIT 0` must return ZERO rows in every query shape. Previously
// ZeroLimitRule rewrote `Limit(0, X)` to NewFullUnorderedScanExpression(nil, …),
// believing nil record-types meant an empty source — but nil means "scan ALL
// record types", i.e. a full table scan. So LIMIT 0 over any non-bare inner
// (WHERE / ORDER BY / index) returned ALL rows (the broken full-scan alternative
// won on cost; the bare case kept the correct Limit(0, Scan)). Fix: drop the
// broken Go-only rule (LIMIT is a Go extension; Java has none) so LIMIT 0 lowers
// to RecordQueryLimitPlan(0), which the executor short-circuits to 0 rows.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_LimitZero(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_limitzero")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_limitzero")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE limitzero "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_limitzero/s WITH TEMPLATE limitzero")
	dsn := fmt.Sprintf("fdbsql:///testdb_limitzero?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1, 'a'), (2, 'b'), (3, 'c')")

	count := func(q string) int {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return n
	}

	zero := map[string]string{
		"bare":             "SELECT id FROM t LIMIT 0",
		"where_alwaystrue": "SELECT id FROM t WHERE s >= '' LIMIT 0",
		"where_index":      "SELECT id FROM t WHERE s >= 'b' LIMIT 0",
		"order_by":         "SELECT id FROM t ORDER BY s LIMIT 0",
		"order_by_offset":  "SELECT id FROM t ORDER BY s LIMIT 0 OFFSET 1",
		"aggregate":        "SELECT s, COUNT(*) FROM t GROUP BY s LIMIT 0",
	}
	for name, q := range zero {
		q := q
		t.Run("zero_"+name, func(t *testing.T) {
			if got := count(q); got != 0 {
				t.Errorf("LIMIT 0 [%s] returned %d rows, want 0", name, got)
			}
		})
	}

	// Non-zero limits unaffected.
	t.Run("nonzero_two", func(t *testing.T) {
		if got := count("SELECT id FROM t ORDER BY s LIMIT 2"); got != 2 {
			t.Errorf("LIMIT 2 returned %d rows, want 2", got)
		}
	})
	t.Run("nonzero_all", func(t *testing.T) {
		if got := count("SELECT id FROM t WHERE s >= 'a' LIMIT 100"); got != 3 {
			t.Errorf("LIMIT 100 returned %d rows, want 3", got)
		}
	})
}
