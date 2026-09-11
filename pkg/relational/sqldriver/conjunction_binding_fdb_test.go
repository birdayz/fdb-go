package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// conjunctionScanShape reads, off the typed plan, the facts a conjunction's
// binding is about: how many comparisons every scan range carries in total,
// how many residual predicates sit on filters, and how many IN-join /
// IN-union nodes there are. It is what lets a rows check refuse a plan that
// returns the right rows through a residual over a half-bounded scan.
type conjunctionScanShape struct {
	comparisons int
	residuals   int
	inNodes     int
}

func conjunctionScanShapeOf(plan plans.RecordQueryPlan) conjunctionScanShape {
	var shape conjunctionScanShape
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		switch n := p.(type) {
		case *plans.RecordQueryIndexPlan:
			for _, cr := range n.GetScanComparisons() {
				shape.comparisons += len(cr.GetComparisons())
			}
		case *plans.RecordQueryCoveringIndexPlan:
			walk(n.GetIndexPlan())
			return
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			walk(n.GetInner())
			return
		case *plans.RecordQueryScanPlan:
			for _, cr := range n.GetScanComparisons() {
				shape.comparisons += len(cr.GetComparisons())
			}
		case *plans.RecordQueryPredicatesFilterPlan:
			shape.residuals += len(n.GetPredicates())
		case *plans.RecordQueryInJoinPlan, *plans.RecordQueryInUnionPlan:
			shape.inNodes++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return shape
}

// TestFDB_ConjunctionBinding runs every shape of embedded.TestConjunctionBinding
// against real FDB with an in-Go oracle, and pins the plan shape beside the
// rows: a two-sided range must carry BOTH bounds in the scan and no residual,
// an equality beside an inequality must bind the equality and re-apply the
// inequality, and an IN beside other conjuncts must explode into an IN-join
// or IN-union. A regression to "correct rows through a residual over a
// half-bounded scan" fails the shape half; a regression in the fold's
// residual bookkeeping fails the rows half.
func TestFDB_ConjunctionBinding(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_conjbind")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_conjbind")
	const table = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, v BIGINT, PRIMARY KEY (id)) "
	const indexes = "CREATE INDEX idx_a ON t (a) CREATE INDEX idx_ab ON t (a, b)"
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE conjbind "+table+indexes)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_conjbind/s WITH TEMPLATE conjbind")
	dsn := fmt.Sprintf("fdbsql:///testdb_conjbind?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rows := oracleGenRows(300)
	for i := 0; i < len(rows); i += 50 {
		end := i + 50
		if end > len(rows) {
			end = len(rows)
		}
		var parts []string
		for _, r := range rows[i:end] {
			parts = append(parts, r.insertSQL())
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, c, s, v) VALUES "+strings.Join(parts, ","))
	}
	planSchema := "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, v BIGINT, PRIMARY KEY (id))\n" +
		"CREATE INDEX idx_a ON T(a)\nCREATE INDEX idx_ab ON T(a, b)"

	cases := []struct {
		name  string
		sql   string
		pred  func(oracleRow) bool
		shape conjunctionScanShape
	}{
		{"two_sided_range", "SELECT id FROM t WHERE a >= 2 AND a < 5", func(r oracleRow) bool { return oracleGe(r.a, 2) && oracleLt(r.a, 5) }, conjunctionScanShape{comparisons: 2}},
		{"two_sided_range_reversed", "SELECT id FROM t WHERE a < 5 AND a >= 2", func(r oracleRow) bool { return oracleGe(r.a, 2) && oracleLt(r.a, 5) }, conjunctionScanShape{comparisons: 2}},
		{"two_lower_bounds", "SELECT id FROM t WHERE a > 2 AND a > 3", func(r oracleRow) bool { return oracleGt(r.a, 3) }, conjunctionScanShape{comparisons: 2}},
		{"between_on_second_column", "SELECT id FROM t WHERE a = 1 AND b BETWEEN 2 AND 5", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleGe(r.b, 2) && oracleLe(r.b, 5) }, conjunctionScanShape{comparisons: 3}},
		{"range_then_equality_residual", "SELECT id FROM t WHERE a > 2 AND a < 5 AND b = 3", func(r oracleRow) bool { return oracleGt(r.a, 2) && oracleLt(r.a, 5) && oracleEq(r.b, 3) }, conjunctionScanShape{comparisons: 2, residuals: 1}},
		{"equality_beside_inequality", "SELECT id FROM t WHERE a = 1 AND a > 0", func(r oracleRow) bool { return oracleEq(r.a, 1) }, conjunctionScanShape{comparisons: 1, residuals: 1}},
		{"equality_beside_excluding_inequality", "SELECT id FROM t WHERE a = 1 AND a > 3", func(r oracleRow) bool { return false }, conjunctionScanShape{comparisons: 1, residuals: 1}},
		{"contradictory_equalities", "SELECT id FROM t WHERE a = 1 AND a = 2", func(r oracleRow) bool { return false }, conjunctionScanShape{comparisons: 1, residuals: 1}},
		{"duplicate_equality", "SELECT id FROM t WHERE a = 1 AND a = 1", func(r oracleRow) bool { return oracleEq(r.a, 1) }, conjunctionScanShape{comparisons: 1}},
		{"empty_two_sided_range", "SELECT id FROM t WHERE a > 5 AND a < 3", func(r oracleRow) bool { return false }, conjunctionScanShape{comparisons: 2}},
		{"in_with_unindexed_residual", "SELECT id FROM t WHERE a IN (1, 2) AND v = 3", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleEq(r.v, 3) }, conjunctionScanShape{comparisons: 1, residuals: 1, inNodes: 1}},
		{"in_with_second_column_bound", "SELECT id FROM t WHERE a IN (1, 2) AND b = 3", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleEq(r.b, 3) }, conjunctionScanShape{comparisons: 2, inNodes: 1}},
		{"in_on_pk_with_residual", "SELECT id FROM t WHERE id IN (1, 2, 300) AND b > 2", func(r oracleRow) bool { return oracleIn(oracleInt(r.id), 1, 2, 300) && oracleGt(r.b, 2) }, conjunctionScanShape{comparisons: 1, residuals: 1, inNodes: 1}},
		{"nested_ins", "SELECT id FROM t WHERE a IN (1, 2) AND b IN (5, 4)", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleIn(r.b, 5, 4) }, conjunctionScanShape{comparisons: 2, inNodes: 2}},
		{"in_beside_a_range_on_the_same_column", "SELECT id FROM t WHERE a IN (1, 2, 3) AND a > 1", func(r oracleRow) bool { return oracleIn(r.a, 2, 3) }, conjunctionScanShape{comparisons: 1, residuals: 1, inNodes: 1}},
		{"in_on_the_second_column", "SELECT id FROM t WHERE a = 1 AND b IN (5, 4)", func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleIn(r.b, 5, 4) }, conjunctionScanShape{comparisons: 2, inNodes: 1}},
		{"in_with_range_on_second_column", "SELECT id FROM t WHERE a IN (1, 2) AND b > 3 AND b <= 5", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && oracleGt(r.b, 3) && oracleLe(r.b, 5) }, conjunctionScanShape{comparisons: 3, inNodes: 1}},
		{"in_with_null_column_residual", "SELECT id FROM t WHERE a IN (1, 2) AND c IS NULL", func(r oracleRow) bool { return oracleIn(r.a, 1, 2) && r.c == nil }, conjunctionScanShape{comparisons: 1, residuals: 1, inNodes: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := embedded.PlanPhysicalForTest(tc.sql, planSchema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got := conjunctionScanShapeOf(plan); got != tc.shape {
				t.Fatalf("plan shape = %+v, want %+v\n  sql:  %s\n  plan: %s", got, tc.shape, tc.sql, plan.Explain())
			}
			rs, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("query %q: %v", tc.sql, err)
			}
			defer rs.Close()
			got := map[int64]int{}
			for rs.Next() {
				var v int64
				if err := rs.Scan(&v); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got[v]++
			}
			if err := rs.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			want := map[int64]int{}
			for _, r := range rows {
				if tc.pred(r) {
					want[r.id]++
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("rows mismatch\n  sql:  %s\n  plan: %s\n  got:  %v\n  want: %v", tc.sql, plan.Explain(), got, want)
			}
			if len(want) == 0 {
				t.Logf("%s: empty by construction, plan %s", tc.name, plan.Explain())
			}
		})
	}
}
