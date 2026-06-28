package sqldriver_test

// Probes LIMIT/OFFSET pagination: successive pages tile the ordered result with no
// gaps or overlaps, the last page may be partial, OFFSET past the end yields empty,
// and OFFSET without LIMIT skips the prefix. Also LIMIT larger than the remainder.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_LimitOffsetPagingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_paging")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_paging")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE paging CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_paging/s WITH TEMPLATE paging")
	dsn := fmt.Sprintf("fdbsql:///testdb_paging?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for i := 1; i <= 10; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", i))
	}

	page := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		return out
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
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := page(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("page1", "SELECT id FROM t ORDER BY id LIMIT 3", []int64{1, 2, 3})
	ck("page2", "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 3", []int64{4, 5, 6})
	ck("page3", "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 6", []int64{7, 8, 9})
	ck("page4_partial", "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 9", []int64{10})
	ck("offset_past_end_empty", "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 100", nil)
	ck("offset_zero", "SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 0", []int64{1, 2})
	ck("limit_exceeds_remainder", "SELECT id FROM t ORDER BY id LIMIT 100 OFFSET 8", []int64{9, 10})

	t.Run("pages_tile_whole_set", func(t *testing.T) {
		var all []int64
		for off := 0; off < 12; off += 4 {
			all = append(all, page(fmt.Sprintf("SELECT id FROM t ORDER BY id LIMIT 4 OFFSET %d", off))...)
		}
		want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		if !eq(all, want) {
			t.Errorf("tiled pages = %v, want %v (no gaps/overlaps)", all, want)
		}
	})
}
