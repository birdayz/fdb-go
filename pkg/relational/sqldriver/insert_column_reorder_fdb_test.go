package sqldriver_test

// Pins Java's INSERT column-list semantics
// (ExpressionVisitor.parseRecordFieldsUnderReorderings, ExpressionVisitor.java
// :1040-1078, tag 4.12.11.0): the row is built by iterating the TARGET
// table's fields and looking each up in the provided column list, so
//
//   - a provided column that is NOT a target field is silently IGNORED,
//     along with its value (the vendored corpus depends on it:
//     composite-aggregates.yamsql inserts into T2(ID, COL1, COL2, COL3)
//     while T2 has no COL3 — Java accepts and drops the 4th value; Go used
//     to 42703-reject, a shared-surface divergence armed the moment RFC-202
//     S3 let that file's DDL through);
//   - a nullable target field absent from the list is filled with NULL;
//   - a NON-nullable target field absent from the list is 23502 (the only
//     non-nullable columns are ARRAY NOT NULL ones — scalar NOT NULL is
//     unexpressible, Java parity: NOT NULL is only allowed for ARRAY column
//     type, rejected at CREATE — so the arm is probed through an array);
//   - more VALUES than named columns is 42601 "Too many parameters";
//   - a named target field with no value is 42601 "Value of column X is not
//     provided".

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_InsertColumnListReorderings(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_iclr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_iclr")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE iclr "+
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT, a BIGINT ARRAY NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_iclr/s WITH TEMPLATE iclr")
	dsn := fmt.Sprintf("fdbsql:///testdb_iclr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// (1) Unknown named column: accepted, its value dropped — the exact
	// corpus shape (an extra trailing column and value).
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t(id, a, b, no_such_col) VALUES (1, 10, 20, 999)"); err != nil {
		t.Fatalf("unknown named column must be ignored (Java :1057 indexOf never finds it): %v", err)
	}
	var a, b int64
	if err := db.QueryRowContext(ctx, "SELECT a, b FROM t WHERE id = 1").Scan(&a, &b); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if a != 10 || b != 20 {
		t.Fatalf("row (a,b) = (%d,%d), want (10,20) — the dropped 4th value must not shift the others", a, b)
	}

	// (2) Nullable field absent from the list → NULL.
	if _, err := db.ExecContext(ctx, "INSERT INTO t(id, b) VALUES (2, 22)"); err != nil {
		t.Fatalf("absent nullable column must fill NULL (Java :1067-1074): %v", err)
	}
	var aNull sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 2").Scan(&aNull); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if aNull.Valid {
		t.Fatalf("a = %v, want NULL", aNull.Int64)
	}

	// (3) Non-nullable field absent from the list → 23502. u.a is
	// BIGINT ARRAY NOT NULL — the one remaining non-nullable column shape.
	_, err = db.ExecContext(ctx, "INSERT INTO u(id) VALUES (3)")
	if err == nil || !strings.Contains(err.Error(), "23502") {
		t.Fatalf("absent NOT NULL ARRAY column must be 23502 (Java :1068), got: %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "violates not-null constraint") {
		t.Fatalf("want Java's wording 'violates not-null constraint', got: %v", err)
	}

	// (4) More VALUES than named columns → 42601 "Too many parameters".
	_, err = db.ExecContext(ctx, "INSERT INTO t(id, a) VALUES (4, 1, 2, 3)")
	if err == nil || !strings.Contains(err.Error(), "Too many parameters") {
		t.Fatalf("want 'Too many parameters' (Java :1055), got: %v", err)
	}

	// (5) Named target field without a value → 42601 "Value of column X is
	// not provided".
	_, err = db.ExecContext(ctx, "INSERT INTO t(id, a, b) VALUES (5)")
	if err == nil || !strings.Contains(err.Error(), `Value of column "A" is not provided`) {
		t.Fatalf(`want 'Value of column "A" is not provided' (Java :1064), got: %v`, err)
	}
}
