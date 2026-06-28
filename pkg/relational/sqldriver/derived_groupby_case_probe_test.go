package sqldriver_test

// Batch probe: derived tables (FROM subquery), GROUP BY + HAVING, and
// CASE/COALESCE/NULLIF expressions. Unique db paths to stay parallel-safe.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func dgcOpen(t *testing.T, dbpath, tpl, ddl string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, dbpath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbpath)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+tpl+" "+ddl)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbpath+"/s WITH TEMPLATE "+tpl)
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbpath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

func dgcInts(t *testing.T, db *sql.DB, ctx context.Context, q string, sortIt bool) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if sortIt {
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	}
	return out
}

func dgcEq(g, w []int64) bool {
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

func TestFDB_DerivedTableOuterFilterProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dgcOpen(t, "/testdb_dgcderiv", "dgcderiv",
		"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,10),(2,20),(3,30),(4,40),(5,50)")

	t.Run("derived_filter_then_outer", func(t *testing.T) {
		got := dgcInts(t, db, ctx, "SELECT x FROM (SELECT id AS x FROM t WHERE v > 25) sub", true)
		if !dgcEq(got, []int64{3, 4, 5}) {
			t.Errorf("derived filter = %v, want [3 4 5]", got)
		}
	})
	t.Run("derived_then_outer_filter", func(t *testing.T) {
		got := dgcInts(t, db, ctx, "SELECT x FROM (SELECT id AS x, v AS y FROM t) sub WHERE y < 25", true)
		if !dgcEq(got, []int64{1, 2}) {
			t.Errorf("derived+outer filter = %v, want [1 2]", got)
		}
	})
	t.Run("derived_aggregate", func(t *testing.T) {
		// COUNT over a derived row set.
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT id FROM t WHERE v >= 30) sub").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 3 {
			t.Errorf("count over derived = %d, want 3", c)
		}
	})
}

func TestFDB_GroupByHavingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dgcOpen(t, "/testdb_dgcgrp", "dgcgrp",
		"CREATE TABLE t (id BIGINT NOT NULL, grp BIGINT, v BIGINT, PRIMARY KEY (id))")
	// grp 1: v 10,20 (sum 30, cnt 2); grp 2: v 30 (sum 30, cnt 1); grp 3: v 40,50,60 (sum 150, cnt 3)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,grp,v) VALUES (1,1,10),(2,1,20),(3,2,30),(4,3,40),(5,3,50),(6,3,60)")

	t.Run("having_count_gt", func(t *testing.T) {
		got := dgcInts(t, db, ctx, "SELECT grp FROM t GROUP BY grp HAVING COUNT(*) > 1", true)
		if !dgcEq(got, []int64{1, 3}) {
			t.Errorf("HAVING COUNT>1 = %v, want [1 3]", got)
		}
	})
	t.Run("having_sum_ge", func(t *testing.T) {
		got := dgcInts(t, db, ctx, "SELECT grp FROM t GROUP BY grp HAVING SUM(v) >= 100", true)
		if !dgcEq(got, []int64{3}) {
			t.Errorf("HAVING SUM>=100 = %v, want [3]", got)
		}
	})
	t.Run("having_with_where", func(t *testing.T) {
		// WHERE filters before grouping: v>15 → grp1 has only v20 (cnt1), grp2 v30 (cnt1), grp3 40,50,60 (cnt3).
		// HAVING COUNT(*)>=2 → only grp3.
		got := dgcInts(t, db, ctx, "SELECT grp FROM t WHERE v > 15 GROUP BY grp HAVING COUNT(*) >= 2", true)
		if !dgcEq(got, []int64{3}) {
			t.Errorf("WHERE+GROUP+HAVING = %v, want [3]", got)
		}
	})
}

func TestFDB_CaseCoalesceNullifProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := dgcOpen(t, "/testdb_dgccase", "dgccase",
		"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,v) VALUES (1,10),(2,20),(3,30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (4)") // v NULL

	t.Run("searched_case", func(t *testing.T) {
		// CASE WHEN v < 15 THEN 1 WHEN v < 25 THEN 2 ELSE 3 END; NULL v → ELSE → 3.
		got := dgcInts(t, db, ctx, "SELECT CASE WHEN v < 15 THEN 1 WHEN v < 25 THEN 2 ELSE 3 END FROM t", true)
		// id1→1, id2→2, id3→3, id4(NULL)→3 (NULL<15 UNKNOWN, NULL<25 UNKNOWN, ELSE)
		if !dgcEq(got, []int64{1, 2, 3, 3}) {
			t.Errorf("searched CASE = %v, want [1 2 3 3]", got)
		}
	})
	t.Run("coalesce_null", func(t *testing.T) {
		// COALESCE(v, -1): NULL → -1.
		got := dgcInts(t, db, ctx, "SELECT COALESCE(v, -1) FROM t", true)
		if !dgcEq(got, []int64{-1, 10, 20, 30}) {
			t.Errorf("COALESCE = %v, want [-1 10 20 30]", got)
		}
	})
	t.Run("nullif_equal_becomes_null", func(t *testing.T) {
		// NULLIF(v, 20): v=20 → NULL, else v. COUNT of non-null → 2 (10,30); the 20 and the already-NULL drop.
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(NULLIF(v, 20)) FROM t").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 2 {
			t.Errorf("COUNT(NULLIF(v,20)) = %d, want 2 (10,30; v=20→NULL, v=NULL stays)", c)
		}
	})
}
