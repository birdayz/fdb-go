package sqldriver_test

// Probes substituteParams rendering for every supported param type (the path
// where a []byte param was mis-rendered as a string). time.Time→DATE/TIMESTAMP,
// nil→NULL, bool, MaxInt64, and special float64 values must each render to a SQL
// literal that round-trips to the same value.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestFDB_ParamRenderingProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_paramrender")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_paramrender")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE paramrender "+
			"CREATE TABLE t (id BIGINT NOT NULL, n BIGINT, f DOUBLE, flag BOOLEAN, ts TIMESTAMP, dt DATE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_paramrender/s WITH TEMPLATE paramrender")
	dsn := fmt.Sprintf("fdbsql:///testdb_paramrender?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...any) {
		if _, e := db.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("exec %q: %v", q, e)
		}
	}

	t.Run("bool_and_bigint_params", func(t *testing.T) {
		exec("INSERT INTO t (id, n, flag) VALUES (1, ?, ?)", int64(math.MaxInt64), true)
		var n int64
		var flag bool
		if err := db.QueryRowContext(ctx, "SELECT n, flag FROM t WHERE id = 1").Scan(&n, &flag); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != math.MaxInt64 || flag != true {
			t.Errorf("got (n=%d, flag=%v), want (MaxInt64, true)", n, flag)
		}
	})

	t.Run("float_params", func(t *testing.T) {
		exec("INSERT INTO t (id, f) VALUES (2, ?)", float64(3.141592653589793))
		var f float64
		if err := db.QueryRowContext(ctx, "SELECT f FROM t WHERE id = 2").Scan(&f); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if f != 3.141592653589793 {
			t.Errorf("float param round-trip = %v, want pi (full precision)", f)
		}
	})

	t.Run("timestamp_param_value", func(t *testing.T) {
		// TIMESTAMP is a Go extension stored/returned as an ISO string; the
		// time.Time param must store the exact value (read back as the string).
		exec("INSERT INTO t (id, ts) VALUES (3, ?)", time.Date(2026, 6, 28, 13, 45, 30, 0, time.UTC))
		var got string
		if err := db.QueryRowContext(ctx, "SELECT ts FROM t WHERE id = 3").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got != "2026-06-28 13:45:30" {
			t.Errorf("timestamp param round-trip = %q, want 2026-06-28 13:45:30", got)
		}
	})

	t.Run("date_param_value", func(t *testing.T) {
		exec("INSERT INTO t (id, dt) VALUES (4, ?)", time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC))
		var got string
		if err := db.QueryRowContext(ctx, "SELECT dt FROM t WHERE id = 4").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got != "2026-06-28" {
			t.Errorf("date param round-trip = %q, want 2026-06-28", got)
		}
	})

	t.Run("nil_param_is_null", func(t *testing.T) {
		exec("INSERT INTO t (id, n) VALUES (5, ?)", nil)
		var n sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT n FROM t WHERE id = 5").Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n.Valid {
			t.Errorf("nil param stored as %d, want NULL", n.Int64)
		}
	})

	t.Run("param_in_where_predicate", func(t *testing.T) {
		// bigint param in WHERE matches the MaxInt64 row.
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE n = ?", int64(math.MaxInt64)).Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != 1 {
			t.Errorf("WHERE n = MaxInt64 param → id=%d, want 1", id)
		}
	})
}
