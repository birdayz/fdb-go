package sqldriver_test

// Probes GREATEST / LEAST (variadic max/min): integers, doubles (mixed numeric),
// and strings (lexicographic). NULL PROPAGATES — if any argument is NULL the result
// is NULL (MySQL/Oracle semantics, not PostgreSQL's skip-NULL).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_GreatestLeastProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_glp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_glp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE glp CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_glp/s WITH TEMPLATE glp")
	dsn := fmt.Sprintf("fdbsql:///testdb_glp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	str := func(expr string) sql.NullString {
		var v sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	val := func(name, expr, want string) {
		t.Run(name, func(t *testing.T) {
			got := str(expr)
			if !got.Valid || got.String != want {
				t.Errorf("%s = (valid=%v, %q), want %q", expr, got.Valid, got.String, want)
			}
		})
	}
	null := func(name, expr string) {
		t.Run(name, func(t *testing.T) {
			if got := str(expr); got.Valid {
				t.Errorf("%s = %q, want NULL (NULL propagates through GREATEST/LEAST)", expr, got.String)
			}
		})
	}

	val("greatest_int", "GREATEST(1, 5, 3)", "5")
	val("least_int", "LEAST(1, 5, 3)", "1")
	val("greatest_mixed_numeric", "GREATEST(2.5, 1, 9)", "9")
	val("greatest_string", "GREATEST('a', 'c', 'b')", "c")
	val("least_string", "LEAST('a', 'c', 'b')", "a")
	null("greatest_with_null", "GREATEST(1, NULL, 3)")
	null("least_with_null", "LEAST(1, NULL, 3)")
}
