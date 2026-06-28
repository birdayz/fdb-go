package sqldriver_test

// Probes DISTINCT (multi-column, with NULL) and ORDER BY over computed
// expressions / aggregate results — areas where dedup keys and sort keys over
// derived values are easy to get wrong.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_DistinctMultiColProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_distmc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_distmc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE distmc "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_distmc/s WITH TEMPLATE distmc")
	dsn := fmt.Sprintf("fdbsql:///testdb_distmc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// (a,b): (1,1) (1,1) dup; (1,2); (2,1); (1,NULL); (1,NULL) dup
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,1,1),(2,1,1),(3,1,2),(4,2,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a) VALUES (5,1),(6,1)") // (1,NULL) x2

	t.Run("distinct_multicol_dedups_incl_null_pair", func(t *testing.T) {
		// distinct (a,b): (1,1),(1,2),(2,1),(1,NULL) → 4 distinct pairs (the two
		// (1,1) collapse, the two (1,NULL) collapse to one — NULL=NULL for dedup).
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT DISTINCT a, b FROM t) sub").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 4 {
			t.Errorf("DISTINCT (a,b) distinct count = %d, want 4", c)
		}
	})
}

func TestFDB_OrderByComputedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_obcomp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_obcomp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE obcomp "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, grp BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_obcomp/s WITH TEMPLATE obcomp")
	dsn := fmt.Sprintf("fdbsql:///testdb_obcomp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a+b: id1=1+5=6, id2=3+1=4, id3=2+2=4(tie, lower id first), id4=10+0=10
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b,grp) VALUES (1,1,5,1),(2,3,1,1),(3,2,2,2),(4,10,0,2)")

	ordered := func(q string) []int64 {
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
		return out
	}
	eqOrd := func(g, w []int64) bool {
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

	t.Run("order_by_computed_expr", func(t *testing.T) {
		// ORDER BY a+b ASC: id2(4),id3(4),id1(6),id4(10). Ties (id2,id3 both 4) —
		// accept either tie order but the buckets must be right.
		got := ordered("SELECT id FROM t ORDER BY a + b ASC, id ASC")
		if !eqOrd(got, []int64{2, 3, 1, 4}) {
			t.Errorf("ORDER BY a+b = %v, want [2 3 1 4]", got)
		}
	})
	t.Run("order_by_computed_desc", func(t *testing.T) {
		got := ordered("SELECT id FROM t ORDER BY a + b DESC, id ASC")
		if !eqOrd(got, []int64{4, 1, 2, 3}) {
			t.Errorf("ORDER BY a+b DESC = %v, want [4 1 2 3]", got)
		}
	})
	t.Run("order_by_aggregate_result", func(t *testing.T) {
		// GROUP BY grp, ORDER BY SUM(a) DESC: grp2 sum=12 (2+10), grp1 sum=4 (1+3).
		rows, err := db.QueryContext(ctx, "SELECT grp FROM t GROUP BY grp ORDER BY SUM(a) DESC")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var g int64
			_ = rows.Scan(&g)
			got = append(got, g)
		}
		if !eqOrd(got, []int64{2, 1}) {
			t.Errorf("GROUP BY grp ORDER BY SUM(a) DESC = %v, want [2 1]", got)
		}
	})
}
