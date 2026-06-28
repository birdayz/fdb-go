package sqldriver_test

// Pins a SHARED, Java-CONFORMANT edge of the byte/string index SARG for strictly-
// greater (`col > X`). The record layer encodes an exclusive low bound as
// Strinc(pack(X)) (Java TupleRange.toRange(), TupleRange.java:485 — identical), the
// prefix-exclusion boundary rather than firstGreaterThan. Observable effect: an
// indexed `b > X'01'` skips a stored value X'0100' (a proper extension of X'01'
// whose tuple encoding `01 01 00 FF 00` sorts below Strinc's `01 01 01`), even
// though X'0100' > X'01' by value. Java does exactly the same, so this is REQUIRED
// for wire-level cross-engine consistency — NOT a Go bug, and must not be "fixed"
// into a divergence. The value-semantics result is recoverable with
// `b >= X AND b <> X` (inclusive-low SARG + residual). ORDER BY sorts X'0100'
// correctly (after X'01'); only the `>` index range boundary has this property.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_BytesGtIndexConformanceProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_bgt")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_bgt")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE bgt CREATE TABLE t (id BIGINT NOT NULL, b BYTES, PRIMARY KEY (id)) "+
			"CREATE INDEX t_b ON t (b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_bgt/s WITH TEMPLATE bgt")
	dsn := fmt.Sprintf("fdbsql:///testdb_bgt?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id1={01}, id2={01,00} (X+trailing-null), id3={01,02}, id4={02}
	for i, v := range [][]byte{{0x01}, {0x01, 0x00}, {0x01, 0x02}, {0x02}} {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, b) VALUES (?, ?)", int64(i+1), v); err != nil {
			t.Fatalf("insert %d: %v", i+1, err)
		}
	}
	ids := func(where string, arg []byte) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where+" ORDER BY b", arg)
		if err != nil {
			t.Fatalf("WHERE %s: %v", where, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			o = append(o, id)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
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

	// CONFORMANT (Java parity): `b > X'01'` skips id2={01,00} via Strinc exclusive-low.
	t.Run("gt_skips_trailing_null_extension", func(t *testing.T) {
		if got := ids("b > ?", []byte{0x01}); !eq(got, []int64{3, 4}) {
			t.Errorf("b > X'01' = %v, want [3 4] (Strinc exclusive-low skips {01,00}; "+
				"matches Java TupleRange.java:485 — if this changed, verify it still matches Java)", got)
		}
	})
	// >= is inclusive-low: includes id1 and id2.
	t.Run("gte_includes_all", func(t *testing.T) {
		if got := ids("b >= ?", []byte{0x01}); !eq(got, []int64{1, 2, 3, 4}) {
			t.Errorf("b >= X'01' = %v, want [1 2 3 4]", got)
		}
	})
	// value-semantics ">" recoverable via inclusive-low + residual not-equals.
	t.Run("workaround_gte_and_ne", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM t WHERE b >= ? AND b <> ? ORDER BY b", []byte{0x01}, []byte{0x01})
		if err != nil {
			t.Fatalf("workaround query: %v", err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			o = append(o, id)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		if !eq(o, []int64{2, 3, 4}) {
			t.Errorf("b >= X'01' AND b <> X'01' = %v, want [2 3 4] (true value-semantics > X'01')", o)
		}
	})
	// ORDER BY sorts the extension correctly (only the > range boundary is affected):
	// by b the order is {01} < {01,00} < {01,02} < {02} → ids 1,2,3,4.
	t.Run("order_by_sorts_extension_after", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY b")
		if err != nil {
			t.Fatalf("order by: %v", err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			o = append(o, id)
		}
		if !eq(o, []int64{1, 2, 3, 4}) {
			t.Errorf("ORDER BY b ids = %v, want [1 2 3 4] (by b: {01}<{01,00}<{01,02}<{02})", o)
		}
	})
}
