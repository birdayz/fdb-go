package sqldriver_test

// Pins the keyword operators: DIV (integer division) and MOD (modulo) work like
// `/` and `%`; XOR is logical exclusive-or. Shift operators `<<` / `>>` are parsed
// but unimplemented (42883 "Unsupported operator"), a clean rejection.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_KeywordOperatorsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_kwopp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_kwopp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE kwopp CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_kwopp/s WITH TEMPLATE kwopp")
	dsn := fmt.Sprintf("fdbsql:///testdb_kwopp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	t.Run("div_mod_keywords", func(t *testing.T) {
		var d, m int64
		if err := db.QueryRowContext(ctx, "SELECT 7 DIV 2, 7 MOD 3 FROM t WHERE id = 1").Scan(&d, &m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if d != 3 || m != 1 {
			t.Errorf("7 DIV 2, 7 MOD 3 = %d, %d; want 3, 1", d, m)
		}
	})
	t.Run("xor_logical", func(t *testing.T) {
		var a, b bool
		if err := db.QueryRowContext(ctx, "SELECT TRUE XOR TRUE, TRUE XOR FALSE FROM t WHERE id = 1").Scan(&a, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if a != false || b != true {
			t.Errorf("TRUE XOR TRUE, TRUE XOR FALSE = %v, %v; want false, true", a, b)
		}
	})
	rejected := func(name, expr string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1")
			if err == nil || !strings.Contains(err.Error(), "42883") {
				t.Errorf("%s error = %v, want 42883 (shift operator unsupported)", expr, err)
			}
		})
	}
	rejected("shift_right_unsupported", "6 >> 1")
	rejected("shift_left_unsupported", "3 << 2")
}
