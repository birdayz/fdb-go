package sqldriver_test

// Regression: a windowed aggregate (SUM(v) OVER (PARTITION BY g)) must be REJECTED,
// not silently computed as a bare aggregate with the OVER clause dropped (which
// returned a single wrong total instead of per-partition window values).
//
// This is a deliberate fail-CLOSED DIVERGENCE from Java, not parity: Java's
// ExpressionVisitor.visitAggregateWindowedFunction silently IGNORES the OVER clause
// and computes a bare aggregate (the exact wrong-result behaviour Go used to have),
// so Java does NOT reject aggregate-OVER. Go rejecting it with 0AF00 is the
// better, allowed read-side behaviour (reject rather than return wrong rows) — there
// is no aggregate-OVER entry in the cross-engine corpus, so the harness has nothing
// to diverge on. General window functions are otherwise unsupported (TODO.md "No
// general-purpose window functions"); only the vector ROW_NUMBER QUALIFY works.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_WindowedAggregateRejected(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_winagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_winagg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE winagg CREATE TABLE t (id BIGINT NOT NULL, grp BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_winagg/s WITH TEMPLATE winagg")
	dsn := fmt.Sprintf("fdbsql:///testdb_winagg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, grp, v) VALUES (1,1,10),(2,1,30),(3,2,20)")

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				// Must reject for the RIGHT reason (0AF00 UNSUPPORTED_QUERY), not a
				// stray parse/table error — pin the SQLSTATE so a future change that
				// rejects for the wrong reason still fails.
				if !strings.Contains(err.Error(), "0AF00") {
					t.Errorf("%s error = %v, want 0AF00 (unsupported windowed aggregate)", name, err)
				}
				return
			}
			// must NOT silently succeed with a dropped OVER clause.
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var s int64
				_ = rows.Scan(&s)
				got = append(got, s)
			}
			t.Errorf("%s unexpectedly succeeded = %v; windowed aggregate must be rejected, "+
				"not computed as a bare aggregate with OVER dropped", name, got)
		})
	}
	rejected("sum_over_partition", "SELECT SUM(v) OVER (PARTITION BY grp) FROM t")
	rejected("sum_over_empty", "SELECT id, SUM(v) OVER () FROM t")
	rejected("count_over_partition", "SELECT COUNT(*) OVER (PARTITION BY grp) FROM t")
	rejected("avg_over_order", "SELECT AVG(v) OVER (ORDER BY id) FROM t")

	// a plain aggregate (no OVER) still works.
	t.Run("plain_aggregate_still_works", func(t *testing.T) {
		var s int64
		if err := db.QueryRowContext(ctx, "SELECT SUM(v) FROM t").Scan(&s); err != nil {
			t.Fatalf("plain SUM: %v", err)
		}
		if s != 60 {
			t.Errorf("plain SUM(v) = %d, want 60", s)
		}
	})
}
