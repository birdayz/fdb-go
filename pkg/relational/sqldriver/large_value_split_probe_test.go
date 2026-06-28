package sqldriver_test

// Probes large-value round-trip through the SQL path, exercising the record layer's
// split-record format (values are split into 100KB chunks). A 50KB value (unsplit),
// a 150KB value (2 chunks), and a 350KB value (4 chunks) all INSERT and SELECT back
// with byte-exact content — the split on write and reassembly on read are correct
// across the 100KB boundary. Uses a non-uniform pattern so a swapped/dropped chunk
// would change the content.

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
	setup := openTestDB(t, "/testdb_lvs")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_lvs")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE lvs CREATE TABLE t (id BIGINT NOT NULL, data STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_lvs/s WITH TEMPLATE lvs")
	dsn := fmt.Sprintf("fdbsql:///testdb_lvs?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	pattern := func(n int) string {
		var b strings.Builder
		b.Grow(n)
		for i := 0; i < n; i++ {
			b.WriteByte(byte('A' + (i % 26)))
		}
		return b.String()
	}
	for _, sz := range []int{50_000, 150_000, 350_000} {
		sz := sz
		t.Run(fmt.Sprintf("roundtrip_%d", sz), func(t *testing.T) {
			want := pattern(sz)
			if _, err := db.ExecContext(ctx, "INSERT INTO t (id, data) VALUES (?, ?)", int64(sz), want); err != nil {
				t.Fatalf("insert sz=%d: %v", sz, err)
			}
			var got string
			if err := db.QueryRowContext(ctx, "SELECT data FROM t WHERE id = ?", int64(sz)).Scan(&got); err != nil {
				t.Fatalf("select sz=%d: %v", sz, err)
			}
			if len(got) != sz {
				t.Fatalf("sz=%d round-trip length = %d, want %d", sz, len(got), sz)
			}
			if got != want {
				// find the first divergence for a useful message.
				for i := 0; i < sz; i++ {
					if got[i] != want[i] {
						t.Fatalf("sz=%d content diverges at byte %d: got %q want %q", sz, i, got[i], want[i])
					}
				}
			}
		})
	}
}
