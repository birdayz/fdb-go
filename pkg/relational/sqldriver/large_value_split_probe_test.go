package sqldriver_test

// Probes large-value round-trip through the split-record format (records >100KB
// are split across FDB key suffixes 1+; this is wire-critical — Java must read
// back the same bytes). Inserts a ~150KB string, reads it back, and verifies it
// reassembles exactly, including via a query filter and an UPDATE to a different
// large value.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_LargeValueSplitProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_largeval")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_largeval")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE largeval "+
			"CREATE TABLE t (id BIGINT NOT NULL, tag BIGINT, payload STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_largeval/s WITH TEMPLATE largeval")
	dsn := fmt.Sprintf("fdbsql:///testdb_largeval?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ~150KB string with a recognizable repeating pattern (> the 100KB split
	// threshold → record split across suffixes 1+).
	big := strings.Repeat("abcdefghij", 15000) // 150,000 bytes
	if len(big) != 150000 {
		t.Fatalf("setup: big len = %d", len(big))
	}
	if _, e := db.ExecContext(ctx, "INSERT INTO t (id, tag, payload) VALUES (1, 100, ?)", big); e != nil {
		t.Fatalf("insert big: %v", e)
	}
	// a small row too, to ensure the split row doesn't clobber neighbors.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, tag, payload) VALUES (2, 200, 'small')")

	t.Run("roundtrip_exact_bytes", func(t *testing.T) {
		var got string
		if err := db.QueryRowContext(ctx, "SELECT payload FROM t WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got != big {
			t.Errorf("large payload round-trip mismatch: got len %d, want len %d (first-diff check)", len(got), len(big))
		}
	})
	t.Run("neighbor_intact", func(t *testing.T) {
		var got string
		if err := db.QueryRowContext(ctx, "SELECT payload FROM t WHERE id = 2").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got != "small" {
			t.Errorf("neighbor row payload = %q, want 'small' (split row clobbered it?)", got)
		}
	})
	t.Run("filter_on_other_col_returns_large", func(t *testing.T) {
		var n int
		var blobLen int
		rows, err := db.QueryContext(ctx, "SELECT payload FROM t WHERE tag = 100")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for rows.Next() {
			var b string
			_ = rows.Scan(&b)
			n++
			blobLen = len(b)
		}
		rows.Close()
		if n != 1 || blobLen != 150000 {
			t.Errorf("tag=100 → %d rows, blobLen %d, want 1 row of 150000", n, blobLen)
		}
	})
	t.Run("update_to_different_large_value", func(t *testing.T) {
		big2 := strings.Repeat("ZYXW", 40000) // 160,000 bytes, different content
		if _, e := db.ExecContext(ctx, "UPDATE t SET payload = ? WHERE id = 1", big2); e != nil {
			t.Fatalf("update big2: %v", e)
		}
		var got string
		if err := db.QueryRowContext(ctx, "SELECT payload FROM t WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got != big2 {
			t.Errorf("after UPDATE to new large value: got len %d, want %d", len(got), len(big2))
		}
	})
	t.Run("delete_large_row", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE id = 1")
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 1").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 0 {
			t.Errorf("after DELETE large row: count = %d, want 0 (split fragments left behind?)", c)
		}
		// neighbor still there
		var c2 int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&c2); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c2 != 1 {
			t.Errorf("total after deleting large row = %d, want 1 (the small neighbor)", c2)
		}
	})
}
