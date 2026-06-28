package sqldriver_test

// Probes for CASE in contexts beyond the binary-join pushdown: a 3-way join with
// a cross-table CASE in the 3rd ON, CASE in GROUP BY, and CASE inside an
// aggregate over a join. These exercise CASE correlation detection in the
// multi-way partition / aggregate paths.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_CaseContextsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_case_ctx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_case_ctx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE case_ctx "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_case_ctx/s WITH TEMPLATE case_ctx")
	dsn := fmt.Sprintf("fdbsql:///testdb_case_ctx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10), (3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (100, 1), (200, 2), (300, 3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id, w) VALUES (50, 1, 5), (51, 2, 99), (52, 3, 7)")

	scanInts := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v.Int64)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eqInts := func(g, w []int64) bool {
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

	// 3-way join, cross-table CASE in the 3rd ON correlated to a (outer leg).
	// CASE WHEN a.x>6 THEN c.w ELSE 5 END = c.w: a1(5)→5=5; a2(10)→99=99;
	// a3(7)→7=7. All match → [1,2,3]. A mis-pushed CASE (a.x unbound) would drop
	// or mis-filter.
	t.Run("threeway_case_in_3rd_on", func(t *testing.T) {
		got := scanInts("SELECT a.id FROM a JOIN b ON b.a_id = a.id " +
			"JOIN c ON c.a_id = a.id AND CASE WHEN a.x > 6 THEN c.w ELSE 5 END = c.w")
		if !eqInts(got, []int64{1, 2, 3}) {
			t.Errorf("3-way CASE-in-3rd-ON = %v, want [1 2 3]", got)
		}
	})

	// CASE in GROUP BY: bucket x>6 → 1 else 0. a1(5)→0, a2(10)→1, a3(7)→1.
	t.Run("group_by_case", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT CASE WHEN x > 6 THEN 1 ELSE 0 END AS bucket, COUNT(*) FROM a GROUP BY CASE WHEN x > 6 THEN 1 ELSE 0 END")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var b, n int64
			if err := rows.Scan(&b, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[b] = n
		}
		if got[0] != 1 || got[1] != 2 || len(got) != 2 {
			t.Errorf("GROUP BY CASE = %v, want {0:1, 1:2}", got)
		}
	})

	// CASE inside an aggregate over a join: SUM(CASE WHEN c.w>6 THEN 1 ELSE 0).
	// c50(5)→0, c51(99)→1, c52(7)→1 → SUM=2.
	t.Run("sum_case_over_join", func(t *testing.T) {
		got := scanInts("SELECT SUM(CASE WHEN c.w > 6 THEN 1 ELSE 0 END) FROM a JOIN c ON c.a_id = a.id")
		if !eqInts(got, []int64{2}) {
			t.Errorf("SUM(CASE) over join = %v, want [2]", got)
		}
	})
}
