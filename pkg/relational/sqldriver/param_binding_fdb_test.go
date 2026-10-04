package sqldriver_test

// Driver parameters are bound as typed constants, as Java's JDBC binds them
// (Type.fromObject), not substituted into the SQL text.

import (
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
	setup := openTestDB(t, "/testdb_param_bind")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_param_bind")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE param_bind_tmpl "+
		"CREATE TABLE T (id BIGINT, n BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) "+
		"CREATE TABLE U (id BIGINT, u UUID, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_param_bind/s WITH TEMPLATE param_bind_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_PARAM_BIND?cluster_file=%s&schema=S", clusterFilePath))
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
