package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// An ARRAY parameter of every element type binds and stores
// (prepared.yamsql, type-with-arrays-roundtrip): a bool element binds as a
// boolean, not as a NULL.
func TestFDB_ArrayParameterElements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_array_params")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_array_params")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE array_params_tpl
		CREATE TABLE a (pk BIGINT, bigint_array BIGINT ARRAY, integer_array INTEGER ARRAY,
			double_array DOUBLE ARRAY, float_array FLOAT ARRAY, string_array STRING ARRAY,
			boolean_array BOOLEAN ARRAY, bytes_array BYTES ARRAY, PRIMARY KEY (pk))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_array_params/s WITH TEMPLATE array_params_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ARRAY_PARAMS?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx, `INSERT INTO a (pk, bigint_array, integer_array, double_array, float_array,
		string_array, boolean_array, bytes_array) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		int64(1), []any{int64(10), int64(20)}, []any{int32(1), int32(2)}, []any{1.5, 2.5},
		[]any{float32(0.5), float32(0.25)}, []any{"a", "b"}, []any{true, false},
		[]any{[]byte{0xca, 0xfe}, []byte{0xf0}}); err != nil {
		t.Fatalf("INSERT array parameters: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO a (pk, boolean_array) VALUES (?, ?)`,
		int64(2), []bool{false, true, true}); err != nil {
		t.Fatalf("INSERT []bool parameter: %v", err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM a WHERE boolean_array = ?`, []bool{true, false}).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows matching [true, false]: %d, %v; want 1", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM a WHERE boolean_array = ?`, []any{false, true, true}).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows matching [false, true, true]: %d, %v; want 1", n, err)
	}
}
