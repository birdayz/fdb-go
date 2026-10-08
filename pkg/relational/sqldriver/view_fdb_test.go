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

// Schema-template views as Java's DdlVisitor stores and compiles them.
func TestFDB_Views(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_views")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_views")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE views_tmpl "+
		"CREATE TABLE T (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE VIEW V1 AS SELECT id, v FROM T WHERE g > 0 "+
		"CREATE VIEW V2 AS SELECT id FROM V1 WHERE v < 10 "+
		`CREATE VIEW "q_view" AS SELECT id AS "k" FROM T `+
		`CREATE INDEX "q_idx" ON "q_view" ("k") `+
		"CREATE VIEW AGG AS SELECT g, MAX_EVER(v) AS m FROM T GROUP BY g "+
		"CREATE INDEX AGG_IDX ON AGG (m, g)")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_views/s WITH TEMPLATE views_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_VIEWS?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO T VALUES (1, 1, 5), (2, 0, 5), (3, 1, 50), (4, 2, 7)")

	for q, want := range map[string]string{
		"SELECT id FROM V1 ORDER BY id":                                "[1 3 4]",
		"SELECT id FROM V2 ORDER BY id":                                "[1 4]",
		"SELECT x.id FROM V2 AS x, T WHERE x.id = T.id AND T.v = 7":    "[4]",
		"WITH V1 AS (SELECT id FROM T WHERE id = 2) SELECT id FROM V1": "[2]",
		`SELECT "k" FROM "q_view" WHERE "k" > 2 ORDER BY "k"`:          "[3 4]",
		"SELECT MAX_EVER(v) FROM T WHERE g = 1 GROUP BY g":             "[50]",
	} {
		rows, err := db.QueryContext(ctx, q)
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

	for i, c := range []struct {
		body string
		code api.ErrorCode
	}{
		{"CREATE TABLE T (id BIGINT, PRIMARY KEY (id)) CREATE VIEW T AS SELECT id FROM T", api.ErrCodeInvalidSchemaTemplate},
		{"CREATE TABLE T (id BIGINT, PRIMARY KEY (id)) CREATE VIEW V AS SELECT nope FROM T", api.ErrCodeUndefinedColumn},
		{"CREATE TABLE T (id BIGINT, PRIMARY KEY (id)) CREATE VIEW V AS SELECT id FROM T WHERE id = ?", api.ErrCodeSyntaxError},
	} {
		_, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE views_bad_%d %s", i, c.body))
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != c.code {
			t.Errorf("%s: want %s, got %v", c.body, c.code, err)
		}
	}
}
