package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// The built-in range() table function (Java's RangeValue.RangeFn).
func TestFDB_RangeTableFunction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_rangefn")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_rangefn")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE rangefn_tpl "+
		"CREATE TABLE t1 (id BIGINT, col1 STRING, PRIMARY KEY (id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_rangefn/s WITH TEMPLATE rangefn_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_RANGEFN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 'a'), (2, 'b'), (3, 'c')")

	for _, c := range []struct {
		q    string
		args []any
		want string
	}{
		{q: "SELECT * FROM range(1, 4)", want: "1;2;3"},
		{q: "SELECT * FROM range(0, 12, 5)", want: "0;5;10"},
		{q: "SELECT * FROM range(6 - 6, 14 + 6 + 1, 20 - 10)", want: "0;10;20"},
		{q: "SELECT ID AS x FROM range(3) AS y", want: "0;1;2"},
		{q: "SELECT * FROM range(0)", want: ""},
		{q: "SELECT * FROM range(?)", args: []any{int64(2)}, want: "0;1"},
		{q: "SELECT x.id, y.id FROM range(2) AS x, range(2) AS y", want: "0,0;0,1;1,0;1,1"},
		{q: "SELECT a.id, b.id FROM t1 AS a, range(a.id) AS b", want: "1,0;2,0;2,1;3,0;3,1;3,2"},
		{q: "SELECT t1.id FROM t1 WHERE EXISTS (SELECT 1 FROM range(t1.id) AS r WHERE r.id = 2)", want: "3"},
		{q: "SELECT count(*) FROM range(1000)", want: "1000"},
	} {
		rows, err := db.QueryContext(ctx, c.q, c.args...)
		if err != nil {
			t.Errorf("%s: %v", c.q, err)
			continue
		}
		cols, _ := rows.Columns()
		var got []string
		for rows.Next() {
			vals := make([]int64, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprint(v)
			}
			got = append(got, strings.Join(parts, ","))
		}
		if err := rows.Err(); err != nil {
			t.Errorf("%s: %v", c.q, err)
		}
		rows.Close()
		if g := strings.Join(got, ";"); g != c.want {
			t.Errorf("%s = %q, want %q", c.q, g, c.want)
		}
	}

	for q, want := range map[string]api.ErrorCode{
		"SELECT * FROM range('a')":        api.ErrCodeCannotConvertType,
		"SELECT * FROM range((1, 2))":     api.ErrCodeCannotConvertType,
		"SELECT * FROM range(1, 2, 3, 4)": api.ErrCodeUndefinedFunction,
		"SELECT * FROM nosuchfn(1)":       api.ErrCodeUndefinedFunction,
		"SELECT * FROM range(nosuch.id)":  api.ErrCodeUndefinedColumn,
	} {
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != want {
			t.Errorf("%s: err = %v, want %s", q, err, want)
		}
	}
	// Java's checkValidRange is a RecordCoreException at execution.
	for _, q := range []string{"SELECT * FROM range(-1)", "SELECT * FROM range(-1, 4)", "SELECT * FROM range(1, 4, -1)", "SELECT * FROM range(1, 4, 0)"} {
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "in range") {
			t.Errorf("%s: err = %v, want a range-bounds error", q, err)
		}
	}
}
