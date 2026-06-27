package sqldriver_test

// Probes for aggregate edge cases: SUM/COUNT/MIN/MAX/AVG with NULLs and
// grouping, COUNT(*) vs COUNT(col), aggregate over an empty set (COUNT=0,
// SUM/MIN/MAX=NULL), and multiple aggregates in one query.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_AggregateEdgeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_agg_edge")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_agg_edge")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE agg_edge "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, grp STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_agg_edge/s WITH TEMPLATE agg_edge")
	dsn := fmt.Sprintf("fdbsql:///testdb_agg_edge?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// grp A: v=10,20,NULL; grp B: v=5.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, grp) VALUES (1, 10, 'A'), (2, 20, 'A'), (4, 5, 'B')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, grp) VALUES (3, 'A')")

	// scalarInt scans a single-row single-col nullable int aggregate.
	scalarInt := func(q string) sql.NullInt64 {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	t.Run("sum_ignores_null", func(t *testing.T) {
		if v := scalarInt("SELECT SUM(v) FROM t WHERE grp = 'A'"); !v.Valid || v.Int64 != 30 {
			t.Errorf("SUM(v) grp A = %v, want 30", v)
		}
	})
	t.Run("count_col_vs_star", func(t *testing.T) {
		if v := scalarInt("SELECT COUNT(v) FROM t WHERE grp = 'A'"); !v.Valid || v.Int64 != 2 {
			t.Errorf("COUNT(v) grp A = %v, want 2 (NULL ignored)", v)
		}
		if v := scalarInt("SELECT COUNT(*) FROM t WHERE grp = 'A'"); !v.Valid || v.Int64 != 3 {
			t.Errorf("COUNT(*) grp A = %v, want 3", v)
		}
	})
	t.Run("min_max_ignore_null", func(t *testing.T) {
		if v := scalarInt("SELECT MIN(v) FROM t WHERE grp = 'A'"); !v.Valid || v.Int64 != 10 {
			t.Errorf("MIN(v) grp A = %v, want 10", v)
		}
		if v := scalarInt("SELECT MAX(v) FROM t WHERE grp = 'A'"); !v.Valid || v.Int64 != 20 {
			t.Errorf("MAX(v) grp A = %v, want 20", v)
		}
	})
	t.Run("empty_set_aggregates", func(t *testing.T) {
		// No rows match → COUNT=0, SUM/MIN/MAX = NULL.
		if v := scalarInt("SELECT COUNT(*) FROM t WHERE id > 100"); !v.Valid || v.Int64 != 0 {
			t.Errorf("COUNT(*) empty = %v, want 0", v)
		}
		if v := scalarInt("SELECT SUM(v) FROM t WHERE id > 100"); v.Valid {
			t.Errorf("SUM(v) empty = %v, want NULL", v)
		}
		if v := scalarInt("SELECT MAX(v) FROM t WHERE id > 100"); v.Valid {
			t.Errorf("MAX(v) empty = %v, want NULL", v)
		}
	})
	t.Run("all_null_sum_is_null", func(t *testing.T) {
		// grp with only-NULL v: insert grp C with one NULL v.
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, grp) VALUES (5, 'C')")
		if v := scalarInt("SELECT SUM(v) FROM t WHERE grp = 'C'"); v.Valid {
			t.Errorf("SUM(v) all-NULL = %v, want NULL", v)
		}
		if v := scalarInt("SELECT COUNT(v) FROM t WHERE grp = 'C'"); !v.Valid || v.Int64 != 0 {
			t.Errorf("COUNT(v) all-NULL = %v, want 0", v)
		}
	})
	t.Run("group_by_multi_agg", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT grp, COUNT(*), SUM(v), MIN(v) FROM t WHERE grp IN ('A','B') GROUP BY grp")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		type agg struct {
			cnt, sum, min sql.NullInt64
		}
		got := map[string]agg{}
		for rows.Next() {
			var g string
			var a agg
			if err := rows.Scan(&g, &a.cnt, &a.sum, &a.min); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[g] = a
		}
		if got["A"].cnt.Int64 != 3 || got["A"].sum.Int64 != 30 || got["A"].min.Int64 != 10 {
			t.Errorf("grp A = %+v, want cnt3 sum30 min10", got["A"])
		}
		if got["B"].cnt.Int64 != 1 || got["B"].sum.Int64 != 5 || got["B"].min.Int64 != 5 {
			t.Errorf("grp B = %+v, want cnt1 sum5 min5", got["B"])
		}
	})
}
