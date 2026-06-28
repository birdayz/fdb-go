package sqldriver_test

// Probes boolean expressions projected in the SELECT list: comparison (a>5), logical
// combinations (AND/OR/NOT), BETWEEN, and IS NULL each produce a proper boolean
// column value.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_BooleanProjectionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_bpp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_bpp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE bpp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_bpp/s WITH TEMPLATE bpp")
	dsn := fmt.Sprintf("fdbsql:///testdb_bpp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,3,20),(2,7,5)")

	boolAt := func(expr string, id int) bool {
		var v bool
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM t WHERE id = %d", expr, id)).Scan(&v); err != nil {
			t.Fatalf("%s (id=%d): %v", expr, id, err)
		}
		return v
	}
	ck := func(name, expr string, id int, want bool) {
		t.Run(name, func(t *testing.T) {
			if got := boolAt(expr, id); got != want {
				t.Errorf("%s (id=%d) = %v, want %v", expr, id, got, want)
			}
		})
	}

	ck("cmp_false", "a > 5", 1, false) // a=3
	ck("cmp_true", "a > 5", 2, true)   // a=7
	ck("eq", "a = 7", 2, true)
	ck("and", "(a > 5) AND (b < 10)", 2, true)  // 7>5 AND 5<10
	ck("or", "(a > 5) OR (b > 10)", 1, true)    // 3>5 false OR 20>10 true
	ck("not", "NOT (a > 5)", 1, true)           // a=3
	ck("between", "a BETWEEN 1 AND 5", 1, true) // a=3
	ck("is_null_false", "a IS NULL", 1, false)  // a=3 non-null
}
