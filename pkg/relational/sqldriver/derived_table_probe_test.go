package sqldriver_test

// Probes for derived tables in FROM (joined with base tables, nested, with an
// aggregate) and a couple of type-coercion comparisons. Derived tables stress
// the anchored-join-record / alias machinery.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_DerivedTableProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_derived")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_derived")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE derived "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, grp BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_derived/s WITH TEMPLATE derived")
	dsn := fmt.Sprintf("fdbsql:///testdb_derived?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x, grp) VALUES (1, 5, 100), (2, 10, 100), (3, 7, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2)")

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
	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return siScanRows(t, rows)
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

	t.Run("derived_filter", func(t *testing.T) {
		if got := ints("SELECT sub.v FROM (SELECT id AS v FROM a WHERE grp = 100) sub"); !eqi(got, []int64{1, 2}) {
			t.Errorf("derived filter = %v, want [1 2]", got)
		}
	})
	t.Run("derived_join_base", func(t *testing.T) {
		got := pairs("SELECT sub.v, c.id FROM (SELECT id AS v FROM a WHERE grp = 100) sub JOIN c ON c.a_id = sub.v")
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("derived JOIN base = %v, want %v", got, want)
		}
	})
	t.Run("derived_aggregate", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT g.a_id, g.cnt FROM (SELECT a_id, COUNT(*) cnt FROM c GROUP BY a_id) g")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var k, n int64
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[k] = n
		}
		if got[1] != 1 || got[2] != 1 || len(got) != 2 {
			t.Errorf("derived aggregate = %v, want {1:1, 2:1}", got)
		}
	})
	t.Run("nested_derived", func(t *testing.T) {
		if got := ints("SELECT o.y FROM (SELECT v AS y FROM (SELECT id AS v FROM a) i) o"); !eqi(got, []int64{1, 2, 3}) {
			t.Errorf("nested derived = %v, want [1 2 3]", got)
		}
	})
	t.Run("coerce_int_eq_float_literal", func(t *testing.T) {
		// int column vs float literal 5.0 — a1 (x=5).
		if got := ints("SELECT id FROM a WHERE x = 5.0"); !eqi(got, []int64{1}) {
			t.Errorf("x = 5.0 = %v, want [1]", got)
		}
	})
}
