package sqldriver_test

// An ARRAY element is never NULL (Java 4.14.2.0): a NULL-typed element is
// refused before planning, a nullable element that evaluates to NULL when it
// does. The approved read extension keeps a comparison of two literal array
// constructors with NULL elements.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_ArrayNullElements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_arr_null")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_arr_null")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE arr_null_tmpl "+
		"CREATE TABLE T (id BIGINT, n BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, m BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_arr_null/s WITH TEMPLATE arr_null_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_ARR_NULL?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO T VALUES (1, NULL), (2, 5)")
	mustExec(t, db, ctx, "INSERT INTO A VALUES (1, [5], 5)")

	run := func(q string, args ...any) (int, error) {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}
	for _, q := range []string{
		"SELECT [1, NULL] FROM T WHERE id = 1",
		"SELECT CARDINALITY([1, NULL]) FROM T WHERE id = 1",
		"SELECT [CAST(NULL AS BIGINT)] FROM T WHERE id = 1",
		"SELECT id, [n] FROM T ORDER BY id",
		"SELECT id FROM A WHERE arr = [m, NULL]",
	} {
		var apiErr *api.Error
		if _, err := run(q); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
			t.Errorf("%s: want 0A000, got %v", q, err)
		}
	}
	var apiErr *api.Error
	if _, err := run("SELECT [?] FROM T WHERE id = 1", nil); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
		t.Errorf("bound NULL element: want 0A000, got %v", err)
	}
	// A nullable element that is NOT NULL on the rows read is fine, and a
	// nullable column element promotes to the column's element type.
	if n, err := run("SELECT id, [n] FROM T WHERE id = 2"); err != nil || n != 1 {
		t.Errorf("[n] over a non-NULL row: %d, %v", n, err)
	}
	if n, err := run("SELECT id FROM A WHERE arr = [m]"); err != nil || n != 1 {
		t.Errorf("arr = [m]: %d, %v", n, err)
	}
	// The read extension: literal-array comparisons keep NULL elements.
	var b bool
	if err := db.QueryRowContext(ctx, "SELECT [1, NULL] = [1, NULL] FROM T WHERE id = 1").Scan(&b); err != nil || !b {
		t.Errorf("[1, NULL] = [1, NULL]: %v, %v", b, err)
	}
}
