package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Java folds IS [NOT] NULL over a NOT NULL constant and arithmetic over a
// typed NULL without evaluating the rest, so these answer instead of 22012.
func TestFDB_ConstantNullFolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_null_fold")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_null_fold")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE null_fold_tmpl "+
		"CREATE TABLE T (id BIGINT, n BIGINT, PRIMARY KEY (id)) CREATE INDEX T_N ON T (n)")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_null_fold/s WITH TEMPLATE null_fold_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_NULL_FOLD?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO T VALUES (1, NULL), (2, 5), (3, 7)")

	for q, want := range map[string]string{
		"SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NULL ORDER BY id":                        "[]",
		"SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NOT NULL ORDER BY id":                    "[1 2 3]",
		"SELECT id FROM T WHERE n > 0 AND COALESCE(1 / 0, 5) IS NULL ORDER BY id":              "[]",
		"SELECT id FROM T WHERE n = 5 AND COALESCE(1 / 0, 5) IS NULL":                          "[]",
		"SELECT id FROM T WHERE CAST('x' AS BIGINT) IS NULL ORDER BY id":                       "[]",
		"SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) IS NULL ORDER BY id":           "[1 2 3]",
		"SELECT id FROM T WHERE n > 0 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL ORDER BY id": "[2 3]",
		"SELECT id FROM T WHERE (5 / 0) + CAST(NULL AS INTEGER) = 5":                           "[]",
		"SELECT id FROM T WHERE id = 2 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL":            "[2]",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		got := []int64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Errorf("%s: %v", q, err)
		}
		rows.Close()
		if fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}

	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT (1 / 0) + CAST(NULL AS INTEGER) FROM T WHERE id = 1").Scan(&v); err != nil || v.Valid {
		t.Errorf("(1 / 0) + CAST(NULL AS INTEGER): %v, %v", v, err)
	}
	rows, err := db.QueryContext(ctx, "SELECT CAST(1 AS BIGINT), CAST(n AS BIGINT) FROM T WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	cts, _ := rows.ColumnTypes()
	rows.Close()
	for i, want := range []bool{false, true} {
		if n, ok := cts[i].Nullable(); !ok || n != want {
			t.Errorf("CAST column %d nullable = %v, want %v", i, n, want)
		}
	}
}
