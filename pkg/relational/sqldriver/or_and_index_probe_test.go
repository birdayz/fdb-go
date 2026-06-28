package sqldriver_test

// Probes OR / AND across two independently-indexed columns (the planner's
// index-union for OR and intersection for AND): two-column OR, AND, OR across the
// same column, three-way OR mixing indexed columns and the PK, and a parenthesized
// (OR) AND combination. All return the correct row sets.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_OrAndIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_oai")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_oai")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE oai "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_b ON t (b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_oai/s WITH TEMPLATE oai")
	dsn := fmt.Sprintf("fdbsql:///testdb_oai?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,1,9),(2,9,2),(3,1,2),(4,9,9)")

	ids := func(where string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("WHERE %s: %v", where, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
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
	ck := func(name, where string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(where); !eq(got, want) {
				t.Errorf("WHERE %s = %v, want %v", where, got, want)
			}
		})
	}

	ck("or_two_columns", "a = 1 OR b = 2", []int64{1, 2, 3})
	ck("and_two_columns", "a = 1 AND b = 2", []int64{3})
	ck("or_same_column", "a = 1 OR a = 9", []int64{1, 2, 3, 4})
	ck("and_two_columns_other", "a = 9 AND b = 9", []int64{4})
	ck("three_way_or_with_pk", "a = 1 OR b = 2 OR id = 4", []int64{1, 2, 3, 4})
	ck("paren_or_and", "(a = 1 OR a = 9) AND b = 2", []int64{2, 3})
}
