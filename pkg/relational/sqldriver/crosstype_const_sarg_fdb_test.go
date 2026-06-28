package sqldriver_test

// Regression for the cross-type int-constant vs DOUBLE-column index SARG fix:
// an INTEGER literal compared to a DOUBLE indexed column must be widened to the
// column's type so the index probe/range packs the right tuple type. Previously
// `d = 5` missed all rows and `d > 6` / `d < 8` returned wrong rows (int/double
// tuple type-codes don't interleave). Fixed in expr.ResolveComparison.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_CrossTypeConstSarg(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_xtconstsarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_xtconstsarg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE xtconstsarg "+
			"CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id)) "+
			"CREATE INDEX t_d ON t (d)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_xtconstsarg/s WITH TEMPLATE xtconstsarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_xtconstsarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// d = 5.0, 7.0, 10.0
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d) VALUES (1, 5.0), (2, 7.0), (3, 10.0)")

	ids := func(q string) []int64 {
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
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
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
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// Graefe follow-up: prove the index SARG actually FIRES with the widened
	// tuple-double comparand (not a silent degrade to full scan — the residual
	// path is correct, so a rows-only test would pass green either way).
	t.Run("uses_index_range_scan", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM t WHERE d = 5").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(T_D") {
			t.Fatalf("expected an IndexScan(T_D ...) range scan for `d = 5`, got: %s", plan)
		}
	})

	// int-literal comparands against a DOUBLE indexed column — all previously broken.
	ck("eq_int_lit", "SELECT id FROM t WHERE d = 5", []int64{1})
	// IN-list with int literals (ResolveIn path) — now widened too.
	ck("in_int_lit", "SELECT id FROM t WHERE d IN (5, 7)", []int64{1, 2})
	ck("gt_int_lit", "SELECT id FROM t WHERE d > 6", []int64{2, 3})
	ck("ge_int_lit", "SELECT id FROM t WHERE d >= 7", []int64{2, 3})
	ck("lt_int_lit", "SELECT id FROM t WHERE d < 8", []int64{1, 2})
	ck("le_int_lit", "SELECT id FROM t WHERE d <= 7", []int64{1, 2})
	ck("ne_int_lit", "SELECT id FROM t WHERE d <> 7", []int64{1, 3})
	ck("reversed_int_lit", "SELECT id FROM t WHERE 5 = d", []int64{1})
	// double-literal comparands still correct.
	ck("eq_dbl_lit", "SELECT id FROM t WHERE d = 5.0", []int64{1})
	ck("gt_dbl_lit", "SELECT id FROM t WHERE d > 6.0", []int64{2, 3})
	// BETWEEN with int bounds (desugars to >= AND <=).
	ck("between_int", "SELECT id FROM t WHERE d BETWEEN 5 AND 8", []int64{1, 2})
}
