package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// A record constructor's field takes its element's own name — an alias, else a
// column's — unless another element has it; a write binds the record to its
// target struct by position, and COALESCE promotes its operands to their
// common type (conformance/record_names_conformance_test.go).
func TestFDB_RecordConstructorFieldNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_record_names")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_record_names")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE record_names_tpl "+
		"CREATE TYPE AS STRUCT S(a BIGINT, b BIGINT) "+
		"CREATE TABLE t (id BIGINT, x BIGINT, y BIGINT, s S, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT, s S, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_record_names/s WITH TEMPLATE record_names_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_RECORD_NAMES?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10, 20, (1, 2)), (2, 30, 40, NULL)")

	structs := func(query string) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			out = append(out, fmt.Sprint(structFields(v)))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return fmt.Sprint(out)
	}
	for q, want := range map[string]string{
		"SELECT (x, y) FROM t WHERE id = 1":                               "[map[X:10 Y:20]]",
		"SELECT (x + 1, y) FROM t WHERE id = 1":                           "[map[Y:20 _0:11]]",
		"SELECT (x AS p, t.y) FROM t WHERE id = 1":                        "[map[P:10 Y:20]]",
		"SELECT (x, x) FROM t WHERE id = 1":                               "[map[_0:10 _1:10]]",
		"SELECT (s.a, s) FROM t WHERE id = 1":                             "[map[A:1 S:map[A:1 B:2]]]",
		"SELECT coalesce(s, (x, y)) FROM t ORDER BY id":                   "[map[_0:1 _1:2] map[_0:30 _1:40]]",
		"SELECT d.r.y FROM (SELECT (x, y) AS r FROM t WHERE id = 1) AS d": "[20]",
	} {
		if got := structs(q); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	mustExec(t, db, ctx, "INSERT INTO u SELECT id, (y AS b, x AS a) FROM t WHERE id = 1")
	mustExec(t, db, ctx, "UPDATE t SET s = coalesce(s, (x, y))")
	mustExec(t, db, ctx, "INSERT INTO u VALUES (2, (5 AS a, 6 AS b))")
	for q, want := range map[string]string{
		"SELECT s FROM u ORDER BY id": "[map[A:20 B:10] map[A:5 B:6]]",
		"SELECT s FROM t ORDER BY id": "[map[A:1 B:2] map[A:30 B:40]]",
	} {
		if got := structs(q); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	mustExec(t, db, ctx, "UPDATE t SET s = (y AS b, x AS a) WHERE id = 2")
	if got, want := structs("SELECT s FROM t WHERE id = 2"), "[map[A:40 B:30]]"; got != want {
		t.Errorf("UPDATE by position: %s, want %s", got, want)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO u VALUES (3, (6 AS b, 5 AS a))")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError || apiErr.Message != "condition is not met!" {
		t.Errorf("INSERT VALUES naming another field: %v, want XX000 condition is not met!", err)
	}
}

// structFields renders a STRUCT cell as its attribute names and values.
func structFields(v any) any {
	st, ok := v.(api.Struct)
	if !ok {
		return v
	}
	out := map[string]any{}
	for i, a := range st.Attributes() {
		name, err := st.MetaData().AttributeName(i + 1)
		if err != nil {
			name = fmt.Sprintf("?%d", i)
		}
		out[name] = structFields(a)
	}
	return out
}
