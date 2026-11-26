package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A row constructor IN an array of structs matches element-wise by ordinal.
func TestFDB_RecordInStructArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_recin")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_recin")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE recin create type as struct fruit_type(name string, color string) "+
		"create table array_table(id bigint, fruit_records fruit_type array, primary key(id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_recin/s WITH TEMPLATE recin")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_RECIN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO array_table VALUES (1, [('apple' as name, 'red' as color), ('banana' as name, 'yellow' as color)]), "+
		"(2, [('grape' as name, 'purple' as color)]), (3, [('mango' as name, 'orange' as color), ('apple' as name, 'green' as color)])")
	for q, want := range map[string]string{
		"select id from array_table where ('apple', 'red') in fruit_records":    "[1]",
		"select id from array_table where ('apple', 'green') in fruit_records":  "[3]",
		"select id from array_table where ('red', 'apple') in fruit_records":    "[]",
		"select id from array_table where ('grape', 'purple') in fruit_records": "[2]",
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
		rows.Close()
		if fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}
}
