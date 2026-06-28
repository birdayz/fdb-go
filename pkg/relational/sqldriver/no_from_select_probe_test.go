package sqldriver_test

// Pins that a FROM-less SELECT (constant/expression with no table) is rejected
// 0AF00 — the record-layer dialect requires a record source. To evaluate a bare
// expression, select it FROM a single-row table (as the other probes do).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_NoFromSelectProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nofrom")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nofrom")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nofrom CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nofrom/s WITH TEMPLATE nofrom")
	dsn := fmt.Sprintf("fdbsql:///testdb_nofrom?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("%s error = %v, want 0AF00 (FROM-less SELECT unsupported; requires a record source)", name, err)
			}
		})
	}
	rejected("select_constant", "SELECT 1")
	rejected("select_expression", "SELECT 1 + 1")
	rejected("select_function", "SELECT UPPER('hi')")

	// the supported way to evaluate a bare expression: FROM a single-row table.
	t.Run("expression_from_singleton_table", func(t *testing.T) {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT 1 + 1 FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != 2 {
			t.Errorf("SELECT 1 + 1 FROM t = %d, want 2", v)
		}
	})
}
