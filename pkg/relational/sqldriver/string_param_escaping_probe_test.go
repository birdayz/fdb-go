package sqldriver_test

// Probes string param escaping in substituteParams (the `'`→`''` path, sibling of
// the []byte render path). Embedded single quotes, injection-looking text,
// backslashes, newlines, and unicode must round-trip EXACTLY (no SQL injection,
// no corruption) and match via a string param in WHERE.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_StringParamEscapingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_stresc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_stresc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE stresc "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_stresc/s WITH TEMPLATE stresc")
	dsn := fmt.Sprintf("fdbsql:///testdb_stresc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...any) {
		if _, e := db.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("exec %q (%v): %v", q, args, e)
		}
	}

	cases := []struct {
		id  int
		val string
	}{
		{1, "O'Brien"},                // single quote
		{2, "it''s already doubled"},  // pre-doubled quotes (must stay literal)
		{3, "'); DROP TABLE t; --"},   // injection attempt via param
		{4, "back\\slash and /slash"}, // backslashes
		{5, "line1\nline2\ttab"},      // control chars
		{6, "héllo wörld 日本語 🎉"},      // unicode incl. emoji
		{7, ""},                       // empty string
		{8, "plain"},                  // control
	}
	for _, c := range cases {
		exec("INSERT INTO t (id, s) VALUES (?, ?)", int64(c.id), c.val)
	}

	t.Run("roundtrip_exact", func(t *testing.T) {
		for _, c := range cases {
			var got string
			if err := db.QueryRowContext(ctx, "SELECT s FROM t WHERE id = ?", int64(c.id)).Scan(&got); err != nil {
				t.Fatalf("scan id=%d: %v", c.id, err)
			}
			if got != c.val {
				t.Errorf("id=%d round-trip = %q, want %q", c.id, got, c.val)
			}
		}
	})

	t.Run("no_injection_table_intact", func(t *testing.T) {
		// the injection-looking row must be stored as data; the table still has all 8 rows.
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 8 {
			t.Errorf("row count = %d, want 8 (injection param must not have run DROP)", c)
		}
	})

	t.Run("equality_via_param_with_quote", func(t *testing.T) {
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE s = ?", "O'Brien").Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != 1 {
			t.Errorf("WHERE s = 'O''Brien' param → id=%d, want 1", id)
		}
	})

	t.Run("equality_via_param_injection_text", func(t *testing.T) {
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE s = ?", "'); DROP TABLE t; --").Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != 3 {
			t.Errorf("WHERE s = injection-text param → id=%d, want 3", id)
		}
	})
}
