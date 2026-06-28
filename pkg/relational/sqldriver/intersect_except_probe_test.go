package sqldriver_test

// Pins that INTERSECT / EXCEPT (and their ALL forms) are NOT in the grammar — they
// are rejected at parse with 42601 (syntax error), not planned. Conformant with
// Java (the RelationalParser.g4 grammar is shared and has no INTERSECT/EXCEPT). Only
// UNION ALL is supported for set operations (union_distinct_unsupported_probe).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_IntersectExceptProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_iep")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_iep")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE iep CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_iep/s WITH TEMPLATE iep")
	dsn := fmt.Sprintf("fdbsql:///testdb_iep?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "42601") {
				t.Errorf("%s error = %v, want 42601 (not in grammar)", name, err)
			}
		})
	}
	rejected("intersect", "SELECT a FROM t WHERE id <= 2 INTERSECT SELECT a FROM t WHERE id >= 2")
	rejected("except", "SELECT a FROM t EXCEPT SELECT a FROM t WHERE id = 1")
	rejected("intersect_all", "SELECT a FROM t INTERSECT ALL SELECT a FROM t")
	rejected("except_all", "SELECT a FROM t EXCEPT ALL SELECT a FROM t WHERE id = 1")
}
