package sqldriver_test

// Driver parameters are bound as typed constants, as Java's JDBC binds them
// (Type.fromObject), not substituted into the SQL text.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/relational/api"
	"github.com/google/uuid"
)

func TestFDB_ParameterBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_param_bind")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_param_bind")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE param_bind_tmpl "+
		"CREATE TABLE T (id BIGINT, n BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) "+
		"CREATE TABLE U (id BIGINT, u UUID, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_param_bind/s WITH TEMPLATE param_bind_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_PARAM_BIND?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO T VALUES (1, 10), (2, 20), (3, 30)")

	ids := func(q string, args ...any) ([]int64, error) {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	code := func(err error) api.ErrorCode {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return apiErr.Code
		}
		return ""
	}

	for _, c := range []struct {
		name string
		sql  string
		args []any
		want []int64
	}{
		{"named ?x", "SELECT id FROM T WHERE id = ?x", []any{sql.Named("x", int64(1))}, []int64{1}},
		{"named $x", "SELECT id FROM T WHERE id = $x", []any{sql.Named("x", int64(1))}, []int64{1}},
		{"named twice", "SELECT id FROM T WHERE id = ?x OR id = ?x + 2 ORDER BY id", []any{sql.Named("x", int64(1))}, []int64{1, 3}},
		{"mixed", "SELECT id FROM T WHERE id = ? OR id = ?x ORDER BY id", []any{int64(1), sql.Named("x", int64(3))}, []int64{1, 3}},
		{"IN array", "SELECT id FROM T WHERE id IN ? ORDER BY id", []any{[]int64{1, 3}}, []int64{1, 3}},
		{"too many args", "SELECT id FROM T WHERE id = ?", []any{int64(1), int64(2)}, []int64{1}},
		{"comment ?", "SELECT id FROM T WHERE id = ? -- why?", []any{int64(2)}, []int64{2}},
	} {
		got, err := ids(c.sql, c.args...)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %s = %v, %v; want %v", c.name, c.sql, got, err, c.want)
		}
	}

	// setInt is INT and overflows; setLong is LONG and does not.
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT ? + 2147483647 FROM T WHERE id = 1", int32(1)).Scan(&n); code(err) != api.ErrCodeNumericValueOutOfRange {
		t.Errorf("INT parameter + INT_MAX: want 22003, got %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT ? + 2147483647 FROM T WHERE id = 1", int64(1)).Scan(&n); err != nil || n != 2147483648 {
		t.Errorf("LONG parameter + INT_MAX = %d, %v", n, err)
	}
	// A bound NULL in an IN list is a NULL array element (0A000), not a bare NULL.
	if _, err := ids("SELECT id FROM T WHERE id IN (?)", nil); code(err) != api.ErrCodeUnsupportedOperation {
		t.Errorf("IN (?) bound NULL: want 0A000, got %v", err)
	}
	if _, err := ids("SELECT id FROM T WHERE id = ? OR id = ?", int64(1)); code(err) != api.ErrCodeUndefinedParameter {
		t.Errorf("missing parameter: want 42F02, got %v", err)
	}
	// A bound NULL compares like an inline NULL.
	if got, err := ids("SELECT id FROM T WHERE n = ?", nil); err != nil || len(got) != 0 {
		t.Errorf("n = NULL: %v, %v", got, err)
	}

	// Array and UUID parameters store their values.
	if _, err := db.ExecContext(ctx, "INSERT INTO A VALUES (?, ?)", int64(1), []int64{7, 8}); err != nil {
		t.Fatalf("INSERT array parameter: %v", err)
	}
	if got, err := ids("SELECT id FROM A WHERE arr = ?", []int64{7, 8}); err != nil || !slices.Equal(got, []int64{1}) {
		t.Errorf("stored array: %v, %v", got, err)
	}
	u := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	if _, err := db.ExecContext(ctx, "INSERT INTO U VALUES (?, ?)", int64(1), u); err != nil {
		t.Fatalf("INSERT uuid parameter: %v", err)
	}
	if got, err := ids("SELECT id FROM U WHERE u = ?", u); err != nil || !slices.Equal(got, []int64{1}) {
		t.Errorf("uuid parameter lookup: %v, %v", got, err)
	}
}

// The binding order's lanes, stored and read back: bytes from [N]byte and a
// named []byte, a UUID through a pointer, a null wrapper by its payload, and
// empty arrays (static element type or untyped) as empty values, not NULL.
func TestFDB_ParameterTypingOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_param_typing")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_param_typing")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE param_typing_tmpl "+
		"CREATE TABLE B (id BIGINT, b BYTES, PRIMARY KEY (id)) "+
		"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) "+
		"CREATE TABLE U (id BIGINT, u UUID, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_param_typing/s WITH TEMPLATE param_typing_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_PARAM_TYPING?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	code := func(err error) api.ErrorCode {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return apiErr.Code
		}
		return ""
	}

	type blob []byte
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO B VALUES (?, ?), (?, ?)", int64(1), [4]byte{1, 2, 3, 4}, int64(2), blob{9})
	for id, want := range map[int64][]byte{1: {1, 2, 3, 4}, 2: {9}} {
		var got []byte
		if err := db.QueryRowContext(ctx, "SELECT b FROM B WHERE id = ?", id).Scan(&got); err != nil || !bytes.Equal(got, want) {
			t.Errorf("BYTES %d: %v, %v; want %v", id, got, err, want)
		}
	}

	u := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	exec("INSERT INTO U VALUES (?, ?), (?, ?)", int64(1), &u, int64(2), (*uuid.UUID)(nil))
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM U WHERE u = ?", &u).Scan(&n); err != nil || n != 1 {
		t.Errorf("*uuid.UUID lookup: %d, %v", n, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT id FROM U WHERE u IS NULL").Scan(&n); err != nil || n != 2 {
		t.Errorf("nil *uuid.UUID stored NULL: %d, %v", n, err)
	}

	// sql.NullInt64 is LONG: no INT overflow.
	if err := db.QueryRowContext(ctx, "SELECT ? + 2147483647 FROM U WHERE id = 1", sql.NullInt64{Int64: 1, Valid: true}).Scan(&n); err != nil || n != 2147483648 {
		t.Errorf("NullInt64 + INT_MAX = %d, %v", n, err)
	}

	exec("INSERT INTO A VALUES (?, ?), (?, ?), (?, ?)", int64(1), []int{}, int64(2), []int64(nil), int64(3), nil)
	rows, err := db.QueryContext(ctx, "SELECT id, arr IS NULL, CARDINALITY(arr) FROM A ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id int64
		var isNull bool
		var card sql.NullInt64
		if err := rows.Scan(&id, &isNull, &card); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%t:%v", id, isNull, card))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if want := []string{"1:false:{0 true}", "2:false:{0 true}", "3:true:{0 false}"}; !slices.Equal(got, want) {
		t.Errorf("empty and NULL arrays: %v, want %v", got, want)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM A WHERE id IN ?", []int{}).Scan(&n); err != nil || n != 0 {
		t.Errorf("IN empty []int: %d, %v", n, err)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO A VALUES (?, ?)", int64(4), [][]int64{{1}}); code(err) != api.ErrCodeInvalidParameter {
		t.Errorf("nested array parameter: want 22023, got %v", err)
	}
}
