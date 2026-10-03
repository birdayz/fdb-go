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
		"SELECT * FROM below('a')":            api.ErrCodeInvalidArgumentForFunction,
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

// Schema-template macro functions, expanded at each call as Java's
// UserDefinedMacroFunction.encapsulate does.
func TestFDB_MacroFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_macro")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_macro")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE macro_tpl "+
		"CREATE TYPE AS STRUCT pt(x BIGINT, y BIGINT) "+
		"CREATE TABLE t (id BIGINT, p pt, PRIMARY KEY (id)) "+
		"CREATE FUNCTION px(IN a TYPE pt) RETURNS BIGINT AS a.x "+
		"CREATE FUNCTION plus(IN a BIGINT, IN b BIGINT DEFAULT 10) RETURNS BIGINT RETURN a + b "+
		"CREATE FUNCTION big(IN a BIGINT) AS SELECT id FROM t WHERE px(p) > a")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_macro/s WITH TEMPLATE macro_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_MACRO?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, (1, 2)), (2, (5, 6)), (3, (9, 1))")

	for q, want := range map[string]string{
		"SELECT px(p) FROM t ORDER BY id":               "[1 5 9]",
		"SELECT plus(px(p), id) FROM t WHERE id = 2":    "[7]",
		"SELECT plus(id) FROM t WHERE id = 3":           "[13]",
		"SELECT id FROM t WHERE px(p) >= 5 ORDER BY id": "[2 3]",
		"SELECT id FROM big(4) ORDER BY id":             "[2 3]",
		"SELECT px((7, 8)) FROM range(1, 3)":            "[7 7]",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			got = append(got, v)
		}
		rows.Close()
		if fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}
	for q, code := range map[string]api.ErrorCode{
		"SELECT plus() FROM t":        api.ErrCodeUndefinedFunction,
		"SELECT plus(1, 2, 3) FROM t": api.ErrCodeUndefinedFunction,
		"SELECT px(id) FROM t":        api.ErrCodeCannotConvertType,
		"SELECT nope(id) FROM t":      api.ErrCodeUnsupportedQuery,
	} {
		_, err := db.QueryContext(ctx, q)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != code {
			t.Errorf("%s: want %s, got %v", q, code, err)
		}
	}
}

// CREATE TEMPORARY FUNCTION binds a function to the transaction, which drops
// it when it ends (CreateTemporaryFunctionConstantAction).
func TestFDB_TemporaryFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_tempfn")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_tempfn")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE tempfn_tpl "+
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE FUNCTION kept(IN n BIGINT) AS SELECT id FROM t WHERE id = n")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_tempfn/s WITH TEMPLATE tempfn_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_TEMPFN?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)")

	ids := func(q interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}, query string,
	) string {
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return err.Error()
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		return fmt.Sprint(got)
	}
	retryTx(t, db, txRetryOpts{}, func(at txAttempt) error {
		tx := at.tx
		for _, s := range []string{
			"CREATE TEMPORARY FUNCTION big() ON COMMIT DROP FUNCTION AS SELECT id FROM t WHERE v > 15",
			"CREATE TEMPORARY FUNCTION twice(IN x BIGINT) RETURNS BIGINT ON COMMIT DROP FUNCTION RETURN x + x",
		} {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		for q, want := range map[string]string{
			"SELECT id FROM big ORDER BY id":                              "[2 3]",
			"SELECT twice(id) FROM t WHERE id = 3":                        "[6]",
			"SELECT b.id FROM big() AS b, kept(3) AS k WHERE b.id = k.id": "[3]",
		} {
			if got := ids(tx, q); got != want {
				t.Errorf("%s: %s, want %s", q, got, want)
			}
		}
		for s, code := range map[string]api.ErrorCode{
			"CREATE TEMPORARY FUNCTION big() ON COMMIT DROP FUNCTION AS SELECT id FROM t":  api.ErrCodeDuplicateFunction,
			"CREATE TEMPORARY FUNCTION kept() ON COMMIT DROP FUNCTION AS SELECT id FROM t": api.ErrCodeDuplicateFunction,
			"DROP TEMPORARY FUNCTION kept": api.ErrCodeInvalidFunctionDefinition,
			"DROP TEMPORARY FUNCTION nope": api.ErrCodeUndefinedFunction,
		} {
			_, err := tx.ExecContext(ctx, s)
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != code {
				t.Errorf("%s: want %s, got %v", s, code, err)
			}
		}
		if _, err := tx.ExecContext(ctx, "CREATE OR REPLACE TEMPORARY FUNCTION big() ON COMMIT DROP FUNCTION AS SELECT id FROM t WHERE v > 25"); err != nil {
			return err
		}
		if got := ids(tx, "SELECT id FROM big"); got != "[3]" {
			t.Errorf("replaced big: %s", got)
		}
		if _, err := tx.ExecContext(ctx, "DROP TEMPORARY FUNCTION IF EXISTS twice"); err != nil {
			return err
		}
		return tx.Commit()
	})
	if got := ids(db, "SELECT id FROM big"); got == "[3]" || got == "[2 3]" {
		t.Errorf("temporary function outlived its transaction: %s", got)
	}
}
