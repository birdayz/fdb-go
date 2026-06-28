package sqldriver_test

// KNOWN-GAP sentinel — nested derived tables lose ALIAS-introduced column names
// beyond one level (TODO.md "nested derived-table alias propagation").
//
// Derived tables (subquery in FROM) are supported and cross-engine-tested (plandiff
// corpus). But an alias introduced in an INNER derived table is not visible TWO
// levels up:
//   works:  SELECT x FROM (SELECT a AS x FROM t) i              (1-level alias)
//   works:  SELECT a FROM (SELECT a FROM (SELECT a FROM t) i) s (2-level, NO alias)
//   FAILS:  SELECT x FROM (SELECT x FROM (SELECT a AS x FROM t) i) s  → 42703 "column X"
// The real column name `a` propagates through any depth; only an alias-introduced
// name is dropped at depth ≥2. Fail-CLOSED (clean 42703, not wrong rows). Standard
// SQL allows this and Java supports derived tables, so this is most likely a Go
// column-anchoring gap (derivedOutputColumns/legColumns not propagating the alias
// name through a nested derived body) — pending a Java-behavior confirmation +
// query-engine review. This test pins the current boundary; flip the failing case
// when fixed.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
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
	t.Run("two_level_inner_alias_currently_42703_BUG", func(t *testing.T) {
		_, err := vals("SELECT x FROM (SELECT x FROM (SELECT a AS x FROM t) i) s")
		// CURRENT (buggy) behavior: alias name lost two levels up → 42703.
		// When fixed, this returns [10,20,30] — flip the assertion + update TODO.
		if err == nil {
			t.Errorf("2-level inner-alias derived unexpectedly succeeded — the alias-" +
				"propagation gap may be FIXED; flip this sentinel + update TODO.md")
			return
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Errorf("err = %v, want 42703 (current alias-propagation gap)", err)
		}
	})
}
