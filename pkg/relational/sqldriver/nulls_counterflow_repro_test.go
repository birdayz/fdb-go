package sqldriver_test

// RFC-165 reproducer matrix: counterflow NULL placement (ASC NULLS LAST /
// DESC NULLS FIRST) across the planner paths that consume a RequestedSortOrder
// (plain elision, IN-union, GROUP BY ordering, DISTINCT, data-access).
//
// The load-bearing pin is groupby_asc_nulls_last: it is RED on master
// (171b1021) and GREEN after the fix. There, the StreamingAgg provides an
// ASC_NULLS_FIRST ordering on the grouping key and RemoveSortRule elided the
// outer `ORDER BY a ASC NULLS LAST` against it (master plan
// `StreamingAgg(keys=[A], IndexScan(T_A COVERING))`), returning NULLs first.
// The plain indexed case is NOT a reproducer — OrderedIndexScanRule already
// refuses a counterflow-ordered index scan — but is kept here as a guard.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NullsCounterflowRepro(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cflow")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cflow")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE cflow "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cflow/s WITH TEMPLATE cflow")
	dsn := fmt.Sprintf("fdbsql:///testdb_cflow?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 10), (3, 20), (5, 5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (2), (4)")

	orderedA := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var a sql.NullInt64
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if a.Valid {
				out = append(out, a.Int64)
			} else {
				out = append(out, -1)
			}
		}
		return out
	}
	explain := func(q string) string {
		rows, err := db.QueryContext(ctx, "EXPLAIN "+q)
		if err != nil {
			return "ERR:" + err.Error()
		}
		defer rows.Close()
		var plan string
		if rows.Next() {
			_ = rows.Scan(&plan)
		}
		return plan
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
	check := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			got := orderedA(q)
			if !eq(got, want) {
				t.Errorf("%s\n  query: %s\n  plan:  %s\n  got:  %v\n  want: %v", name, q, explain(q), got, want)
			}
		})
	}

	// a in data: {5,10,20} + two NULLs. Counterflow ASC NULLS LAST -> nulls last.
	check("plain_asc_nulls_last", "SELECT a FROM t ORDER BY a ASC NULLS LAST",
		[]int64{5, 10, 20, -1, -1})
	check("plain_desc_nulls_first", "SELECT a FROM t ORDER BY a DESC NULLS FIRST",
		[]int64{-1, -1, 20, 10, 5})
	check("dataaccess_asc_nulls_last", "SELECT a FROM t WHERE id > 0 ORDER BY a ASC NULLS LAST",
		[]int64{5, 10, 20, -1, -1})
	check("in_list_asc_nulls_last", "SELECT a FROM t WHERE a IN (5, 10, 20) ORDER BY a ASC NULLS LAST",
		[]int64{5, 10, 20})
	check("in_list_desc_nulls_first", "SELECT a FROM t WHERE a IN (5, 10, 20) ORDER BY a DESC NULLS FIRST",
		[]int64{20, 10, 5})
	check("groupby_asc_nulls_last", "SELECT a FROM t GROUP BY a ORDER BY a ASC NULLS LAST",
		[]int64{5, 10, 20, -1})
	check("distinct_asc_nulls_last", "SELECT DISTINCT a FROM t ORDER BY a ASC NULLS LAST",
		[]int64{5, 10, 20, -1})

	// review review catch (RFC-165 impl): a materialized counterflow sort must
	// advertise its NULL placement, else a PARENT sort elides against it as if
	// natural. Inner produces ASC NULLS LAST = [5,10,20,NULL] (LIMIT 4 keeps the
	// inner ordering live); the outer ORDER BY a ASC (= NULLS FIRST) must re-sort
	// to [NULL,5,10,20]. Before carrying NullsFirst through properties.Ordering,
	// the outer sort was elided and this returned [5,10,20,NULL]. The outer
	// InMemorySort must be retained (EXPLAIN: nested InMemorySort).
	check("nested_counterflow_outer_resort",
		"SELECT a FROM (SELECT a FROM t GROUP BY a ORDER BY a ASC NULLS LAST LIMIT 4) s ORDER BY a ASC",
		[]int64{-1, 5, 10, 20})
}
