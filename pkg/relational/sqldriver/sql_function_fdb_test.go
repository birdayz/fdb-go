package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// Schema-template table-valued SQL functions, as Java's CompiledSqlFunction
// binds and runs them.
func TestFDB_SQLFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_sqlfn")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_sqlfn")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sqlfn_tpl "+
		"CREATE TABLE t (id BIGINT, g BIGINT, s STRING, PRIMARY KEY (id)) "+
		"CREATE FUNCTION below(IN n BIGINT, IN tag STRING DEFAULT 'x') AS SELECT id, s FROM t WHERE id < n AND s = tag "+
		"CREATE FUNCTION all_x(IN n BIGINT DEFAULT 10) AS SELECT * FROM below(n) "+
		"CREATE VIEW v AS SELECT id FROM below(3, 'y')")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sqlfn/s WITH TEMPLATE sqlfn_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_SQLFN?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 1, 'x'), (2, 1, 'y'), (3, 2, 'x'), (4, 2, 'x')")

	for q, want := range map[string]string{
		"SELECT id FROM below(4) ORDER BY id":                                          "[1 3]",
		"SELECT id FROM below(tag => 'y', n => 5)":                                     "[2]",
		"SELECT id FROM all_x ORDER BY id":                                             "[1 3 4]",
		"SELECT id FROM v":                                                             "[2]",
		"SELECT b.id FROM below(10) AS b WHERE b.id > 1 ORDER BY b.id":                 "[3 4]",
		"SELECT t.id FROM t WHERE EXISTS (SELECT 1 FROM below(t.g + 2)) ORDER BY t.id": "[1 2 3 4]",
		"WITH all_x AS (SELECT id FROM t WHERE id = 2) SELECT id FROM all_x":           "[2]",
		"SELECT id FROM below(?) ORDER BY id":                                          "[1]",
	} {
		var args []any
		if q == "SELECT id FROM below(?) ORDER BY id" {
			args = []any{int64(2)}
		}
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		rows.Close()
		if fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}

	for q, code := range map[string]api.ErrorCode{
		"SELECT * FROM below('a')":            api.ErrCodeUndefinedFunction,
		"SELECT * FROM below()":               api.ErrCodeUndefinedFunction,
		"SELECT * FROM below":                 api.ErrCodeUndefinedFunction,
		"SELECT * FROM below(1, 'x', 3)":      api.ErrCodeUndefinedFunction,
		"SELECT * FROM below(n => 1, n => 2)": api.ErrCodeSyntaxError,
	} {
		_, err := db.QueryContext(ctx, q)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != code {
			t.Errorf("%s: want %s, got %v", q, code, err)
		}
	}
}
