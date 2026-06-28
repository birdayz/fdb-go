package sqldriver_test

// Probes string trailing-whitespace comparison and length semantics. Comparison is
// byte-exact with NO blank-padding (`'a' = 'a '` is FALSE; consistent with the
// byte-ordering / case-sensitive semantics). LENGTH, CHAR_LENGTH, and
// CHARACTER_LENGTH all count CHARACTERS (runes), not bytes — LENGTH('héllo')=5,
// not 6 (unlike MySQL's byte-based LENGTH).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_StringLenPadProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_slpp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_slpp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE slpp CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_slpp/s WITH TEMPLATE slpp")
	dsn := fmt.Sprintf("fdbsql:///testdb_slpp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1, 'a '), (2, 'héllo')")

	matches := func(where string) bool {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		return rows.Next()
	}
	t.Run("trailing_ws_not_padded_constant", func(t *testing.T) {
		if matches("'a' = 'a '") {
			t.Errorf("'a' = 'a ' matched; trailing whitespace must be byte-distinct (no padding)")
		}
	})
	t.Run("trailing_ws_column_distinct", func(t *testing.T) {
		if matches("s = 'a'") { // s='a ' (with trailing space)
			t.Errorf("s = 'a' matched (s='a '); trailing whitespace is significant")
		}
		if !matches("s = 'a '") {
			t.Errorf("s = 'a ' did not match (s='a '); exact byte match should hold")
		}
	})

	intVal := func(expr string, id int) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM t WHERE id = %d", expr, id)).Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	t.Run("length_counts_chars_not_bytes", func(t *testing.T) {
		// 'héllo' = 5 chars, 6 UTF-8 bytes — all length fns return 5 (char count).
		for _, fn := range []string{"LENGTH(s)", "CHAR_LENGTH(s)", "CHARACTER_LENGTH(s)"} {
			if got := intVal(fn, 2); got != 5 {
				t.Errorf("%s on 'héllo' = %d, want 5 (characters, not bytes)", fn, got)
			}
		}
	})
	t.Run("length_counts_trailing_space", func(t *testing.T) {
		if got := intVal("LENGTH(s)", 1); got != 2 { // 'a ' = 2 chars
			t.Errorf("LENGTH('a ') = %d, want 2 (trailing space counted)", got)
		}
	})
}
