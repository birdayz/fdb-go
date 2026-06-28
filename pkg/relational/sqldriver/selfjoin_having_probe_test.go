package sqldriver_test

// Probes for self-joins (same table under two aliases) and HAVING (single-table
// and over a join). Self-joins stress alias disambiguation in join predicates;
// HAVING stresses post-aggregate filtering.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_SelfJoinHavingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_selfjoin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_selfjoin")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE selfjoin "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, grp BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id) CREATE INDEX a_x ON a (x)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_selfjoin/s WITH TEMPLATE selfjoin")
	dsn := fmt.Sprintf("fdbsql:///testdb_selfjoin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x, grp) VALUES (1, 5, 100), (2, 5, 100), (3, 7, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 1), (52, 2)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return siScanRows(t, rows)
	}
	ints := func(q string) []int64 {
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
	eqi := func(g, w []int64) bool {
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

	// Self-join: pairs with equal x and t1.id < t2.id → (1,2) (both x=5).
	t.Run("self_join", func(t *testing.T) {
		got := pairs("SELECT t1.id, t2.id FROM a t1 JOIN a t2 ON t1.x = t2.x AND t1.id < t2.id")
		want := []string{"1|2"}
		if !eqStrSlices(got, want) {
			t.Errorf("self-join rows = %v, want %v", got, want)
		}
	})

	// HAVING on a single-table GROUP BY: grp with >1 member → grp 100.
	t.Run("having_single", func(t *testing.T) {
		got := ints("SELECT grp FROM a GROUP BY grp HAVING COUNT(*) > 1")
		if !eqi(got, []int64{100}) {
			t.Errorf("HAVING single = %v, want [100]", got)
		}
	})

	// HAVING over a join: a.id with >=2 matching c → a1 (c50, c51).
	t.Run("having_over_join", func(t *testing.T) {
		got := ints("SELECT a.id FROM a JOIN c ON c.a_id = a.id GROUP BY a.id HAVING COUNT(c.id) >= 2")
		if !eqi(got, []int64{1}) {
			t.Errorf("HAVING over join = %v, want [1]", got)
		}
	})

	// Self-join with an aggregate: count siblings sharing x (excluding self).
	t.Run("self_join_count", func(t *testing.T) {
		got := ints("SELECT t1.id FROM a t1 JOIN a t2 ON t1.x = t2.x AND t1.id <> t2.id")
		// ids that have a same-x sibling: 1 and 2 (both x=5).
		if !eqi(got, []int64{1, 2}) {
			t.Errorf("self-join siblings = %v, want [1 2]", got)
		}
	})
}
