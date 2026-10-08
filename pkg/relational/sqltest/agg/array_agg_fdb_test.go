package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// ARRAY_AGG as in Java 4.14.2.0's ArrayAggValue.
func TestFDB_ArrayAgg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_array_agg")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_array_agg")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE array_agg_tmpl "+
		"CREATE TABLE T (id BIGINT, g BIGINT, n BIGINT, s STRING, PRIMARY KEY (id)) CREATE INDEX T_G ON T (g)")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_array_agg/s WITH TEMPLATE array_agg_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ARRAY_AGG?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO T VALUES (1, 1, 10, 'a'), (2, 1, NULL, 'b'), (3, 2, 30, 'c'), (4, 2, 40, 'd')")

	rowsOf := func(q string) (string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return "", err
			}
			out = append(out, fmt.Sprint(vals...))
		}
		return fmt.Sprint(out), rows.Err()
	}
	for q, want := range map[string]string{
		"SELECT ARRAY_AGG(id) FROM T":                                     "[[1 2 3 4]]",
		"SELECT ARRAY_AGG(s) FROM T WHERE g = 2":                          "[[c d]]",
		"SELECT g, ARRAY_AGG(id) FROM T GROUP BY g":                       "[1 [1 2] 2 [3 4]]",
		"SELECT g, ARRAY_AGG(n IGNORE NULLS) FROM T GROUP BY g":           "[1 [10] 2 [30 40]]",
		"SELECT g, ARRAY_AGG(id LIMIT 1) FROM T GROUP BY g":               "[1 [1] 2 [3]]",
		"SELECT g, ARRAY_AGG(id LIMIT 0) FROM T GROUP BY g":               "[1 [] 2 []]",
		"SELECT g, ARRAY_AGG(n RESPECT NULLS LIMIT 1) FROM T GROUP BY g":  "[1 [10] 2 [30]]",
		"SELECT ARRAY_AGG(id) FROM T WHERE id > 100":                      "[<nil>]",
		"SELECT g, ARRAY_AGG(id) FROM T WHERE id > 100 GROUP BY g":        "[]",
		"SELECT ARRAY_AGG(n IGNORE NULLS) FROM T WHERE id = 2":            "[[]]",
		"SELECT ARRAY_AGG(id), ARRAY_AGG(id IGNORE NULLS LIMIT 2) FROM T": "[[1 2 3 4] [1 2]]",
	} {
		if got, err := rowsOf(q); err != nil || got != want {
			t.Errorf("%s: %s %v, want %s", q, got, err, want)
		}
	}
	for q, code := range map[string]api.ErrorCode{
		"SELECT g, ARRAY_AGG(n) FROM T GROUP BY g":          api.ErrCodeUnsupportedOperation,
		"SELECT ARRAY_AGG(DISTINCT id) FROM T":              api.ErrCodeUnsupportedQuery,
		"SELECT ARRAY_AGG(id) OVER (PARTITION BY g) FROM T": api.ErrCodeUnsupportedQuery,
		"SELECT ARRAY_AGG(id ORDER BY id) FROM T":           api.ErrCodeUnsupportedQuery,
		"SELECT ARRAY_AGG(id LIMIT 2147483648) FROM T":      api.ErrCodeInvalidParameter,
		"SELECT ARRAY_AGG(NULL) FROM T":                     api.ErrCodeUnknownType,
	} {
		_, err := rowsOf(q)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != code {
			t.Errorf("%s: want %s, got %v", q, code, err)
		}
	}
}
