package sqldriver_test

// Probes string comparison/ordering semantics (wire-relevant: FDB tuple encodes
// strings as UTF-8 bytes, so ORDER BY and range comparisons are BYTE/codepoint
// order — uppercase ('A'=65) sorts before lowercase ('a'=97), not locale-aware or
// case-insensitive). A locale/case-folding divergence would break index order.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_StringOrderingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_strord")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_strord")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE strord "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_strord/s WITH TEMPLATE strord")
	dsn := fmt.Sprintf("fdbsql:///testdb_strord?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 'Apple'(A=65) 'Banana'(B=66) 'apple'(a=97) 'cherry'(c=99) '' (empty)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1, 'apple'), (2, 'Banana'), (3, 'cherry'), (4, 'Apple'), (5, '')")

	ordered := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
	eq := func(g, w []string) bool {
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

	t.Run("order_by_byte_order", func(t *testing.T) {
		// byte order: "" < "Apple" < "Banana" < "apple" < "cherry".
		got := ordered("SELECT s FROM t ORDER BY s")
		want := []string{"", "Apple", "Banana", "apple", "cherry"}
		if !eq(got, want) {
			t.Errorf("ORDER BY s = %v, want %v (byte order: upper before lower)", got, want)
		}
	})
	t.Run("range_uppercase_before_lowercase", func(t *testing.T) {
		// s > 'B' (byte 66): "Banana"(66...) > "B"? "Banana">"B" yes; "apple"(97)>'B' yes;
		// "cherry"(99)>'B' yes; "Apple"(65...) > 'B'(66)? 'A'(65)<'B'(66) → no.
		got := ordered("SELECT s FROM t WHERE s > 'B' ORDER BY s")
		want := []string{"Banana", "apple", "cherry"}
		if !eq(got, want) {
			t.Errorf("WHERE s > 'B' = %v, want %v", got, want)
		}
	})
	t.Run("equality_case_sensitive", func(t *testing.T) {
		// 'apple' and 'Apple' are DISTINCT (case-sensitive).
		got := ordered("SELECT s FROM t WHERE s = 'apple'")
		if !eq(got, []string{"apple"}) {
			t.Errorf("WHERE s = 'apple' = %v, want [apple] (case-sensitive; 'Apple' excluded)", got)
		}
	})
	t.Run("empty_string_sorts_first", func(t *testing.T) {
		// empty string is the smallest non-NULL string.
		got := ordered("SELECT s FROM t WHERE s < 'A' ORDER BY s")
		if !eq(got, []string{""}) {
			t.Errorf("WHERE s < 'A' = %v, want [\"\"] (empty string smallest)", got)
		}
	})
}
