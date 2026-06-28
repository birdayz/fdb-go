package sqldriver_test

// Probes modulo (%) and integer division (/) with negative operands. Java uses
// native Java int ops (truncate toward zero; remainder takes the dividend's sign:
// -7%3=-1, -7/2=-3) — identical to Go's native operators, so the two must agree.
// A divergence here (e.g. floored division like Python) would be a real bug.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ModuloDivisionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_moddiv")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_moddiv")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE moddiv CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_moddiv/s WITH TEMPLATE moddiv")
	dsn := fmt.Sprintf("fdbsql:///testdb_moddiv?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	scalar := func(expr string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	for _, c := range []struct {
		expr string
		want int64
	}{
		{"7 % 3", 1},
		{"-7 % 3", -1},
		{"7 % -3", 1},
		{"-7 % -3", -1},
		{"8 % 4", 0},
		{"7 / 2", 3},
		{"-7 / 2", -3},
		{"7 / -2", -3},
		{"-7 / -2", 3},
		{"6 / 3", 2},
		{"-6 / 3", -2},
	} {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			if got := scalar(c.expr); got != c.want {
				t.Errorf("%s = %d, want %d (truncate toward zero / remainder sign of dividend)", c.expr, got, c.want)
			}
		})
	}

	t.Run("mod_by_zero_errors", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT 5 % 0 FROM t WHERE id = 1")
		if err == nil {
			t.Errorf("5 %% 0 unexpectedly succeeded; want a division-by-zero error")
		}
	})
}
