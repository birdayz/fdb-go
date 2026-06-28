package sqldriver_test

// Pins that only UNION ALL is supported: the deduplicating plain `UNION` is rejected
// 0AF00 ("only UNION ALL is supported"), in self, two-branch, and literal-branch
// forms. UNION ALL keeps duplicates (set-bag union) — verified by row counts.
//
// CONFORMANT with Java: fdb-relational's QueryVisitor.java:351 raises the identical
// ErrorCode.UNSUPPORTED_QUERY with the exact same "only UNION ALL is supported"
// message — Go is a faithful port (same limitation, same wording, same SQLSTATE),
// not a Go-only gap.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_UnionDistinctUnsupportedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_udu")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_udu")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE udu CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_udu/s WITH TEMPLATE udu")
	dsn := fmt.Sprintf("fdbsql:///testdb_udu?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,20)")

	vals := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
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

	t.Run("union_all_keeps_duplicates", func(t *testing.T) {
		got := vals("SELECT a FROM t WHERE id <= 2 UNION ALL SELECT a FROM t WHERE id >= 2")
		// {10,20} ++ {20,20} = [10,20,20,20]
		if !eq(got, []int64{10, 20, 20, 20}) {
			t.Errorf("UNION ALL = %v, want [10 20 20 20]", got)
		}
	})
	t.Run("union_all_self_doubles", func(t *testing.T) {
		got := vals("SELECT a FROM t UNION ALL SELECT a FROM t")
		if !eq(got, []int64{10, 10, 20, 20, 20, 20}) {
			t.Errorf("UNION ALL self = %v, want [10 10 20 20 20 20]", got)
		}
	})

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("%s err = %v, want 0AF00 (only UNION ALL supported)", name, err)
			}
		})
	}
	rejected("plain_union_dedup", "SELECT a FROM t WHERE id <= 2 UNION SELECT a FROM t WHERE id >= 2")
	rejected("plain_union_self", "SELECT a FROM t UNION SELECT a FROM t")
	rejected("plain_union_literal_branch", "SELECT a FROM t WHERE id = 1 UNION SELECT 99 FROM t WHERE id = 1")
}
