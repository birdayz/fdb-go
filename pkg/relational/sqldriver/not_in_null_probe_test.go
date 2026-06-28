package sqldriver_test

// Probes IN / NOT IN with a NULL list element. The classic NOT-IN-NULL 3VL trap
// never arises here because BOTH engines reject a NULL in the IN list at semantic
// analysis: Java's SemanticAnalyzer.validateInListItems throws WRONG_OBJECT_TYPE
// (42809, "NULL values are not allowed in the IN list") and Go ports that exactly
// (eval_predicate.go / logical_predicate.go / walk.go). Pins (a) correct IN/NOT IN
// over a non-NULL list and (b) the conformant 42809 rejection of a NULL element.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_NotInNullProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_notinnull")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_notinnull")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE notinnull "+
			"CREATE TABLE t (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_notinnull/s WITH TEMPLATE notinnull")
	dsn := fmt.Sprintf("fdbsql:///testdb_notinnull?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, x) VALUES (1, 1), (2, 2), (3, 3)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("in_no_null", "SELECT id FROM t WHERE x IN (1, 2)", []int64{1, 2})
	ck("not_in_no_null", "SELECT id FROM t WHERE x NOT IN (1, 2)", []int64{3})

	// A NULL list element is rejected at semantic analysis (42809) — conformant
	// with Java's SemanticAnalyzer.validateInListItems. This pre-empts the
	// NOT-IN-NULL 3VL trap entirely (the predicate never evaluates).
	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "42809") {
				t.Errorf("%s error = %v, want 42809 (NULL not allowed in IN list)", name, err)
			}
		})
	}
	rejected("in_with_null_rejected", "SELECT id FROM t WHERE x IN (1, NULL)")
	rejected("not_in_with_null_rejected", "SELECT id FROM t WHERE x NOT IN (1, NULL)")
}
