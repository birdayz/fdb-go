package sqltest

// A NULL IN-list item (Java 4.14.2.0): a bare NULL is 42809 before planning;
// a parenthesised, typed, bound or column NULL is an ARRAY element and is
// 0A000 when the list is evaluated.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_InListNullItems(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_in_null")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_in_null")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE in_null_tmpl "+
		"CREATE TABLE T (id BIGINT, n BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_in_null/s WITH TEMPLATE in_null_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_IN_NULL?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO T VALUES (1, NULL), (2, 5)")

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
	code := func(err error) api.ErrorCode {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return apiErr.Code
		}
		return ""
	}
	for _, tc := range []struct {
		q    string
		args []any
		want api.ErrorCode
	}{
		{q: "SELECT id FROM T WHERE id IN (NULL)", want: "42809"},
		{q: "SELECT id FROM T WHERE id IN (1, NULL)", want: "42809"},
		{q: "SELECT id FROM T WHERE id NOT IN (NULL)", want: "42809"},
		{q: "SELECT id FROM T WHERE id IN ((NULL))", want: "0A000"},
		{q: "SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT))", want: "0A000"},
		{q: "SELECT id FROM T WHERE id IN (1, CAST(NULL AS BIGINT))", want: "0A000"},
		{q: "SELECT id FROM E WHERE id IN (CAST(NULL AS BIGINT))", want: "0A000"},
		{q: "SELECT id FROM T WHERE id < 0 AND id IN (CAST(NULL AS BIGINT))", want: "0A000"},
		{q: "SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT), id)", want: "0A000"},
		{q: "SELECT id FROM T WHERE id IN (?)", args: []any{nil}, want: "0A000"},
		{q: "SELECT id FROM T WHERE id IN (1, ?)", args: []any{nil}, want: "0A000"},
		{q: "SELECT id FROM T WHERE id IN (n, 1)", want: "0A000"},
		{q: "SELECT id FROM T WHERE id NOT IN (CAST(NULL AS BIGINT))", want: "0A000"},
		{q: "SELECT id IN (CAST(NULL AS BIGINT)) FROM T", want: "0A000"},
		// Per-row evaluation over an empty table never evaluates the list.
		{q: "SELECT id FROM E WHERE id NOT IN (CAST(NULL AS BIGINT))"},
		{q: "SELECT id IN (CAST(NULL AS BIGINT)) FROM E"},
		{q: "SELECT x FROM E WHERE x IN (CAST(NULL AS BIGINT), x)"},
		// Java explodes an unindexed IN too and fails when the explode opens;
		// Go keeps it a residual filter, so over an empty table it answers no
		// rows (a superset of Java's answers, never different rows).
		{q: "SELECT x FROM E WHERE x IN (CAST(NULL AS BIGINT))"},
		{q: "SELECT id FROM T WHERE id IN (n, 1) AND id = 2"},
	} {
		_, err := run(tc.q, tc.args...)
		if got := code(err); got != tc.want {
			t.Errorf("%s %v: want %q, got %v", tc.q, tc.args, tc.want, err)
		}
	}
}
