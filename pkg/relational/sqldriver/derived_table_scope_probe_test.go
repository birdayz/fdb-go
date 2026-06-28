package sqldriver_test

// Probes derived-table column SCOPING: a derived table exposes only the columns in
// its SELECT list; base-table columns it does not project are hidden, and
// referencing one (in the outer SELECT, WHERE, or via the derived alias) is a clean
// 42703 — not a silent leak of the underlying row.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DerivedTableScopeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dtsc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dtsc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dtsc CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dtsc/s WITH TEMPLATE dtsc")
	dsn := fmt.Sprintf("fdbsql:///testdb_dtsc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,10,100),(2,20,200)")

	t.Run("projected_column_visible", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a FROM (SELECT a FROM t) d")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if n != 2 {
			t.Errorf("projected column visible rows = %d, want 2", n)
		}
	})
	hidden := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s error = %v, want 42703 (hidden column)", q, err)
			}
		})
	}
	hidden("select_hidden_pk", "SELECT id FROM (SELECT a FROM t) d")
	hidden("select_hidden_col", "SELECT b FROM (SELECT a FROM t) d")
	hidden("where_hidden_col", "SELECT a FROM (SELECT a FROM t) d WHERE b > 100")
	hidden("qualified_hidden_col", "SELECT d.id FROM (SELECT a FROM t) d")
}
