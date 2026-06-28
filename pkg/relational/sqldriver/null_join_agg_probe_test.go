package sqldriver_test

// Probes for NULL 3VL across joins + aggregates over outer joins — classic
// wrong-rows territory: NULL join keys must not match, COUNT(col) ignores NULL
// (so a null-extended LEFT-join row counts 0), COUNT(*) counts the null-extended
// row, SUM/AVG ignore NULL, and NULL WHERE comparisons exclude.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NullJoinAggProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_null_agg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_null_agg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE null_agg "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_null_agg/s WITH TEMPLATE null_agg")
	dsn := fmt.Sprintf("fdbsql:///testdb_null_agg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: (1,5), (2,NULL), (3,7); c: 50→a1, 51→a1, 52→a_id NULL.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id) VALUES (52)")

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

	t.Run("inner_join_null_key_no_match", func(t *testing.T) {
		// a2.id has no c; c52.a_id NULL matches no a.
		got := pairs("SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id")
		want := []string{"1|50", "1|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("inner join rows = %v, want %v", got, want)
		}
	})
	t.Run("left_join_null_extend", func(t *testing.T) {
		got := pairs("SELECT a.id, c.id FROM a LEFT JOIN c ON c.a_id = a.id")
		want := []string{"1|50", "1|51", "2|NULL", "3|NULL"}
		if !eqStrSlices(got, want) {
			t.Errorf("left join rows = %v, want %v", got, want)
		}
	})
	t.Run("count_col_over_left_join", func(t *testing.T) {
		// COUNT(c.id) ignores the null-extended rows → a1:2, a2:0, a3:0.
		rows, err := db.QueryContext(ctx, "SELECT a.id, COUNT(c.id) FROM a LEFT JOIN c ON c.a_id = a.id GROUP BY a.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var id, n int64
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[id] = n
		}
		if got[1] != 2 || got[2] != 0 || got[3] != 0 || len(got) != 3 {
			t.Errorf("COUNT(c.id) per a = %v, want {1:2, 2:0, 3:0}", got)
		}
	})
	t.Run("count_star_over_left_join", func(t *testing.T) {
		// COUNT(*) counts the null-extended rows: 2 + 1 + 1 = 4.
		if got := ints("SELECT COUNT(*) FROM a LEFT JOIN c ON c.a_id = a.id"); !eqi(got, []int64{4}) {
			t.Errorf("COUNT(*) over left join = %v, want [4]", got)
		}
	})
	t.Run("sum_ignores_null", func(t *testing.T) {
		if got := ints("SELECT SUM(x) FROM a"); !eqi(got, []int64{12}) {
			t.Errorf("SUM(x) = %v, want [12] (NULL ignored)", got)
		}
	})
	t.Run("where_ne_excludes_null", func(t *testing.T) {
		// x <> 5: a1(5) excluded, a2(NULL) excluded (UNKNOWN), a3(7) kept.
		if got := ints("SELECT id FROM a WHERE x <> 5"); !eqi(got, []int64{3}) {
			t.Errorf("WHERE x<>5 = %v, want [3]", got)
		}
	})
	t.Run("where_is_null", func(t *testing.T) {
		if got := ints("SELECT id FROM a WHERE x IS NULL"); !eqi(got, []int64{2}) {
			t.Errorf("WHERE x IS NULL = %v, want [2]", got)
		}
	})
}
