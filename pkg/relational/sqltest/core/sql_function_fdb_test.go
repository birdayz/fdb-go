package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// Schema-template table-valued SQL functions, as Java's CompiledSqlFunction
// binds and runs them.
func TestFDB_SQLFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_sqlfn")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_sqlfn")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sqlfn_tpl "+
		"CREATE TABLE t (id BIGINT, g BIGINT, s STRING, PRIMARY KEY (id)) "+
		"CREATE FUNCTION below(IN n BIGINT, IN tag STRING DEFAULT 'x') AS SELECT id, s FROM t WHERE id < n AND s = tag "+
		"CREATE FUNCTION all_x(IN n BIGINT DEFAULT 10) AS SELECT * FROM below(n) "+
		"CREATE VIEW v AS SELECT id FROM below(3, 'y')")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_sqlfn/s WITH TEMPLATE sqlfn_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_SQLFN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 1, 'x'), (2, 1, 'y'), (3, 2, 'x'), (4, 2, 'x')")

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
	setup := testkit.OpenDB(t, "/FRL/testdb_macro")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_macro")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE macro_tpl "+
		"CREATE TYPE AS STRUCT pt(x BIGINT, y BIGINT) "+
		"CREATE TABLE t (id BIGINT, p pt, PRIMARY KEY (id)) "+
		"CREATE FUNCTION px(IN a TYPE pt) RETURNS BIGINT AS a.x "+
		"CREATE FUNCTION plus(IN a BIGINT, IN b BIGINT DEFAULT 10) RETURNS BIGINT RETURN a + b "+
		"CREATE FUNCTION big(IN a BIGINT) AS SELECT id FROM t WHERE px(p) > a")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_macro/s WITH TEMPLATE macro_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_MACRO?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, (1, 2)), (2, (5, 6)), (3, (9, 1))")

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

// Boolean macro bodies persist as Java's values: a boolean literal as a
// LiteralValue, LIKE as a LikeOperatorValue, which a WHERE lifts back to the
// LIKE predicate.
func TestFDB_BooleanMacroFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_boolmacro")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_boolmacro")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE boolmacro_tpl "+
		"CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id)) "+
		"CREATE FUNCTION always_true() RETURNS BOOLEAN RETURN TRUE "+
		"CREATE FUNCTION always_false() RETURNS BOOLEAN AS FALSE "+
		"CREATE FUNCTION starts_a(IN x STRING) RETURNS BOOLEAN RETURN x LIKE 'a%' "+
		"CREATE FUNCTION pct(IN x STRING) RETURNS BOOLEAN RETURN x LIKE '%!%' ESCAPE '!'")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_boolmacro/s WITH TEMPLATE boolmacro_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_BOOLMACRO?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 'abc'), (2, 'b%'), (3, 'a')")

	for q, want := range map[string]string{
		"SELECT id FROM t WHERE always_true() ORDER BY id":  "[1 2 3]",
		"SELECT id FROM t WHERE always_false() ORDER BY id": "[]",
		"SELECT id FROM t WHERE starts_a(s) ORDER BY id":    "[1 3]",
		"SELECT id FROM t WHERE pct(s) ORDER BY id":         "[2]",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		got := []int64{}
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
	rows, err := db.QueryContext(ctx, "SELECT always_true(), starts_a(s) FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var a, b bool
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprint(a, b))
	}
	rows.Close()
	if want := "[true true true false true true]"; fmt.Sprint(got) != want {
		t.Errorf("projected boolean macros = %v, want %s", got, want)
	}
}

// CREATE TEMPORARY FUNCTION binds a function to the transaction, which drops
// it when it ends (CreateTemporaryFunctionConstantAction).
func TestFDB_TemporaryFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_tempfn")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_tempfn")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE tempfn_tpl "+
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE FUNCTION kept(IN n BIGINT) AS SELECT id FROM t WHERE id = n")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_tempfn/s WITH TEMPLATE tempfn_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_TEMPFN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)")

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
	testkit.RetryTx(t, db, testkit.TxRetryOpts{}, func(at testkit.TxAttempt) error {
		tx := at.Tx
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

// TestFDB_NestedSQLFunctionPlansThroughItsIndex: a call's arguments are a
// one-row values box pushed into its body, so a function calling a function
// carries both bodies' predicates into one index scan. Rewriting must converge:
// rebuilding a child reference per rule firing made this call plan forever.
func TestFDB_NestedSQLFunctionPlansThroughItsIndex(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := testkit.DgcOpen(t, "/FRL/testdb_nestedfn", "nestedfn",
		"CREATE TABLE employees (id BIGINT, name STRING, department STRING, salary BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX dept_idx AS SELECT department, salary FROM employees ORDER BY department, salary "+
			"CREATE FUNCTION employees_in_dept(IN dept STRING) AS SELECT id, name, salary FROM employees WHERE department = dept "+
			"CREATE FUNCTION high_earners(IN dept STRING) AS SELECT * FROM employees_in_dept(dept) WHERE salary > 100000")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO employees VALUES (1, 'Alice', 'Engineering', 100000), "+
		"(2, 'Bob', 'Engineering', 110000), (3, 'Carol', 'Engineering', 150000), (5, 'Eve', 'Sales', 120000)")

	const q = "SELECT id FROM high_earners('Engineering')"
	planCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var plan string
	if err := db.QueryRowContext(planCtx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(plan, "IndexScan(DEPT_IDX, [=, <>]") {
		t.Fatalf("planned %s, want both bodies' predicates in one DEPT_IDX scan", plan)
	}
	if got := testkit.DgcInts(t, db, ctx, q, true); !testkit.DgcEq(got, []int64{2, 3}) {
		t.Errorf("%s = %v, want [2 3]", q, got)
	}
}
