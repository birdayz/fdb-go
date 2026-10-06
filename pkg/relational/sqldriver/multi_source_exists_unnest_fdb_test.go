package sqldriver_test

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
)

// An existential's child WHERE must read the outer element before FirstOrDefault,
// including when the child is a join rather than a scan.
func TestFDB_MultiSourceExistsReadsUnnestElement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const dbPath = "/FRL/multi_source_exists_unnest"
	setup := openTestDB(t, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE multi_source_exists_unnest_tmpl" +
			" CREATE TYPE AS STRUCT elem (k BIGINT, tags BIGINT ARRAY)" +
			" CREATE TABLE t (id BIGINT, arr BIGINT ARRAY, bs elem ARRAY, PRIMARY KEY (id))" +
			" CREATE TABLE u (id BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE v (id BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE multi_source_exists_unnest_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO t VALUES (1, [1, 2, 2], [(1, [1, 2]), (2, [3])]), (2, [3, 4], [(3, [2, 4])]), (3, [], []), (4, NULL, NULL)",
		"INSERT INTO u VALUES (0), (1), (2), (3)",
		"INSERT INTO v VALUES (0), (2), (3)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	const from = `SELECT o FROM t, t.arr AS o WHERE `
	const child = `SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE `
	for _, tc := range []struct {
		name, sql string
		want      []string
	}{
		{"scalar", from + `EXISTS (` + child + `A.id = o)`, []string{"2", "2", "3"}},
		{"not_exists", from + `NOT EXISTS (` + child + `A.id = o)`, []string{"1", "4"}},
		{"quoted_alias", `SELECT "a" FROM t, t.arr AS "a" WHERE EXISTS (` + child + `A.id = "a")`, []string{"2", "2", "3"}},
		{"quoted_inner", from + `EXISTS (SELECT 1 FROM u AS "o" JOIN v AS B ON "o".id = B.id WHERE "o".id = o)`, []string{"2", "2", "3"}},
		{"on_correlation", from + `EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id AND B.id = o)`, []string{"2", "2", "3"}},
		{"outer_only", from + `EXISTS (` + child + `o = 2)`, []string{"2", "2"}},
		{"table_and_element", from + `EXISTS (` + child + `A.id = o AND B.id = t.id + 1)`, []string{"2", "2", "3"}},
		{"third_inner", from + `EXISTS (SELECT 1 FROM u A JOIN v B ON A.id = B.id JOIN u C ON C.id = B.id WHERE C.id = o)`, []string{"2", "2", "3"}},
		{"second_exists", from + `EXISTS (` + child + `A.id = o) AND NOT EXISTS (` + child + `A.id = o + 1)`, []string{"3"}},
		{"independent", from + `EXISTS (` + child + `A.id = 2)`, []string{"1", "2", "2", "3", "4"}},
		{"empty_inner", from + `EXISTS (` + child + `A.id = o AND B.id = 99)`, nil},
		{"ordinal", `SELECT o, p FROM t, t.arr AS o AT p WHERE EXISTS (` + child + `A.id = p)`, []string{"2|2", "2|3", "4|2"}},
		{"element_and_ordinal", `SELECT o, p FROM t, t.arr AS o AT p WHERE EXISTS (` + child + `A.id = o AND B.id = p)`, []string{"2|2"}},
		{"record_element", `SELECT x.k FROM t, t.bs AS x WHERE EXISTS (` + child + `A.id = x.k)`, []string{"2", "3"}},
		{"record_element_at", `SELECT x.k, p FROM t, t.bs AS x AT p WHERE EXISTS (` + child + `A.id = x.k AND B.id = p)`, []string{"2|2"}},
		{"spine_tip", `SELECT o FROM t, t.bs AS x, x.tags AS o WHERE EXISTS (` + child + `A.id = o)`, []string{"2", "2", "3"}},
		{"spine_both_links", `SELECT o FROM t, t.bs AS x, x.tags AS o WHERE EXISTS (` + child + `A.id = o AND B.id = x.k - 1)`, []string{"2"}},
		{"spine_at", `SELECT o, p FROM t, t.bs AS x, x.tags AS o AT p WHERE EXISTS (` + child + `A.id = p)`, []string{"2|2", "4|2"}},
		{"nested_exists", from + `EXISTS (` + child + `A.id = o AND EXISTS (SELECT 1 FROM v C WHERE C.id = o))`, []string{"2", "2", "3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pinRows(t, db, ctx, tc.sql)
			sort.Strings(got)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("%s: rows = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}
