package sqldriver_test

// Probes degenerate pagination + empty-input aggregation (the same "degenerate
// value" vein that surfaced the LIMIT 0 bug): OFFSET beyond the row count, OFFSET
// 0, LIMIT+OFFSET combinations, and scalar/grouped aggregates over an empty
// input (COUNT→0, SUM/MIN/MAX/AVG→NULL, GROUP BY→no groups).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OffsetEmptyAggProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_offagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_offagg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE offagg "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_offagg/s WITH TEMPLATE offagg")
	dsn := fmt.Sprintf("fdbsql:///testdb_offagg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 10), (2, 20), (3, 30)")

	count := func(q string) int {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n
	}

	// --- Pagination edges ---
	t.Run("offset_beyond_rows", func(t *testing.T) {
		if got := count("SELECT id FROM t ORDER BY id LIMIT 10 OFFSET 5"); got != 0 {
			t.Errorf("OFFSET 5 over 3 rows = %d, want 0", got)
		}
	})
	t.Run("offset_zero_all", func(t *testing.T) {
		if got := count("SELECT id FROM t ORDER BY id LIMIT 10 OFFSET 0"); got != 3 {
			t.Errorf("OFFSET 0 = %d, want 3", got)
		}
	})
	t.Run("limit_offset_mid", func(t *testing.T) {
		// rows ordered 1,2,3; OFFSET 1 LIMIT 1 → just id=2.
		rows, _ := db.QueryContext(ctx, "SELECT id FROM t ORDER BY id LIMIT 1 OFFSET 1")
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			got = append(got, v)
		}
		if len(got) != 1 || got[0] != 2 {
			t.Errorf("LIMIT 1 OFFSET 1 = %v, want [2]", got)
		}
	})
	t.Run("offset_exactly_rows", func(t *testing.T) {
		if got := count("SELECT id FROM t ORDER BY id LIMIT 10 OFFSET 3"); got != 0 {
			t.Errorf("OFFSET 3 over 3 rows = %d, want 0", got)
		}
	})

	// --- Empty-input scalar aggregation (WHERE matches nothing) ---
	t.Run("empty_count_zero", func(t *testing.T) {
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE v > 1000").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 0 {
			t.Errorf("COUNT(*) over empty = %d, want 0", c)
		}
	})
	t.Run("empty_sum_null", func(t *testing.T) {
		var s sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT SUM(v) FROM t WHERE v > 1000").Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s.Valid {
			t.Errorf("SUM over empty = %d, want NULL", s.Int64)
		}
	})
	t.Run("empty_max_null", func(t *testing.T) {
		var m sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT MAX(v) FROM t WHERE v > 1000").Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if m.Valid {
			t.Errorf("MAX over empty = %d, want NULL", m.Int64)
		}
	})
	t.Run("empty_min_null", func(t *testing.T) {
		var m sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT MIN(v) FROM t WHERE v > 1000").Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if m.Valid {
			t.Errorf("MIN over empty = %d, want NULL", m.Int64)
		}
	})

	// --- Empty-input grouped aggregation → zero groups ---
	t.Run("empty_groupby_no_groups", func(t *testing.T) {
		if got := count("SELECT v, COUNT(*) FROM t WHERE v > 1000 GROUP BY v"); got != 0 {
			t.Errorf("GROUP BY over empty = %d groups, want 0", got)
		}
	})
}
