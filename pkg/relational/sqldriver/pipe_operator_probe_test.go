package sqldriver_test

// Pins a real dialect gotcha: `||` is LOGICAL OR, not SQL-standard string concat.
// The grammar maps `||` into `logicalOperator` (AND | XOR | OR | '||'), so
// `TRUE || FALSE` is TRUE (a boolean), `a = 1 || b = 2` is `(a=1) OR (b=2)`, and a
// string operand is rejected 42804 ("expected boolean"). Use CONCAT() to
// concatenate. Conformant with Java (shared grammar).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_PipeOperatorProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_pipeop")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_pipeop")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE pipeop CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pipeop/s WITH TEMPLATE pipeop")
	dsn := fmt.Sprintf("fdbsql:///testdb_pipeop?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, s) VALUES (1, 1, 2, 'x')")

	t.Run("pipe_is_logical_or", func(t *testing.T) {
		var v bool
		if err := db.QueryRowContext(ctx, "SELECT TRUE || FALSE FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !v {
			t.Errorf("TRUE || FALSE = %v, want true (|| is logical OR)", v)
		}
	})
	t.Run("pipe_in_where_is_or", func(t *testing.T) {
		var n int
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE a = 1 || b = 99")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 1 {
			t.Errorf("WHERE a=1 || b=99 matched %d, want 1 ((a=1) OR (b=99))", n)
		}
	})
	t.Run("pipe_on_strings_rejected", func(t *testing.T) {
		// `s || s` is `s OR s` → string is not boolean → 42804.
		_, err := db.QueryContext(ctx, "SELECT s || s FROM t WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "42804") {
			t.Errorf("s || s error = %v, want 42804 (|| is OR, not concat — use CONCAT)", err)
		}
	})
	t.Run("concat_is_the_concat_path", func(t *testing.T) {
		var s string
		if err := db.QueryRowContext(ctx, "SELECT CONCAT(s, s) FROM t WHERE id = 1").Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s != "xx" {
			t.Errorf("CONCAT(s, s) = %q, want \"xx\"", s)
		}
	})
}
