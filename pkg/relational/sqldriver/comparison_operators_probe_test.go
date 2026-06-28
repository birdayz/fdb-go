package sqldriver_test

// Probes comparison-operator surface: `!=` and `<>` are equivalent (both exclude
// NULL via 3VL); `NOT (a = 5)` also excludes NULL (NOT UNKNOWN = UNKNOWN); and the
// MySQL null-safe `<=>` operator is NOT in the grammar (42601) — the supported
// null-safe form is `IS NOT DISTINCT FROM`.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_ComparisonOperatorsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cmpops")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cmpops")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE cmpops CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cmpops/s WITH TEMPLATE cmpops")
	dsn := fmt.Sprintf("fdbsql:///testdb_cmpops?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 5), (2, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (3)") // a NULL

	ids := func(where string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("query WHERE %s: %v", where, err)
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
	ck := func(name, where string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(where); !eq(got, want) {
				t.Errorf("%s = %v, want %v", where, got, want)
			}
		})
	}

	ck("bang_eq_excludes_null", "a != 5", []int64{2})
	ck("ltgt_excludes_null", "a <> 5", []int64{2})
	ck("not_eq_excludes_null", "NOT (a = 5)", []int64{2})
	ck("is_not_distinct_from_null_safe", "a IS NOT DISTINCT FROM NULL", []int64{3})
	ck("is_distinct_from", "a IS DISTINCT FROM 5", []int64{2, 3}) // 10 and NULL are distinct from 5

	t.Run("mysql_null_safe_eq_unsupported", func(t *testing.T) {
		// `<=>` is not in the grammar — use IS NOT DISTINCT FROM.
		_, err := db.QueryContext(ctx, "SELECT id FROM t WHERE a <=> 5")
		if err == nil || !strings.Contains(err.Error(), "42601") {
			t.Errorf("a <=> 5 error = %v, want 42601 (<=> not supported; use IS NOT DISTINCT FROM)", err)
		}
	})
}
