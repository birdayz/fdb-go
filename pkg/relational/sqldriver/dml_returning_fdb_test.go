package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// UPDATE and DELETE ... RETURNING answer the modified rows through Query
// (QueryVisitor.visitUpdateStatement / visitDeleteStatement): an UPDATE's
// "old" and "new" records, a DELETE's deleted record. A statement that
// answers rows is refused on Exec, and one that answers none on Query, before
// it runs.
func TestFDB_DMLReturning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_dml_returning")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_dml_returning")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE dml_returning_tpl "+
		"CREATE TABLE a (a1 BIGINT, a2 BIGINT, a3 BIGINT, PRIMARY KEY (a1))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_dml_returning/s WITH TEMPLATE dml_returning_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_DML_RETURNING?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO a VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300)")

	query := func(q string, args ...any) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return "ERROR " + err.Error()
		}
		defer rows.Close()
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
		if err := rows.Err(); err != nil {
			return "ERROR " + err.Error()
		}
		return fmt.Sprintf("%v %v", cols, got)
	}
	for _, tc := range []struct{ query, want string }{
		{`UPDATE a SET a2 = a2 + 1 WHERE a1 <= 2 RETURNING "old".a2, "new".a2`, `[A2 A2] [[10 11] [20 21]]`},
		{`UPDATE a SET a2 = ? WHERE a1 = 1 RETURNING "new".a1, "new".a2 * 2 AS d`, `[A1 D] [[1 84]]`},
		{`UPDATE a SET a3 = 0 WHERE a1 = 3 RETURNING "new".*`, `[A1 A2 A3] [[3 30 0]]`},
		{`UPDATE a SET a3 = 0 WHERE a1 > 100 RETURNING "new".a3`, `[A3] []`},
		{`UPDATE a SET a3 = 7 WHERE a1 = 3 RETURNING "new".a3 OPTIONS (DRY RUN)`, `[A3] [[7]]`},
		{`SELECT a1, a2, a3 FROM a ORDER BY a1`, `[A1 A2 A3] [[1 42 100] [2 21 200] [3 30 0]]`},
		{`DELETE FROM a WHERE a1 = 2 RETURNING a1 + a2 + a3`, `[_0] [[223]]`},
		{`SELECT COUNT(*) FROM a`, `[_0] [[2]]`},
	} {
		var args []any
		if tc.query == `UPDATE a SET a2 = ? WHERE a1 = 1 RETURNING "new".a1, "new".a2 * 2 AS d` {
			args = []any{int64(42)}
		}
		if got := query(tc.query, args...); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.query, got, tc.want)
		}
	}

	for _, tc := range []struct {
		query string
		code  api.ErrorCode
	}{
		{`UPDATE a SET a2 = 1 RETURNING a2`, api.ErrCodeUndefinedColumn},
		{`UPDATE a SET a2 = 1 RETURNING new.a2`, api.ErrCodeUndefinedColumn},
		{`DELETE FROM a RETURNING a.a1`, api.ErrCodeUndefinedColumn},
		{`DELETE FROM a RETURNING *`, api.ErrCodeNoResultSet},
		{`DELETE FROM a WHERE a1 = 1`, api.ErrCodeNoResultSet},
	} {
		_, err := db.QueryContext(ctx, tc.query)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
			t.Errorf("Query %s: want %s, got %v", tc.query, tc.code, err)
		}
	}
	_, err = db.ExecContext(ctx, `DELETE FROM a WHERE a1 = 1 RETURNING a1`)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeExecuteUpdateReturnedResultSet {
		t.Errorf("Exec of a RETURNING statement: want 42F61, got %v", err)
	}
	if got := query(`SELECT COUNT(*) FROM a`); got != `[_0] [[2]]` {
		t.Errorf("refused statements changed the table: %s", got)
	}
	res, err := db.ExecContext(ctx, `DELETE FROM a WHERE a1 = 1 RETURNING *`)
	if err != nil {
		t.Fatalf("Exec of RETURNING *: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RETURNING * deleted %d rows, want 1", n)
	}
}
