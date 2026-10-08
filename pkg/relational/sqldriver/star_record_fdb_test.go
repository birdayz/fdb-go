package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// `(*)` and `(T.*)` pack the star expansion into one record column, named as
// Java's visitRecordConstructor names it.
func TestFDB_StarRecordConstructor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_starrec")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_starrec")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE starrec_tpl "+
		"CREATE TABLE foo (id BIGINT, val BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE bar (bid BIGINT, name STRING, PRIMARY KEY (bid))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_starrec/s WITH TEMPLATE starrec_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_STARREC?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO foo VALUES (1, 10)")
	testkit.MustExec(t, db, ctx, "INSERT INTO bar VALUES (1, 'a')")

	for q, want := range map[string]string{
		"SELECT (*) FROM foo":                                                 "FOO={ID:1 VAL:10}",
		"SELECT (*) FROM foo AS f":                                            "F={ID:1 VAL:10}",
		"SELECT (foo.*) FROM foo, bar":                                        "FOO={ID:1 VAL:10}",
		"SELECT (*) AS r FROM foo":                                            "R={ID:1 VAL:10}",
		"SELECT (*) FROM foo, bar WHERE id = bid":                             "_0={ID:1 VAL:10 BID:1 NAME:a}",
		"SELECT (*) FROM (SELECT foo.val AS v, bar.bid AS v FROM foo, bar) X": "X={V:10 V_2:1}",
		"SELECT (*) FROM VALUES (1, 2) AS X(A, B)":                            "X={A:1 B:2}",
		"SELECT (*) FROM VALUES (1, 2)":                                       "_0={_0:1 _1:2}",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		cols, _ := rows.Columns()
		var got []string
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			s, ok := v.(api.Struct)
			if !ok {
				got = append(got, fmt.Sprintf("%T %v", v, v))
				continue
			}
			var parts []string
			for i := 1; i <= s.AttributeCount(); i++ {
				name, _ := s.MetaData().AttributeName(i)
				a, _ := s.Attribute(i)
				parts = append(parts, fmt.Sprintf("%s:%v", name, a))
			}
			got = append(got, fmt.Sprintf("%s={%s}", strings.Join(cols, ","), strings.Join(parts, " ")))
		}
		rows.Close()
		sort.Strings(got)
		if strings.Join(got, ";") != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}
}
