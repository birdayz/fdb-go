package sqldriver_test

// Probes BOOLEAN column semantics + indexing: equality to true/false, bare
// boolean predicate, NOT, IS NULL, and ORDER BY over a boolean (false<true in
// tuple encoding; NULL smallest).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_BooleanIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_bool")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_bool")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE booltbl "+
			"CREATE TABLE t (id BIGINT NOT NULL, flag BOOLEAN, PRIMARY KEY (id)) "+
			"CREATE INDEX t_flag ON t (flag)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_bool/s WITH TEMPLATE booltbl")
	dsn := fmt.Sprintf("fdbsql:///testdb_bool?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, flag) VALUES (1, true), (2, false)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (3)") // flag NULL

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

	ck("eq_true", "SELECT id FROM t WHERE flag = true", []int64{1})
	ck("eq_false", "SELECT id FROM t WHERE flag = false", []int64{2})
	ck("bare_pred", "SELECT id FROM t WHERE flag", []int64{1})
	ck("not_pred", "SELECT id FROM t WHERE NOT flag", []int64{2})
	ck("is_null", "SELECT id FROM t WHERE flag IS NULL", []int64{3})
	ck("is_not_null", "SELECT id FROM t WHERE flag IS NOT NULL", []int64{1, 2})
}
