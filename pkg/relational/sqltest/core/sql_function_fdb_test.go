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

// A macro over a struct with an enum field persists the enum type
// (Type.Enum.toProto) and runs from the stored metadata.
func TestFDB_EnumStructMacroFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_enummacro")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_enummacro")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE enummacro_tpl "+
		"CREATE TYPE AS ENUM mood ('HAPPY', 'SAD') "+
		"CREATE TYPE AS STRUCT st(m mood, n BIGINT) "+
		"CREATE TABLE t (id BIGINT, p st, PRIMARY KEY (id)) "+
		"CREATE FUNCTION st_n(IN x TYPE st) RETURNS BIGINT AS x.n "+
		"CREATE FUNCTION st_m(IN x TYPE st) AS x.m")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_enummacro/s WITH TEMPLATE enummacro_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ENUMMACRO?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, ('HAPPY', 10)), (2, ('SAD', 20))")
	rows, err := db.QueryContext(ctx, "SELECT st_n(p), st_m(p) FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var n int64
		var m string
		if err := rows.Scan(&n, &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %s", n, m))
	}
	rows.Close()
	if want := "[10 HAPPY 20 SAD]"; fmt.Sprint(got) != want {
		t.Errorf("enum struct macros = %v, want %s", got, want)
	}
}

// A macro parameter's struct type is Java's descriptor type: a field whose
// identifier needs escaping is named as written and stored under its
// protobuf spelling, so the body can reach it.
func TestFDB_MacroOverEscapedStructField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_escmacro")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_escmacro")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE escmacro_tpl "+
		`CREATE TYPE AS STRUCT st("a.b" BIGINT, c BIGINT) `+
		"CREATE TABLE t (id BIGINT, p st, PRIMARY KEY (id)) "+
		`CREATE FUNCTION ab(IN x TYPE st) RETURNS BIGINT AS x."a.b"`)
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_escmacro/s WITH TEMPLATE escmacro_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ESCMACRO?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, (5, 6)), (2, (7, 8))")
	rows, err := db.QueryContext(ctx, "SELECT ab(p) FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
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
	if fmt.Sprint(got) != "[5 7]" {
		t.Errorf("ab(p) = %v, want [5 7]", got)
	}
}

// Comparison, AND/OR, NOT and IS NULL macro bodies persist as Java's
// RelOpValue/AndOrValue/NotValue trees and, called, plan and evaluate exactly
// as their bodies written inline.
func TestFDB_ComparisonMacroFunctions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_relopmacro")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_relopmacro")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE relopmacro_tpl "+
		"CREATE TABLE t (id BIGINT, a BIGINT, b BOOLEAN, s STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX t_a AS SELECT a FROM t ORDER BY a "+
		"CREATE FUNCTION gt5(IN x BIGINT) RETURNS BOOLEAN RETURN x > 5 "+
		"CREATE FUNCTION isn(IN x BIGINT) RETURNS BOOLEAN RETURN x IS NULL "+
		"CREATE FUNCTION nb(IN x BOOLEAN) RETURNS BOOLEAN RETURN NOT x "+
		"CREATE FUNCTION both_(IN x BOOLEAN, IN y BIGINT) RETURNS BOOLEAN RETURN x AND y >= 3 "+
		"CREATE FUNCTION combo(IN x BIGINT, IN y STRING) RETURNS BOOLEAN RETURN x >= 2 AND NOT (y = 'q') OR y IS NULL "+
		"CREATE FUNCTION dist(IN x BIGINT, IN y BIGINT) RETURNS BOOLEAN RETURN x IS DISTINCT FROM y "+
		"CREATE FUNCTION inl(IN x BIGINT) RETURNS BOOLEAN RETURN x IN (1, 3, 9) "+
		"CREATE FUNCTION ninl(IN x BIGINT, IN y BIGINT) RETURNS BOOLEAN RETURN x NOT IN (y, 3) "+
		"CREATE FUNCTION btw(IN x BIGINT) RETURNS BOOLEAN RETURN x BETWEEN 2 AND 7 "+
		"CREATE FUNCTION nbtw(IN x BIGINT) RETURNS BOOLEAN RETURN x NOT BETWEEN 2 AND 7 "+
		"CREATE FUNCTION gtf(IN x BIGINT) RETURNS BOOLEAN RETURN x > 2.5 "+
		"CREATE FUNCTION ist(IN x BOOLEAN) RETURNS BOOLEAN RETURN x IS NOT TRUE "+
		"CREATE FUNCTION lk(IN x STRING) RETURNS BOOLEAN RETURN x NOT LIKE 'q%' "+
		"CREATE FUNCTION cs(IN x BIGINT, IN y BOOLEAN) RETURNS BOOLEAN RETURN CASE WHEN x > 5 THEN y WHEN x IS NULL THEN FALSE ELSE x < 2 END")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_relopmacro/s WITH TEMPLATE relopmacro_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_RELOPMACRO?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 1, TRUE, 'q'), (2, 3, FALSE, 'r'), (3, 7, TRUE, NULL), (4, NULL, NULL, 'q'), (5, 9, TRUE, 'z')")

	query := func(q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id int64
			var v sql.NullBool
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d:%v/%v", id, v.Valid, v.Bool))
		}
		return strings.Join(out, " ")
	}
	explain := func(q string) string {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("explain %s: %v", q, err)
		}
		return plan
	}
	for call, inline := range map[string]string{
		"gt5(a)":      "a > 5",
		"isn(a)":      "a IS NULL",
		"nb(b)":       "NOT b",
		"both_(b, a)": "b AND a >= 3",
		"combo(a, s)": "a >= 2 AND NOT (s = 'q') OR s IS NULL",
		"dist(a, 3)":  "a IS DISTINCT FROM 3",
		"inl(a)":      "a IN (1, 3, 9)",
		"ninl(a, id)": "a NOT IN (id, 3)",
		"btw(a)":      "a BETWEEN 2 AND 7",
		"nbtw(a)":     "a NOT BETWEEN 2 AND 7",
		"gtf(a)":      "a > 2.5",
		"ist(b)":      "b IS NOT TRUE",
		"lk(s)":       "s NOT LIKE 'q%'",
		"cs(a, b)":    "CASE WHEN a > 5 THEN b WHEN a IS NULL THEN FALSE ELSE a < 2 END",
	} {
		mq := "SELECT id, " + call + " FROM t ORDER BY id"
		iq := "SELECT id, " + inline + " FROM t ORDER BY id"
		if got, want := query(mq), query(iq); got != want {
			t.Errorf("projected %s = %s, inline %s", call, got, want)
		}
		mw := "SELECT id, TRUE FROM t WHERE " + call + " ORDER BY id"
		iw := "SELECT id, TRUE FROM t WHERE " + inline + " ORDER BY id"
		if got, want := query(mw), query(iw); got != want {
			t.Errorf("filtered %s = %s, inline %s", call, got, want)
		}
		if got, want := explain(mw), explain(iw); got != want {
			t.Errorf("%s plans as %s, inline as %s", call, got, want)
		}
	}
	if got := query("SELECT id, TRUE FROM t WHERE gt5(a) ORDER BY id"); got != "3:true/true 5:true/true" {
		t.Errorf("gt5 rows = %s", got)
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

// An array of structs whose field is a quoted name with a dot ("a.b", stored
// a__2b) inserts and reads back: the array's element type names the field by
// its user identifier while each element literal is built from the stored
// descriptor, and the two must still agree.
func TestFDB_StructArrayInsertWithEscapedField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_escarr")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_escarr")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE escarr_tpl "+
		`CREATE TYPE AS STRUCT st("a.b" BIGINT, c BIGINT) `+
		"CREATE TABLE t (id BIGINT, ps st ARRAY, PRIMARY KEY (id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_escarr/s WITH TEMPLATE escarr_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ESCARR?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, [(1, 2), (3, 4)])")
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT e."a.b", e.c FROM t, t.ps AS e ORDER BY e."a.b"`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, c int64
		if err := rows.Scan(&a, &c); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d/%d", a, c))
	}
	rows.Close()
	if fmt.Sprint(got) != "[1/2 3/4]" {
		t.Errorf("elements = %v, want [1/2 3/4]", got)
	}
}
