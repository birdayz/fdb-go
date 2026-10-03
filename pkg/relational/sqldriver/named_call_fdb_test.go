package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// A macro takes named arguments, bound by name in declaration order with the
// declared defaults; a name given twice is 42601, and a name no parameter has,
// a missing parameter without a default or too many arguments is no such
// function (conformance/named_call_conformance_test.go). A macro may return a
// struct type no table stores, and a built-in reads a named call's values in
// order.
func TestFDB_MacroNamedArguments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_named_call")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_named_call")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE named_call_tpl "+
		"CREATE TYPE AS STRUCT st1(y BIGINT, z BIGINT) "+
		"CREATE TABLE t (id BIGINT, a BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) "+
		"CREATE FUNCTION add2(IN a BIGINT, IN b BIGINT DEFAULT 100) RETURNS BIGINT RETURN a * 10 + b "+
		"CREATE FUNCTION st1_d(IN y BIGINT, IN z BIGINT DEFAULT 2L) RETURNS st1 RETURN (y, z) "+
		"CREATE FUNCTION st1_z(IN s TYPE st1) RETURNS BIGINT RETURN s.z "+
		"CREATE FUNCTION tf(IN lo BIGINT, IN hi BIGINT DEFAULT 10) AS SELECT id FROM t WHERE id BETWEEN lo AND hi")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_named_call/s WITH TEMPLATE named_call_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_NAMED_CALL?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10, [1, 2]), (2, 20, [3])")

	for q, want := range map[string]string{
		"SELECT add2(b => 1, a => a) FROM t WHERE id = 2": "[201]",
		"SELECT add2(a => 3) FROM t WHERE id = 1":         "[130]",
		"SELECT add2(3) FROM t WHERE id = 1":              "[130]",
		"SELECT st1_z(st1_d(y => 4)) FROM t WHERE id = 1": "[2]",
		"SELECT st1_z(st1_d(z => 5, y => 4)) FROM t":      "[5 5]",
		`SELECT st1_z(st1_d("Z" => 7, y => a)) FROM t`:    "[7 7]",
		"SELECT cardinality(x => arr) FROM t ORDER BY id": "[2 1]",
		"SELECT id FROM tf(hi => 1, lo => 1)":             "[1]",
	} {
		got, err := queryInt64s(ctx, db, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}
	for _, c := range []struct {
		sql     string
		code    api.ErrorCode
		message string
	}{
		{"SELECT add2(b => 1) FROM t", api.ErrCodeUndefinedFunction, "could not find function 'ADD2'"},
		{"SELECT add2(a => 1, c => 2) FROM t", api.ErrCodeUndefinedFunction, "could not find function 'ADD2'"},
		{"SELECT add2(1, 2, 3) FROM t", api.ErrCodeUndefinedFunction, "could not find function 'ADD2'"},
		{"SELECT add2() FROM t", api.ErrCodeUndefinedFunction, "could not find function 'ADD2'"},
		{"SELECT add2(a => 1, a => 2) FROM t", api.ErrCodeSyntaxError, "argument name(s) used more than onceA=2"},
		{"SELECT add2(a => 1, b => 2, a => 3, b => 4) FROM t", api.ErrCodeSyntaxError, "argument name(s) used more than onceB=2,A=2"},
		{"SELECT st1_d(y => 4, z => 5, y => 6, z => 7) FROM t", api.ErrCodeSyntaxError, "argument name(s) used more than onceZ=2,Y=2"},
		{"SELECT add2(a => 'x') FROM t", api.ErrCodeInvalidArgumentForFunction, "The function is not defined for the given argument types argument type doesn't match with function definition"},
		{"SELECT nope(x => 1) FROM t", api.ErrCodeUnsupportedQuery, "Unsupported operator NOPE"},
		{"SELECT id FROM tf(hi => 1)", api.ErrCodeUndefinedFunction, "could not find function 'TF'"},
		{"SELECT id FROM tf(lo => 1, x => 2)", api.ErrCodeUndefinedFunction, "could not find function 'TF'"},
		{"SELECT id FROM tf(lo => 1, lo => 2)", api.ErrCodeSyntaxError, "argument name(s) used more than onceLO=2"},
		{"SELECT id FROM tf(1, 2, 3)", api.ErrCodeUndefinedFunction, "could not find function 'TF'"},
	} {
		_, err := queryInt64s(ctx, db, c.sql)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Message != c.message {
			t.Errorf("%s: error %v, want %s %q", c.sql, err, c.code, c.message)
		}
	}
	if _, err := queryInt64s(ctx, db, "SELECT add2(1, b => 2) FROM t"); err == nil {
		t.Error("a mixed named and positional call parsed")
	}
}
