package sqldriver_test

// Probes SQL three-valued logic (NULL → UNKNOWN) through NOT / AND / OR
// predicates. The subtle cases: `<>`/`NOT(=)` exclude NULL rows (UNKNOWN, not
// TRUE); `UNKNOWN AND FALSE = FALSE` (so NOT can still include such a row);
// `UNKNOWN OR TRUE = TRUE`. These are a classic wrong-rows source.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_ThreeValuedLogicNotAndNeq(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_3vlnn")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_3vlnn")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE tvlnn "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3vlnn/s WITH TEMPLATE tvlnn")
	dsn := fmt.Sprintf("fdbsql:///testdb_3vlnn?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id1(a=1,b=1) id2(a=1,b=NULL) id3(a=NULL,b=2) id4(a=2,b=2) id5(a=NULL,b=NULL)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,1,1),(4,2,2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a) VALUES (2,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,b) VALUES (3,2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (5)")

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

	ck("eq", "SELECT id FROM t WHERE a = 1", []int64{1, 2})
	ck("neq_excludes_null", "SELECT id FROM t WHERE a <> 1", []int64{4})
	ck("not_eq_excludes_null", "SELECT id FROM t WHERE NOT (a = 1)", []int64{4})
	ck("or_true_absorbs_unknown", "SELECT id FROM t WHERE a = 1 OR b = 2", []int64{1, 2, 3, 4})
	ck("and_unknown", "SELECT id FROM t WHERE a = 1 AND b = 1", []int64{1})
	ck("and_with_isnull", "SELECT id FROM t WHERE a = 1 AND b IS NULL", []int64{2})
	// NOT(a=1 AND b=1): UNKNOWN AND FALSE = FALSE → NOT = TRUE, so id3 & id4 included.
	ck("not_and_false_absorbs", "SELECT id FROM t WHERE NOT (a = 1 AND b = 1)", []int64{3, 4})
	// a<>1 OR b<>2: id1 (b<>2 true), id4 (a<>1 true). Others UNKNOWN.
	ck("neq_or_neq", "SELECT id FROM t WHERE a <> 1 OR b <> 2", []int64{1, 4})
	// IS NULL / IS NOT NULL.
	ck("a_is_null", "SELECT id FROM t WHERE a IS NULL", []int64{3, 5})
	ck("a_is_not_null", "SELECT id FROM t WHERE a IS NOT NULL", []int64{1, 2, 4})
	// NULL-safe equality: a IS NOT DISTINCT FROM NULL → rows where a IS NULL.
	ck("not_distinct_from_null", "SELECT id FROM t WHERE a IS NOT DISTINCT FROM NULL", []int64{3, 5})
}
