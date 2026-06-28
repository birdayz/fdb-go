package sqldriver_test

// Probes aggregates over empty and all-NULL inputs (SQL-standard): over zero rows
// or an all-NULL column, SUM/MIN/MAX/AVG return NULL while COUNT(*) returns 0 and
// COUNT(col) returns 0. Distinguishes NULL (no rows / all-NULL) from 0.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_EmptyAggregateProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_emptyagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_emptyagg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE emptyagg CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_emptyagg/s WITH TEMPLATE emptyagg")
	dsn := fmt.Sprintf("fdbsql:///testdb_emptyagg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Two rows, both v NULL.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1), (2)")

	nullInt := func(q string) sql.NullInt64 {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	nullFloat := func(q string) sql.NullFloat64 {
		var v sql.NullFloat64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}

	// --- all-NULL column (2 rows, both v NULL) ---
	t.Run("count_star_all_null", func(t *testing.T) {
		if got := nullInt("SELECT COUNT(*) FROM t"); !got.Valid || got.Int64 != 2 {
			t.Errorf("COUNT(*) over 2 all-NULL rows = %v, want 2", got)
		}
	})
	t.Run("count_col_all_null", func(t *testing.T) {
		if got := nullInt("SELECT COUNT(v) FROM t"); !got.Valid || got.Int64 != 0 {
			t.Errorf("COUNT(v) over all-NULL = %v, want 0", got)
		}
	})
	t.Run("sum_all_null_is_null", func(t *testing.T) {
		if got := nullInt("SELECT SUM(v) FROM t"); got.Valid {
			t.Errorf("SUM(v) over all-NULL = %d, want NULL", got.Int64)
		}
	})
	t.Run("max_all_null_is_null", func(t *testing.T) {
		if got := nullInt("SELECT MAX(v) FROM t"); got.Valid {
			t.Errorf("MAX(v) over all-NULL = %d, want NULL", got.Int64)
		}
	})
	t.Run("avg_all_null_is_null", func(t *testing.T) {
		if got := nullFloat("SELECT AVG(v) FROM t"); got.Valid {
			t.Errorf("AVG(v) over all-NULL = %v, want NULL", got.Float64)
		}
	})

	// --- empty input (WHERE matches nothing) ---
	t.Run("count_star_empty_is_zero", func(t *testing.T) {
		if got := nullInt("SELECT COUNT(*) FROM t WHERE id = 999"); !got.Valid || got.Int64 != 0 {
			t.Errorf("COUNT(*) over empty = %v, want 0", got)
		}
	})
	t.Run("sum_empty_is_null", func(t *testing.T) {
		if got := nullInt("SELECT SUM(v) FROM t WHERE id = 999"); got.Valid {
			t.Errorf("SUM(v) over empty = %d, want NULL", got.Int64)
		}
	})
	t.Run("max_empty_is_null", func(t *testing.T) {
		if got := nullInt("SELECT MAX(v) FROM t WHERE id = 999"); got.Valid {
			t.Errorf("MAX(v) over empty = %d, want NULL", got.Int64)
		}
	})
}
