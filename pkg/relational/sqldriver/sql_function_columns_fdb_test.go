package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// A table function call and a recursive CTE read their rows through a
// quantifier, which names a repeated or unnamed column by its position
// (Expressions.underlyingAsColumns); a derived table keeps its select list's
// names. A function's name may be any quoted identifier.
func TestFDB_QuantifierColumnNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_qcols")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_qcols")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE qcols_tpl "+
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE FUNCTION fd(IN x BIGINT) AS SELECT a.id, b.id FROM t a, t b WHERE a.id = x AND b.id = x "+
		"CREATE FUNCTION fs(IN x BIGINT) AS SELECT * FROM t a, t b WHERE a.id = x AND b.id = x "+
		"CREATE FUNCTION fe(IN x BIGINT) AS SELECT id + 1, id FROM t WHERE id = x "+
		`CREATE FUNCTION "नमस्त"(IN x BIGINT) AS SELECT id FROM t WHERE id = x`)
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_qcols/s WITH TEMPLATE qcols_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_QCOLS?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10), (2, 20)")

	for _, tc := range []struct{ query, want string }{
		{`SELECT * FROM fd(1)`, `[_0 _1] [[1 1]]`},
		{`SELECT * FROM fs(1)`, `[_0 _1 _2 _3] [[1 10 1 10]]`},
		{`SELECT * FROM fe(1)`, `[_0 ID] [[2 1]]`},
		{`SELECT q."_1" FROM fd(1) q`, `[_1] [[1]]`},
		{`SELECT q.id FROM fe(1) q`, `[ID] [[1]]`},
		{`SELECT 'é' AS e, q.id FROM "नमस्त"(2) q`, `[E ID] [[é 2]]`},
		{`SELECT * FROM (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1) q`, `[ID ID] [[1 1]]`},
		{`WITH RECURSIVE r AS (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1 ` +
			`UNION ALL SELECT r."_0" + 1, r."_1" FROM r WHERE r."_0" < 3) SELECT * FROM r`, `[_0 _1] [[1 1] [2 1] [3 1]]`},
	} {
		rows, err := db.QueryContext(ctx, tc.query)
		if err != nil {
			t.Errorf("%s: %v", tc.query, err)
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var got [][]any
		for rows.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			got = append(got, cells)
		}
		rows.Close()
		if line := fmt.Sprintf("%v %v", cols, got); line != tc.want {
			t.Errorf("%s: %s, want %s", tc.query, line, tc.want)
		}
	}

	for _, q := range []string{
		`SELECT q.id FROM fd(1) q`,
		`SELECT q.v FROM fs(1) q`,
		`WITH RECURSIVE r AS (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1 ` +
			`UNION ALL SELECT r.id + 1, r."_1" FROM r WHERE r."_0" < 3) SELECT * FROM r`,
	} {
		_, err := db.QueryContext(ctx, q)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
			t.Errorf("%s: want 42703, got %v", q, err)
		}
	}
}
