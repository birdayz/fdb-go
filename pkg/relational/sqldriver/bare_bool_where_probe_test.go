package sqldriver_test

// Pins bare boolean WHERE forms — boolean literal (WHERE TRUE / WHERE FALSE) and
// boolean column (WHERE flag / WHERE NOT flag), incl. combination with column
// predicates. Java 4.12 plans these; Go supports them too (DIVERGENCES.md's old
// `bare_bool_where_rejected` Go-gap note is stale — the literal forms work). The
// existing corpus `bare_bool_where` covers only `WHERE flag`; this adds the
// literal forms.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_BareBoolWhereProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_barebw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_barebw")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE barebw "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, flag BOOLEAN, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_barebw/s WITH TEMPLATE barebw")
	dsn := fmt.Sprintf("fdbsql:///testdb_barebw?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, flag) VALUES (1,1,true),(2,2,false),(3,3,true)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	all := []int64{1, 2, 3}
	ck("where_true", "SELECT id FROM t WHERE TRUE", all)
	ck("where_false", "SELECT id FROM t WHERE FALSE", nil)
	ck("true_and_col", "SELECT id FROM t WHERE TRUE AND a = 2", []int64{2})
	ck("false_or_col", "SELECT id FROM t WHERE FALSE OR a = 2", []int64{2})
	ck("where_bool_col", "SELECT id FROM t WHERE flag", []int64{1, 3})
	ck("where_not_bool_col", "SELECT id FROM t WHERE NOT flag", []int64{2})
}
