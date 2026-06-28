package sqldriver_test

// Probes CAST edge cases: string→BIGINT parse, double→BIGINT ROUNDS half-up
// (3.9→4, 3.4→3, -3.9→-4; matches Java Math.round, NOT truncation), non-numeric
// string→BIGINT errors 22F3H, int→DOUBLE / int→STRING, and string/int→BOOLEAN
// coercion (CAST(1 AS BOOLEAN)=true — explicit cast coerces, unlike the implicit
// `flag = 1` comparison).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CastEdgesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_castp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_castp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE castp CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_castp/s WITH TEMPLATE castp")
	dsn := fmt.Sprintf("fdbsql:///testdb_castp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	str := func(expr string) string {
		var v sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v.String
	}
	ck := func(name, expr, want string) {
		t.Run(name, func(t *testing.T) {
			if got := str(expr); got != want {
				t.Errorf("%s = %q, want %q", expr, got, want)
			}
		})
	}

	ck("string_to_bigint", "CAST('123' AS BIGINT)", "123")
	ck("double_to_bigint_round_up", "CAST(3.9 AS BIGINT)", "4")
	ck("double_to_bigint_round_down", "CAST(3.4 AS BIGINT)", "3")
	ck("double_to_bigint_round_neg", "CAST(-3.9 AS BIGINT)", "-4")
	ck("int_to_double", "CAST(5 AS DOUBLE)", "5")
	ck("int_to_string", "CAST(42 AS STRING)", "42")
	ck("string_to_boolean", "CAST('true' AS BOOLEAN)", "true")
	ck("int_to_boolean", "CAST(1 AS BOOLEAN)", "true")
	ck("maxint_bigint", "CAST(9223372036854775807 AS BIGINT)", "9223372036854775807")

	t.Run("nonnumeric_string_to_bigint_errors", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT CAST('abc' AS BIGINT) FROM t WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "22F3H") {
			t.Errorf("CAST('abc' AS BIGINT) error = %v, want 22F3H (cannot cast)", err)
		}
	})
}
