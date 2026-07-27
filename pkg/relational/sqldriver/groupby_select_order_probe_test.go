package sqldriver_test

// CQ-5 regression — GROUP BY output follows SQL SELECT-list order. The
// aggregate's private runtime ABI remains keys-then-aggregates, while one exact
// ordinal post-aggregate projection exposes SELECT order. These tests pin the
// boundary, including duplicate labels and alias collisions: labels never
// participate in producer identity.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func TestFDB_GroupBySelectOrderProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gso")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gso")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gso "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE m (id BIGINT NOT NULL, a BIGINT, b BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gso/s WITH TEMPLATE gso")
	dsn := fmt.Sprintf("fdbsql:///testdb_gso?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// single group a=7, SUM(v)=10+20=30
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, v) VALUES (1, 7, 10), (2, 7, 20)")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO m (id, a, b, v) VALUES "+
			"(1, 1, 10, 10), (2, 1, 10, 25), (3, 2, 20, 10), (4, 3, 30, NULL)")

	assertRows := func(t *testing.T, query string, wantCols []string, wantRows [][]any) {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if !reflect.DeepEqual(cols, wantCols) {
			t.Fatalf("columns: got %v, want %v", cols, wantCols)
		}
		var got [][]any
		for rows.Next() {
			vals := make([]sql.NullInt64, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			row := make([]any, len(cols))
			for i, v := range vals {
				if v.Valid {
					row[i] = v.Int64
				}
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if !reflect.DeepEqual(got, wantRows) {
			t.Fatalf("rows for %q:\n got  %v\n want %v", query, got, wantRows)
		}
	}

	// key-first SELECT: order already matches — correct in both order and names.
	t.Run("key_first_select_correct", func(t *testing.T) {
		var c0, c1 int64
		if err := db.QueryRowContext(ctx, "SELECT a, SUM(v) FROM t GROUP BY a").Scan(&c0, &c1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c0 != 7 || c1 != 30 {
			t.Errorf("SELECT a, SUM(v) = (%d, %d), want (7, 30)", c0, c1)
		}
	})

	// Aggregate-first SELECT must still expose SELECT-list order.
	t.Run("aggregate_first", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(v), a FROM t GROUP BY a")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if !rows.Next() {
			t.Fatal("no rows")
		}
		var c0, c1 int64
		if err := rows.Scan(&c0, &c1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c0 != 30 || c1 != 7 {
			t.Errorf("SELECT order: got (%d, %d) cols=%v, want (30, 7)", c0, c1, cols)
		}
	})

	// A computed expression over an aggregate uses the same output boundary.
	t.Run("computed_aggregate_first", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(v)+1, a FROM t GROUP BY a")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if !rows.Next() {
			t.Fatal("no rows")
		}
		var c0, c1 int64
		if err := rows.Scan(&c0, &c1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c0 != 31 || c1 != 7 {
			t.Errorf("SELECT order: got (%d, %d) cols=%v, want (31, 7)", c0, c1, cols)
		}
	})

	// Explicit labels remain correct and do not influence slot identity.
	t.Run("name_based_access_is_correct", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(v) AS total, a AS grp FROM t GROUP BY a")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if !rows.Next() {
			t.Fatal("no rows")
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		byName := map[string]int64{}
		for i, c := range cols {
			if n, ok := vals[i].(int64); ok {
				byName[c] = n
			}
		}
		if byName["TOTAL"] != 30 {
			t.Errorf("TOTAL (SUM(v)) = %d, want 30 (cols=%v)", byName["TOTAL"], cols)
		}
		if byName["GRP"] != 7 {
			t.Errorf("GRP (a) = %d, want 7 (cols=%v)", byName["GRP"], cols)
		}
	})

	t.Run("multi_key_multi_aggregate_interleaving", func(t *testing.T) {
		assertRows(t,
			"SELECT b, SUM(v), a, COUNT(*) FROM m GROUP BY a, b ORDER BY a",
			[]string{"B", "SUM(V)", "A", "COUNT(*)"},
			[][]any{
				{int64(10), int64(35), int64(1), int64(2)},
				{int64(20), int64(10), int64(2), int64(1)},
				{int64(30), nil, int64(3), int64(1)},
			})
	})

	t.Run("aggregate_alias_does_not_capture_group_key", func(t *testing.T) {
		assertRows(t,
			"SELECT SUM(v) AS a, m.a FROM m GROUP BY m.a ORDER BY m.a",
			[]string{"A", "A"},
			[][]any{{int64(35), int64(1)}, {int64(10), int64(2)}, {nil, int64(3)}})
	})

	t.Run("bare_order_alias_precedes_colliding_group_key", func(t *testing.T) {
		assertRows(t,
			"SELECT SUM(v) AS a, a FROM m GROUP BY a ORDER BY a",
			[]string{"A", "A"},
			[][]any{{nil, int64(3)}, {int64(10), int64(2)}, {int64(35), int64(1)}})
	})

	t.Run("duplicate_visible_group_key_slots_share_native_identity", func(t *testing.T) {
		assertRows(t,
			"SELECT a, a, COUNT(*) FROM m GROUP BY a ORDER BY a",
			[]string{"A", "A", "COUNT(*)"},
			[][]any{
				{int64(1), int64(1), int64(2)},
				{int64(2), int64(2), int64(1)},
				{int64(3), int64(3), int64(1)},
			})
	})

	t.Run("computed_key_leaf_does_not_bind_aggregate_alias", func(t *testing.T) {
		assertRows(t,
			"SELECT SUM(v) AS a, m.a+1 AS k FROM m GROUP BY m.a ORDER BY m.a",
			[]string{"A", "K"},
			[][]any{{int64(35), int64(2)}, {int64(10), int64(3)}, {nil, int64(4)}})
		assertRows(t,
			"SELECT SUM(v) AS a, m.a+SUM(v) AS z, m.a AS g FROM m GROUP BY m.a ORDER BY m.a",
			[]string{"A", "Z", "G"},
			[][]any{
				{int64(35), int64(36), int64(1)},
				{int64(10), int64(12), int64(2)},
				{nil, nil, int64(3)},
			})
	})

	t.Run("positional_order_uses_select_slot_identity", func(t *testing.T) {
		assertRows(t,
			"SELECT SUM(v) AS a, m.a FROM m GROUP BY m.a ORDER BY 1 DESC",
			[]string{"A", "A"},
			[][]any{{int64(35), int64(1)}, {int64(10), int64(2)}, {nil, int64(3)}})
		assertRows(t,
			"SELECT SUM(v) AS a, m.a FROM m GROUP BY m.a ORDER BY 2 DESC",
			[]string{"A", "A"},
			[][]any{{nil, int64(3)}, {int64(10), int64(2)}, {int64(35), int64(1)}})
	})

	t.Run("group_key_alias_and_hidden_accumulators", func(t *testing.T) {
		assertRows(t,
			"SELECT SUM(v), a AS grp FROM m GROUP BY a HAVING COUNT(*) > 0 ORDER BY grp",
			[]string{"SUM(V)", "GRP"},
			[][]any{{int64(35), int64(1)}, {int64(10), int64(2)}, {nil, int64(3)}})
		assertRows(t,
			"SELECT SUM(v), a FROM m GROUP BY a ORDER BY COUNT(*) DESC, a",
			[]string{"SUM(V)", "A"},
			[][]any{{int64(35), int64(1)}, {int64(10), int64(2)}, {nil, int64(3)}})
	})

	t.Run("cte_and_derived_boundaries_are_positional", func(t *testing.T) {
		assertRows(t,
			"WITH d(s,g) AS (SELECT SUM(v),a FROM m GROUP BY a) SELECT s,g FROM d ORDER BY g",
			[]string{"S", "G"},
			[][]any{{int64(35), int64(1)}, {int64(10), int64(2)}, {nil, int64(3)}})
		assertRows(t,
			"SELECT d.s,d.g FROM (SELECT SUM(v) AS s,a AS g FROM m GROUP BY a) d ORDER BY d.g",
			[]string{"S", "G"},
			[][]any{{int64(35), int64(1)}, {int64(10), int64(2)}, {nil, int64(3)}})
	})

	t.Run("distinct_count_star_positional_order", func(t *testing.T) {
		assertRows(t,
			"SELECT DISTINCT COUNT(*) AS c FROM m GROUP BY a ORDER BY 1",
			[]string{"C"},
			[][]any{{int64(1)}, {int64(2)}})
	})

	t.Run("global_count_star_keeps_hidden_aggregates_private", func(t *testing.T) {
		assertRows(t,
			"SELECT COUNT(*) FROM m HAVING SUM(v) > 0",
			[]string{"COUNT(*)"},
			[][]any{{int64(4)}})
		assertRows(t,
			"SELECT COUNT(*) FROM m HAVING SUM(v) < 0",
			[]string{"COUNT(*)"},
			nil)
		assertRows(t,
			"SELECT COUNT(*) AS c FROM m HAVING SUM(v) > 0 ORDER BY SUM(v)",
			[]string{"C"},
			[][]any{{int64(4)}})
	})
}
