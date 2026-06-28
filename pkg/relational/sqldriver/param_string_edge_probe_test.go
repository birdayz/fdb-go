package sqldriver_test

// Probes parameter-rendering edge cases beyond param_rendering_probe: MinInt64
// boundary, and string params containing backslash (NOT a SQL escape char),
// newlines, unicode, embedded single quotes (doubled), and the empty string —
// each must round-trip byte-exact (and no SQL injection).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
)

func TestFDB_ParamStringEdgeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_paramedge")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_paramedge")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE paramedge CREATE TABLE t (id BIGINT NOT NULL, n BIGINT, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_paramedge/s WITH TEMPLATE paramedge")
	dsn := fmt.Sprintf("fdbsql:///testdb_paramedge?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Run("min_int64", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, n) VALUES (1, ?)", int64(math.MinInt64)); err != nil {
			t.Fatalf("insert MinInt64: %v", err)
		}
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT n FROM t WHERE id = 1").Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != math.MinInt64 {
			t.Errorf("MinInt64 round-trip = %d, want %d", n, int64(math.MinInt64))
		}
	})

	strCases := []struct{ name, val string }{
		{"backslash", `a\b\c`},                     // backslash is literal in SQL (not an escape)
		{"backslash_end", `trailing\`},             // trailing backslash must not escape the closing quote
		{"newline", "line1\nline2"},                // embedded newline
		{"tab", "a\tb"},                            // embedded tab
		{"unicode", "héllo → 世界 🎉"},                // multibyte UTF-8
		{"embedded_quote", "O'Brien's"},            // single quotes (doubled by the renderer)
		{"two_quotes", "''"},                       // just quotes
		{"empty", ""},                              // empty string (distinct from NULL)
		{"injection_try", "x'); DROP TABLE t; --"}, // must be stored literally, not executed
	}
	for i, c := range strCases {
		i, c := i, c
		id := 10 + i
		t.Run("str_"+c.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, "INSERT INTO t (id, s) VALUES (?, ?)", int64(id), c.val); err != nil {
				t.Fatalf("insert %q: %v", c.val, err)
			}
			var got string
			if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT s FROM t WHERE id = %d", id)).Scan(&got); err != nil {
				t.Fatalf("scan %q: %v", c.val, err)
			}
			if got != c.val {
				t.Errorf("string param %q round-trip = %q", c.val, got)
			}
		})
	}

	t.Run("injection_did_not_drop_table", func(t *testing.T) {
		var cnt int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&cnt); err != nil {
			t.Fatalf("table gone? %v", err)
		}
		if cnt == 0 {
			t.Errorf("table empty/dropped after injection attempt")
		}
	})
}
