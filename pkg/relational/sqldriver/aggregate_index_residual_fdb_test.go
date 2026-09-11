package sqldriver_test

// Rows behind TestAggregateIndexResidualOverGroupingColumns (embedded), RFC-248:
// a predicate on grouping columns the aggregate index's scan cannot bind is
// applied as a residual filter above the scan. The indexed schema's answer is
// compared with the unindexed schema's through DML that empties, revives and
// merges groups, for COUNT(*) (a plain scan) and SUM (the COUNT(*)-companion
// merge), over residual shapes that exercise the partition: a non-leading
// equality, a gap, a leading range (bound, no residual), a range then an
// equality, IS NULL, IN, a column-to-column residual, an OR, contradictory
// equalities, and ORDER BY through the residual. Every read is asserted, via
// the typed plan, to be served by the aggregate index — a read that fell back
// to a base scan would agree with the twin for a reason unrelated to this
// change.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_AggregateIndexResidual(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const table = "CREATE TABLE t (id BIGINT, a STRING, b STRING, c STRING, v BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
		"CREATE TABLE cust (id BIGINT, b STRING, region STRING, PRIMARY KEY (id)) "
	const indexes = "CREATE INDEX t_sum_abc AS SELECT SUM(v) FROM t GROUP BY a, b, c " +
		"CREATE INDEX t_cnt_abc AS SELECT COUNT(*) FROM t GROUP BY a, b, c " +
		"CREATE INDEX t_cnt_d_a AS SELECT COUNT(*) FROM t GROUP BY d, a "
	w := mmNewTwin(t, ctx, "/testdb_aggresidual", "aggresidual", table, indexes)

	as := []string{"'x'", "'y'", "'z'", "NULL"}
	bs := []string{"'p'", "'q'", "NULL"}
	cs := []string{"'x'", "'m'", "'z'"}
	ds := []string{"0.5", "1.5", "2.5", "NULL"}
	var rows []string
	for id := int64(0); id < 60; id++ {
		rows = append(rows, fmt.Sprintf("(%d, %s, %s, %s, %d, %s)", id,
			as[id%4], bs[(id/2)%3], cs[(id/3)%3], (id*7)%11-3, ds[(id/5)%4]))
	}
	w.Exec("INSERT INTO t (id, a, b, c, v, d) VALUES " + strings.Join(rows, ", "))
	w.Exec("INSERT INTO cust (id, b, region) VALUES (1, 'p', 'eu'), (2, 'q', 'us'), (3, NULL, 'us'), (4, 'zz', 'eu')")

	// wantResidual says which reads carry a residual filter above the aggregate
	// scan; the others bind everything they filter (a leading range, a leading
	// IS NULL). Asserted on the typed plan, never by matching SQL text.
	reads := []struct {
		sql          string
		wantResidual bool
	}{
		{"SELECT a, b, c, COUNT(*) FROM t WHERE b = 'p' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE b = 'p' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a > 'x' GROUP BY a, b, c ORDER BY a, b, c", false},
		{"SELECT a, b, c, SUM(v) FROM t WHERE a > 'x' GROUP BY a, b, c ORDER BY a, b, c", false},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a > 'x' AND b = 'q' GROUP BY a, b, c ORDER BY a, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a >= 'x' AND a < 'z' GROUP BY a, b, c ORDER BY a, b, c", false},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a IS NULL GROUP BY a, b, c ORDER BY b, c", false},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a IS NULL AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE b IS NULL GROUP BY a, b, c ORDER BY a, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE c IN ('m', 'z') GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = c GROUP BY a, b, c ORDER BY a, b", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE b = 'q' OR c = 'm' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND a = 'y' GROUP BY a, b, c", true},
		// An equality beside an inequality on the leading key: the equality
		// binds, the inequality is a residual over the group key.
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND a > 'm' GROUP BY a, b, c ORDER BY b, c", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE a > 'm' AND a = 'y' GROUP BY a, b, c ORDER BY b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY a DESC, b", true},
		{"SELECT d, a, COUNT(*) FROM t WHERE d > 1.0 GROUP BY d, a ORDER BY d, a", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE b = 'p' GROUP BY a, b, c HAVING COUNT(*) > 1 ORDER BY a, c", true},
		// A grouping-column leaf under a function wrapper stays wrapped after
		// the rewrite.
		{"SELECT a, b, c, COUNT(*) FROM t WHERE UPPER(b) = 'P' GROUP BY a, b, c ORDER BY a, c", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE LENGTH(c) = 1 AND a = 'y' GROUP BY a, b, c ORDER BY b, c", true},
		// Correlated: the outer row's b shares the grouping column's name and
		// must stay the outer read — rewritten by name it would compare the
		// group with itself and count every group for every customer.
		{"SELECT c.id, (SELECT COUNT(*) FROM t WHERE t.b = c.b AND t.a = 'x' AND t.c = 'z' GROUP BY t.a, t.b, t.c) FROM cust c ORDER BY c.id", true},
		{"SELECT c.id, (SELECT SUM(v) FROM t WHERE t.b = c.b AND t.a = 'y' AND t.c = 'm' GROUP BY t.a, t.b, t.c) FROM cust c ORDER BY c.id", true},
		// Two aggregates over two indexes: the residual sits above the
		// intersection (the third scan site).
		{"SELECT a, b, c, SUM(v), COUNT(*) FROM t WHERE b = 'p' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, SUM(v), COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
	}
	for _, r := range reads {
		plan, err := embedded.PlanPhysicalForTest(r.sql, table+indexes, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", r.sql, err)
		}
		if reached, _ := aggregateIndexAndSortIn(plan); !reached {
			t.Fatalf("read is not served by the aggregate index, so its rows prove nothing here\n  q: %s\n  plan: %s", r.sql, plan.Explain())
		}
		if residualFilterIn(plan) != r.wantResidual {
			t.Fatalf("residual filter above the aggregate scan = %v, want %v\n  q: %s\n  plan: %s", !r.wantResidual, r.wantResidual, r.sql, plan.Explain())
		}
	}
	sweep := func(stage string) {
		t.Helper()
		for _, r := range reads {
			q := r.sql
			gi, ei := mmRows(t, ctx, w.idx, q)
			gn, en := mmRows(t, ctx, w.plain, q)
			if ei != nil || en != nil {
				t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", stage, q, ei, en)
				continue
			}
			if !mmEqRows(gi, gn) {
				t.Errorf("%s: the residual-filtered aggregate index disagrees with the unindexed twin\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
					stage, q, gi, gn, w.Explain(q))
			}
		}
	}
	sweep("initial")
	for i, stmt := range []string{
		"UPDATE t SET b = 'p' WHERE a = 'z'",                                    // moves groups into the residual's selection
		"DELETE FROM t WHERE a = 'x' AND c = 'z'",                               // empties the gap arm's groups
		"INSERT INTO t (id, a, b, c, v, d) VALUES (100, 'x', 'q', 'z', 5, 1.5)", // revives one
		"UPDATE t SET c = 'x' WHERE b IS NULL",                                  // makes a = c true for more groups
		"UPDATE t SET v = 0 WHERE a > 'x'",                                      // SUM to zero under the range arm
		"DELETE FROM t WHERE d > 1.0",                                           // empties the double-range arm entirely
	} {
		w.Exec(stmt)
		sweep(fmt.Sprintf("after dml %d (%s)", i, stmt))
	}
}

// residualFilterIn reports whether a PredicatesFilter sits directly above an
// aggregate index plan or the merge/intersection it feeds.
func residualFilterIn(plan plans.RecordQueryPlan) bool {
	found := false
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if f, ok := p.(*plans.RecordQueryPredicatesFilterPlan); ok && len(f.GetChildren()) == 1 {
			switch f.GetChildren()[0].(type) {
			case *plans.RecordQueryAggregateIndexPlan, *plans.RecordQueryMultiIntersectionOnValuesPlan:
				found = true
			}
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return found
}
