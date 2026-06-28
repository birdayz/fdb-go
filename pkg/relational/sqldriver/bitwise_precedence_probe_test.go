package sqldriver_test

// Completes the operator-precedence model with the bitwise class. Bitwise
// operators (& | ^) are flat among themselves (left-to-right, like arithmetic and
// logical): `6 & 3 | 1` = (6&3)|1 = 3, NOT the C-standard 6&(3|1)=2. But the
// bitwise class binds TIGHTER than arithmetic (separate, earlier grammar
// alternative): `6 + 1 & 3` = 6 + (1&3) = 7, NOT (6+1)&3 = 3. So the full level
// order is: bitwise < arithmetic < comparison < logical (tightest→loosest), each
// flat within its class. Conformant with Java (shared grammar).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_BitwisePrecedenceProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_bwp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_bwp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE bwp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_bwp/s WITH TEMPLATE bwp")
	dsn := fmt.Sprintf("fdbsql:///testdb_bwp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 6)") // 6 = 0b110

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
		{"6 & 3", 2},     // 110 & 011 = 010
		{"6 | 1", 7},     // 110 | 001 = 111
		{"6 ^ 3", 5},     // 110 ^ 011 = 101
		{"6 & 3 | 1", 3}, // flat: (6&3)|1 = 2|1 = 3
		{"6 | 1 & 3", 3}, // flat: (6|1)&3 = 7&3 = 3
		{"a & 3 | 1", 3}, // column, flat
		{"6 + 1 & 3", 7}, // bitwise tighter than +: 6 + (1&3) = 6+1 = 7
	} {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			if got := scalar(c.expr); got != c.want {
				t.Errorf("%s = %d, want %d", c.expr, got, c.want)
			}
		})
	}

	// bitwise binds tighter than comparison: `a & 3 = 2` → (a&3)=2 → 2=2 → match.
	t.Run("bitwise_tighter_than_comparison", func(t *testing.T) {
		var n int
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE a & 3 = 2")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 1 {
			t.Errorf("WHERE a & 3 = 2 matched %d, want 1 ((a&3)=2 → 2=2)", n)
		}
	})
}
