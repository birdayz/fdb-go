package sqldriver_test

// Probes INSERT/UPDATE value type coercion — WIRE-RELEVANT. An int literal into a
// DOUBLE column must be WIDENED and stored as a double (5 → 5.0), so the stored
// record's wire type is correct (Java reads it back as a double) and a
// double-typed index probe finds it. Narrowing a double literal into a BIGINT
// column is rejected (22000) — Java's PromoteValue has no double→long promotion,
// so both engines reject it (conformant).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFDB_InsertTypeCoercionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inscoerce")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inscoerce")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inscoerce "+
			"CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, n BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_d ON t (d) CREATE INDEX t_n ON t (n)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inscoerce/s WITH TEMPLATE inscoerce")
	dsn := fmt.Sprintf("fdbsql:///testdb_inscoerce?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// int literal 5 WIDENED into DOUBLE column d; int 5 into BIGINT n (same type).
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d, n) VALUES (1, 5, 5)")

	t.Run("double_col_reads_back_as_double", func(t *testing.T) {
		var d float64
		if err := db.QueryRowContext(ctx, "SELECT d FROM t WHERE id = 1").Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if math.Abs(d-5.0) > 1e-9 {
			t.Errorf("d (int 5 widened into DOUBLE col) = %v, want 5.0", d)
		}
	})

	// Decisive WIRE test: an int literal stored into a DOUBLE column must be
	// stored AS a double, so a double-typed index probe finds it. If it fails,
	// INSERT stored the wrong wire type (and Java would misread the record).
	t.Run("double_index_probe_finds_int_inserted", func(t *testing.T) {
		var id sql.NullInt64
		err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE d = 5.0").Scan(&id)
		if err == sql.ErrNoRows {
			t.Fatal("d=5.0 found nothing — int literal stored with the wrong wire type (not widened to DOUBLE)")
		}
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !id.Valid || id.Int64 != 1 {
			t.Errorf("d=5.0 → id=%v, want 1", id.Int64)
		}
	})

	// Narrowing double→BIGINT on INSERT is rejected (22000), matching Java
	// (PromoteValue has only widening promotions; double→long is not promotable).
	t.Run("narrowing_double_to_bigint_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, n) VALUES (3, 7.0)")
		if err == nil {
			t.Fatal("INSERT 7.0 into BIGINT succeeded; Java rejects narrowing (no double→long promote)")
		}
		if !strings.Contains(err.Error(), "22000") {
			t.Errorf("narrowing INSERT error = %v, want SQLSTATE 22000", err)
		}
	})

	// UPDATE cross-type widening: set DOUBLE d to an int literal; the index must
	// reflect the widened double value.
	t.Run("update_int_into_double_col", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "UPDATE t SET d = 9 WHERE id = 1") // int 9 widened into DOUBLE
		var id sql.NullInt64
		err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE d = 9.0").Scan(&id)
		if err == sql.ErrNoRows {
			t.Fatal("after UPDATE d=9 (int), d=9.0 found nothing — UPDATE stored wrong wire type")
		}
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !id.Valid || id.Int64 != 1 {
			t.Errorf("after UPDATE, d=9.0 → id=%v, want 1", id.Int64)
		}
	})
}
