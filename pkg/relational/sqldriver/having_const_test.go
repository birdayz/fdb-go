package sqldriver_test

// RFC-166 regression: a constant-folded HAVING (FALSE / NULL) over a scalar
// (no-GROUP-BY) aggregate must return 0 rows, not 1. Before the fix,
// PushFilterThroughGroupByRule pushed the ConstantPredicate below the scalar
// GroupBy, and the scalar aggregate emitted one row over the (now empty) input
// -> COUNT(*)=0 surfaced as a row. Java's PredicatePushDownRule.visitGroupByExpression
// pushes nothing through a GroupBy, so the HAVING filter stays above the aggregate
// and removes the row -> 0 rows (matches Postgres).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_HavingConstantScalarAggregate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_havingconst")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_havingconst")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE havingconst "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_havingconst/s WITH TEMPLATE havingconst")
	dsn := fmt.Sprintf("fdbsql:///testdb_havingconst?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 10), (2, 20), (3, 10)")

	rowCount := func(q string) int {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n
	}
	scalar := func(q string) (int64, bool) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		if !rows.Next() {
			return 0, false
		}
		var x sql.NullInt64
		if err := rows.Scan(&x); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return x.Int64, x.Valid
	}

	t.Run("having_false_scalar_zero_rows", func(t *testing.T) {
		if n := rowCount("SELECT COUNT(*) FROM t HAVING FALSE"); n != 0 {
			t.Errorf("SELECT COUNT(*) FROM t HAVING FALSE returned %d rows, want 0", n)
		}
	})
	t.Run("having_null_scalar_zero_rows", func(t *testing.T) {
		if n := rowCount("SELECT COUNT(*) FROM t HAVING NULL"); n != 0 {
			t.Errorf("SELECT COUNT(*) FROM t HAVING NULL returned %d rows, want 0", n)
		}
	})
	t.Run("control_having_1eq0_zero_rows", func(t *testing.T) {
		// Non-field LHS makes this a ComparisonPredicate (never pushed) — was
		// already correct; pin it stays correct.
		if n := rowCount("SELECT COUNT(*) FROM t HAVING 1 = 0"); n != 0 {
			t.Errorf("HAVING 1 = 0 returned %d rows, want 0", n)
		}
	})
	t.Run("having_true_scalar_one_row", func(t *testing.T) {
		// HAVING TRUE keeps the single scalar row; COUNT(*) over all 3 rows = 3.
		if v, ok := scalar("SELECT COUNT(*) FROM t HAVING TRUE"); !ok || v != 3 {
			t.Errorf("HAVING TRUE = (%d, ok=%v), want (3, true)", v, ok)
		}
	})
	t.Run("nonscalar_having_false_zero_rows", func(t *testing.T) {
		if n := rowCount("SELECT v, COUNT(*) FROM t GROUP BY v HAVING FALSE"); n != 0 {
			t.Errorf("GROUP BY v HAVING FALSE returned %d rows, want 0", n)
		}
	})
	t.Run("grouping_key_pushdown_preserved", func(t *testing.T) {
		// Option B keeps the legitimate grouping-column pushdown: HAVING v = 10
		// selects the v=10 group (ids 1,3) -> COUNT(*)=2.
		v, ok := scalar("SELECT COUNT(*) FROM t GROUP BY v HAVING v = 10")
		if !ok || v != 2 {
			t.Errorf("GROUP BY v HAVING v = 10 -> COUNT = (%d, ok=%v), want (2, true)", v, ok)
		}
	})
}
