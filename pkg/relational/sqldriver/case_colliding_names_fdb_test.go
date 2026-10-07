package sqldriver_test

// Names that differ only in case are distinct, as in Java: quoted columns
// "COLUMN", "column" and "cOLumN" in one table each keep their own value
// (an INSERT used to fold them onto the last), and tables "Table1" and
// "TaBlE1" each resolve to themselves (a scan used to fold both onto the
// ambiguous TABLE1). Java corpus keyword-case-insensitivity.yamsql and
// case-sensitivity.yamsql.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func TestFDB_CaseCollidingNames(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_case_colliding")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_case_colliding")
	mustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE case_colliding_tmpl `+
		`create table t2(id bigint, "COLUMN" string, "column" string, "cOLumN" string, primary key(id)) `+
		`create table "Table1"("id" bigint, "col1" bigint, "col2" bigint, primary key("id")) `+
		`create table "TaBlE1"("x" bigint, "y" string, primary key("x")) `+
		`create index "i1" as select "col2", "col1" from "Table1" order by "col2", "col1"`)
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_case_colliding/s WITH TEMPLATE case_colliding_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_CASE_COLLIDING?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, ctx, `insert into t2 values (1, 'a_UP', 'a_lo', 'a_Mx')`)
	mustExec(t, db, ctx, `insert into t2(id, "column") values (2, 'only_lo')`)
	mustExec(t, db, ctx, `insert into "Table1" values (1, 10, 1)`)
	mustExec(t, db, ctx, `insert into "TaBlE1" values (1, 'foo')`)

	for _, tc := range []struct {
		query string
		want  [][]any
	}{
		{`select "COLUMN", "column", "cOLumN" from t2 where id = 1`, [][]any{{"a_UP", "a_lo", "a_Mx"}}},
		{`select * from t2 where id = 2`, [][]any{{int64(2), nil, "only_lo", nil}}},
		{`select * from "Table1"`, [][]any{{int64(1), int64(10), int64(1)}}},
		{`select * from "TaBlE1"`, [][]any{{int64(1), "foo"}}},
		{`select "col2" from "Table1" order by "col2"`, [][]any{{int64(1)}}},
		{`select "Table1"."id", "TaBlE1"."y" from "Table1", "TaBlE1"`, [][]any{{int64(1), "foo"}}},
	} {
		rows, err := db.QueryContext(ctx, tc.query)
		if err != nil {
			t.Errorf("%s: %v", tc.query, err)
			continue
		}
		cols, _ := rows.Columns()
		var got [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("%s: scan: %v", tc.query, err)
			}
			got = append(got, vals)
		}
		if err := rows.Err(); err != nil {
			t.Errorf("%s: %v", tc.query, err)
		}
		rows.Close()
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %v\nwant %v", tc.query, got, tc.want)
		}
	}
}
