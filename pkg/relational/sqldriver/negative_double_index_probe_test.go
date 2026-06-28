package sqldriver_test

// Probes negative-DOUBLE ordering and index range/equality over an indexed DOUBLE
// column — the FDB tuple float encoding must order negatives correctly (sign-bit
// handling), and SARGs spanning the sign boundary must return the right rows.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NegativeDoubleIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ndi")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ndi")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ndi CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id)) "+
			"CREATE INDEX t_d ON t (d)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ndi/s WITH TEMPLATE ndi")
	dsn := fmt.Sprintf("fdbsql:///testdb_ndi?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d) VALUES (1,-2.5),(2,-1.0),(3,0.0),(4,1.5),(5,2.5)")

	ds := func(q string) []float64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []float64
		for rows.Next() {
			var v float64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		return o
	}
	eq := func(g, w []float64) bool {
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
	ck := func(name, q string, want []float64) {
		t.Run(name, func(t *testing.T) {
			if got := ds(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("order_asc", "SELECT d FROM t ORDER BY d ASC", []float64{-2.5, -1, 0, 1.5, 2.5})
	ck("order_desc", "SELECT d FROM t ORDER BY d DESC", []float64{2.5, 1.5, 0, -1, -2.5})
	ck("lt_zero", "SELECT d FROM t WHERE d < 0 ORDER BY d", []float64{-2.5, -1})
	ck("range_spanning_sign", "SELECT d FROM t WHERE d > -1.5 AND d < 2.0 ORDER BY d", []float64{-1, 0, 1.5})
	ck("eq_negative", "SELECT d FROM t WHERE d = -1.0", []float64{-1})
	ck("between_spanning_sign", "SELECT d FROM t WHERE d BETWEEN -2.0 AND 1.6 ORDER BY d", []float64{-1, 0, 1.5})
}
