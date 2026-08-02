package sqldriver_test

// DUPLICATE GROUP BY expressions reject 42702 — Java parity.
//
// Java's rejection point is Expressions.pullUp (Expressions.java:112),
// reached from LogicalOperator.generateGroupBy (LogicalOperator.java:454):
// the grouping expressions are pulled up over the GroupByExpression's
// result value, and a grouping value containing the same expression twice
// maps it to TWO output columns — AMBIGUOUS_COLUMN, before planning ever
// starts. The identity is the RESOLVED value, so `GROUP BY category,
// T.category` (one source) rejects exactly like the bare duplicate, while
// same-named keys of DIFFERENT sources stay legal.
//
// Before the fix Go deterministically addressed the first duplicate slot
// and returned rows for every one of these shapes (the tolerance was even
// documented in buildAggregateOutputSlots). Live-Java measurement is
// pinned in conformance's DuplicateGroupByJavaProbe; the upstream corpus
// witnesses the same class through bitmap-aggregate-index.yamsql's
// negatives (blocked on AS-SELECT index DDL, PR #577).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_DuplicateGroupBy(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dupgroupby")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dupgroupby")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dupgroupby "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, category STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, category STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dupgroupby/s WITH TEMPLATE dupgroupby")
	dsn := fmt.Sprintf("fdbsql:///testdb_dupgroupby?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 VALUES (7, 'a')")

	t.Run("duplicates reject 42702", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{
			`SELECT category, COUNT(*) FROM t1 GROUP BY category, category`,
			`SELECT COUNT(*) FROM t1 GROUP BY category, category`,
			`SELECT category FROM t1 GROUP BY category, category`,
			`SELECT category, COUNT(*) FROM t1 GROUP BY category, category, category`,
			// The qualifier folds away on a single source — same resolved
			// value, same 42702 (measured on live Java).
			`SELECT category, COUNT(*) FROM t1 GROUP BY category, t1.category`,
			// Non-adjacent duplicate.
			`SELECT category, amount, COUNT(*) FROM t1 GROUP BY category, amount, category`,
			// EXPRESSION keys compare by RESOLVED VALUE, not source text —
			// the whitespace-variant spelling is the load-bearing shape: a
			// raw-text identity catches only the byte-identical twin
			// (measured live: Java 42702s both spellings).
			`SELECT amount+1, COUNT(*) FROM t1 GROUP BY amount+1, amount+1`,
			`SELECT amount+1, COUNT(*) FROM t1 GROUP BY amount+1, amount + 1`,
		} {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Errorf("%s: expected 42702, succeeded", q)
				continue
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeAmbiguousColumn {
				t.Errorf("%s: expected 42702 AMBIGUOUS_COLUMN, got %v", q, err)
			}
		}
	})

	t.Run("distinct expressions still group", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx,
			`SELECT COUNT(*) FROM t1 GROUP BY amount+1, amount+2`)
		if err != nil {
			t.Fatalf("distinct expressions: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 3 {
			t.Fatalf("distinct expressions: got %d groups, want 3", n)
		}
	})

	t.Run("distinct keys still group", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx,
			`SELECT category, amount, COUNT(*) FROM t1 GROUP BY category, amount`)
		if err != nil {
			t.Fatalf("distinct keys: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 3 {
			t.Fatalf("distinct keys: got %d groups, want 3", n)
		}
	})

	t.Run("same-named keys of different sources are legal", func(t *testing.T) {
		t.Parallel()
		// Java's identity is the resolved VALUE: t1.category and
		// t2.category are different columns, so grouping by both is legal.
		rows, err := db.QueryContext(ctx,
			`SELECT t1.category, t2.category, COUNT(*) FROM t1, t2 GROUP BY t1.category, t2.category`)
		if err != nil {
			t.Fatalf("cross-source same-named keys: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 2 {
			t.Fatalf("cross-source keys: got %d groups, want 2 (a×a, b×a)", n)
		}
	})
}
