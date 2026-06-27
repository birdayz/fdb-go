package sqldriver_test

// Probes for type coercion in comparisons / arithmetic: int vs float literals
// (=, <, >), mixed int/float arithmetic, CAST, negative values, and large
// near-int64 values. Coercion bugs (precision, truncation, sign) silently
// drop/admit rows.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_TypeCoercionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_coerce")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_coerce")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE coerce "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, f DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_coerce/s WITH TEMPLATE coerce")
	dsn := fmt.Sprintf("fdbsql:///testdb_coerce?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, f) VALUES "+
		"(1, 5, 1.5), (2, 10, 2.5), (3, -3, 3.0), (4, 7, 7.0), (5, 9223372036854775807, 0.0)")

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
		for i := 1; i < len(out); i++ {
			for j := i; j > 0 && out[j-1] > out[j]; j-- {
				out[j-1], out[j] = out[j], out[j-1]
			}
		}
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
	check := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ints(q); !eqi(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	check("int_eq_float_lit", "SELECT id FROM t WHERE a = 5.0", []int64{1})
	check("int_lt_float_lit", "SELECT id FROM t WHERE a < 7.5", []int64{1, 3, 4}) // 5,-3,7 < 7.5
	check("int_gt_float_lit", "SELECT id FROM t WHERE a > 6.5", []int64{2, 4, 5}) // 10, 7, maxint
	check("int_ne_negative", "SELECT id FROM t WHERE a <> -3", []int64{1, 2, 4, 5})
	check("negative_lt", "SELECT id FROM t WHERE a < 0", []int64{3})
	check("double_col_eq", "SELECT id FROM t WHERE f = 7.0", []int64{4})
	check("double_col_range", "SELECT id FROM t WHERE f > 1.0 AND f < 3.0", []int64{1, 2})
	check("int_plus_float", "SELECT id FROM t WHERE a + 0.5 = 5.5", []int64{1})
	check("maxint_eq", "SELECT id FROM t WHERE a = 9223372036854775807", []int64{5})
	check("cast_int_to_double", "SELECT id FROM t WHERE CAST(a AS DOUBLE) = 7.0", []int64{4})
	check("mul_float", "SELECT id FROM t WHERE a * 1.5 = 7.5", []int64{1}) // 5*1.5=7.5
}
