package sqldriver_test

// Probes the TIMESTAMP, DATE, and UUID column types (accepted but previously
// untested): string-literal round-trip, chronological ORDER BY (not accidental
// lexicographic), range/equality/BETWEEN comparison, and UUID equality. Non-string
// (epoch-int) assignment to a temporal column is a clean 22000 type error.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_TemporalUuidTypesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_tut")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_tut")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE tut CREATE TABLE t (id BIGINT NOT NULL, ts TIMESTAMP, d DATE, u UUID, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_tut/s WITH TEMPLATE tut")
	dsn := fmt.Sprintf("fdbsql:///testdb_tut?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, ts, d) VALUES "+
		"(1,'2024-01-15 10:30:00','2024-01-15'),(2,'2024-03-20 08:00:00','2024-03-20'),(3,'2023-12-01 12:00:00','2023-12-01')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, u) VALUES (5, '550e8400-e29b-41d4-a716-446655440000')")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		return o
	}
	idsSorted := func(q string) []int64 {
		o := ids(q)
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

	t.Run("timestamp_roundtrip", func(t *testing.T) {
		var s string
		if err := db.QueryRowContext(ctx, "SELECT CAST(ts AS STRING) FROM t WHERE id = 1").Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s != "2024-01-15 10:30:00" {
			t.Errorf("TIMESTAMP round-trip = %q", s)
		}
	})
	t.Run("timestamp_chronological_order", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE ts IS NOT NULL ORDER BY ts"); !eq(got, []int64{3, 1, 2}) {
			t.Errorf("ORDER BY ts = %v, want [3 1 2] (2023-12, 2024-01, 2024-03)", got)
		}
	})
	t.Run("timestamp_comparison", func(t *testing.T) {
		if got := idsSorted("SELECT id FROM t WHERE ts > '2024-01-01 00:00:00'"); !eq(got, []int64{1, 2}) {
			t.Errorf("WHERE ts > 2024-01-01 = %v, want [1 2]", got)
		}
	})
	t.Run("date_order_and_between", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE d IS NOT NULL ORDER BY d"); !eq(got, []int64{3, 1, 2}) {
			t.Errorf("ORDER BY d = %v, want [3 1 2]", got)
		}
		if got := ids("SELECT id FROM t WHERE d = '2024-03-20'"); !eq(got, []int64{2}) {
			t.Errorf("WHERE d = 2024-03-20 = %v, want [2]", got)
		}
		if got := ids("SELECT id FROM t WHERE d BETWEEN '2024-01-01' AND '2024-02-01'"); !eq(got, []int64{1}) {
			t.Errorf("WHERE d BETWEEN = %v, want [1]", got)
		}
	})
	t.Run("uuid_roundtrip_and_equality", func(t *testing.T) {
		var s string
		if err := db.QueryRowContext(ctx, "SELECT CAST(u AS STRING) FROM t WHERE id = 5").Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("UUID round-trip = %q", s)
		}
		if got := ids("SELECT id FROM t WHERE u = '550e8400-e29b-41d4-a716-446655440000'"); !eq(got, []int64{5}) {
			t.Errorf("WHERE u = <uuid> = %v, want [5]", got)
		}
	})
	t.Run("temporal_epoch_int_type_error", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, ts) VALUES (9, 1705314600000)")
		if err == nil || !strings.Contains(err.Error(), "22000") {
			t.Errorf("epoch-int into TIMESTAMP error = %v, want 22000", err)
		}
	})
}
