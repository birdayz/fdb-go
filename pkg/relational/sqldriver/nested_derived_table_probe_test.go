package sqldriver_test

// Nested derived-table column resolution. An alias introduced in an INNER
// derived table is visible at any nesting depth, matching Java:
//   SELECT x FROM (SELECT a AS x FROM t) i                    (1-level alias)
//   SELECT a FROM (SELECT a FROM (SELECT a FROM t) i) s       (2-level, NO alias)
//   SELECT x FROM (SELECT x FROM (SELECT a AS x FROM t) i) s  (2-level alias)
// RFC-173 Slice 1 fixed the last case: identifier resolution keeps the OUTPUT
// column name verbatim (the source-name reverse-map is retired), so the alias
// `x` is no longer buried under the source column `a` beyond one level.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NestedDerivedTableProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ndt")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ndt")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ndt CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ndt/s WITH TEMPLATE ndt")
	dsn := fmt.Sprintf("fdbsql:///testdb_ndt?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")

	vals := func(q string) ([]int64, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o, nil
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

	t.Run("one_level_alias_works", func(t *testing.T) {
		got, err := vals("SELECT x FROM (SELECT a AS x FROM t) i")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !eq(got, []int64{10, 20, 30}) {
			t.Errorf("= %v, want [10 20 30]", got)
		}
	})
	t.Run("two_level_no_alias_works", func(t *testing.T) {
		got, err := vals("SELECT a FROM (SELECT a FROM (SELECT a FROM t) i) s")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !eq(got, []int64{10, 20, 30}) {
			t.Errorf("= %v, want [10 20 30]", got)
		}
	})
	t.Run("two_level_inner_alias", func(t *testing.T) {
		// RFC-173 Slice 1 (buried-reference precursor): an alias introduced in an
		// inner derived table now resolves through any nesting depth, because
		// identifier resolution keeps the OUTPUT column name verbatim (the
		// source-name reverse-map is retired). Previously this failed 42703 because
		// the alias name `x` was buried under the source column `a`.
		got, err := vals("SELECT x FROM (SELECT x FROM (SELECT a AS x FROM t) i) s")
		if err != nil {
			t.Fatalf("2-level inner-alias derived should resolve `x`: %v", err)
		}
		if !eq(got, []int64{10, 20, 30}) {
			t.Errorf("= %v, want [10 20 30]", got)
		}
	})
}
