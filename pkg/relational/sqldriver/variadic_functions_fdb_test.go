package sqldriver_test

// COALESCE, GREATEST and LEAST as Java 4.14.2.0's VariadicFunctionValue has
// them: two or more arguments, an operator map (no BYTES, no all-NULL call),
// every argument evaluated, and result nullability by function.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_VariadicFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_variadic")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_variadic")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE variadic_tmpl "+
		"CREATE TABLE T (id BIGINT, n BIGINT, b BYTES, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_variadic/s WITH TEMPLATE variadic_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_VARIADIC?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO T VALUES (1, NULL, X'00'), (2, 5, X'01')")

	for _, c := range []struct {
		sql  string
		code api.ErrorCode
	}{
		{"SELECT COALESCE(NULL, NULL) FROM T WHERE id = 1", api.ErrCodeInvalidArgumentForFunction},
		{"SELECT GREATEST(NULL, NULL) FROM T WHERE id = 1", api.ErrCodeInvalidArgumentForFunction},
		{"SELECT COALESCE(b, X'00') FROM T", api.ErrCodeInvalidArgumentForFunction},
		{"SELECT COALESCE(n, 'a') FROM T WHERE id = 1", api.ErrCodeCannotConvertType},
		{"SELECT COALESCE(1) FROM T WHERE id = 1", api.ErrCodeInternalError},
		{"SELECT GREATEST(1) FROM T WHERE id = 1", api.ErrCodeInternalError},
		{"SELECT COALESCE(1, 1 / 0) FROM T WHERE id = 1", api.ErrCodeDivisionByZero},
		{"SELECT COALESCE(id, 1 / 0) FROM T WHERE id = 1", api.ErrCodeDivisionByZero},
	} {
		rows, err := db.QueryContext(ctx, c.sql)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != c.code {
			t.Errorf("%s: want %s, got %v", c.sql, c.code, err)
		}
	}

	nullable := func(q string) bool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		n, ok := cts[0].Nullable()
		if !ok {
			t.Fatalf("%s: nullability unknown", q)
		}
		return n
	}
	for q, want := range map[string]bool{
		"SELECT COALESCE(n, 0) FROM T WHERE id = 1":   false,
		"SELECT COALESCE(n, n) FROM T WHERE id = 1":   true,
		"SELECT COALESCE(1, 2) FROM T WHERE id = 1":   false,
		"SELECT GREATEST(1, 5) FROM T WHERE id = 1":   false,
		"SELECT GREATEST(1, 2.5) FROM T WHERE id = 1": false,
		"SELECT LEAST(n, 1) FROM T WHERE id = 1":      true,
	} {
		if got := nullable(q); got != want {
			t.Errorf("%s: nullable = %v, want %v", q, got, want)
		}
	}

	// In a predicate a COALESCE over constant heads still folds.
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM T WHERE COALESCE(NULL, n) = 5").Scan(&id); err != nil || id != 2 {
		t.Fatalf("COALESCE(NULL, n) = 5: %d, %v", id, err)
	}

	// A boolean expression is a function argument like any other.
	ids := func(q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			out = append(out, v)
		}
		return out
	}
	for q, want := range map[string]string{
		"SELECT id FROM T WHERE COALESCE(n = 5, FALSE) ORDER BY id":      "[2]",
		"SELECT id FROM T WHERE NOT COALESCE(n = 5, FALSE) ORDER BY id":  "[1]",
		"SELECT id FROM T WHERE COALESCE(NULL, TRUE, n = 1) ORDER BY id": "[1 2]",
	} {
		if got := fmt.Sprint(ids(q)); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	var got []bool
	rows, err := db.QueryContext(ctx, "SELECT COALESCE(n = 5, FALSE) FROM T ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b bool
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	rows.Close()
	if fmt.Sprint(got) != "[false true]" {
		t.Errorf("COALESCE(n = 5, FALSE) projection: %v", got)
	}
}
