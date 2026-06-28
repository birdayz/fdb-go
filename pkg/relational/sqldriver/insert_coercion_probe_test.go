package sqldriver_test

// Probes INSERT value coercion (wire-relevant — determines the stored type): an
// integer literal into a DOUBLE column is widened to 5.0 (stored as a real DOUBLE,
// so a `d = 5.0` query matches), while incompatible/narrowing assignments are
// rejected 22000 (double→BIGINT, fractional double→BIGINT, string→BIGINT,
// int→STRING) — conformant with Java's PromoteValue (no double→long, no
// string↔numeric implicit coercion).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFDB_InsertCoercionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inscoercep")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inscoercep")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inscoercep CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, n BIGINT, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inscoercep/s WITH TEMPLATE inscoercep")
	dsn := fmt.Sprintf("fdbsql:///testdb_inscoercep?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Run("int_literal_widens_to_double", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d) VALUES (1, 5)")
		var d float64
		if err := db.QueryRowContext(ctx, "SELECT d FROM t WHERE id = 1").Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if math.Abs(d-5.0) > 1e-9 {
			t.Errorf("int 5 → DOUBLE stored as %v, want 5.0", d)
		}
		// stored as a real DOUBLE → a double-equality query matches it.
		var c int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE d = 5.0").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 1 {
			t.Errorf("WHERE d = 5.0 matched %d rows, want 1 (int-inserted value stored as DOUBLE)", c)
		}
	})

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "22000") {
				t.Errorf("%s error = %v, want 22000 (incompatible assignment)", name, err)
			}
		})
	}
	rejected("double_to_bigint", "INSERT INTO t (id, n) VALUES (2, 5.0)")
	rejected("fractional_double_to_bigint", "INSERT INTO t (id, n) VALUES (3, 5.5)")
	rejected("string_to_bigint", "INSERT INTO t (id, n) VALUES (4, '7')")
	rejected("int_to_string", "INSERT INTO t (id, s) VALUES (5, 9)")
}
