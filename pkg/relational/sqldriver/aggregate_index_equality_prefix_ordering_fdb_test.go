package sqldriver_test

// Rows behind TestAggregateIndexEqualityPrefixElidesSort (embedded): once the
// planner stops sorting `WHERE b = 1 GROUP BY b, a ORDER BY a` in memory, the
// ORDER the aggregate index delivers is the answer — so the indexed schema's
// sequence is compared with the unindexed schema's (which sorts) exactly, NULL
// groups included (NULL sorts first ascending), through DML that adds, empties
// and revives groups, and for a DOUBLE prefix pinned to one physical key.
//
// The plan-shape half lives in the embedded package; this half asserts the
// aggregate index is reached (via the typed plan, never EXPLAIN text) so a
// green here is a statement about the index's order and not about a fallback.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_AggregateIndexEqualityPrefixOrdering(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const table = "CREATE TABLE t (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, d DOUBLE, PRIMARY KEY (pk1, pk2)) "
	const indexes = "CREATE INDEX t_cnt_b_a AS SELECT COUNT(*) FROM t GROUP BY b, a " +
		"CREATE INDEX t_max_b_a AS SELECT MAX(pk2) FROM t GROUP BY b, a " +
		"CREATE INDEX t_cnt_pk1_a AS SELECT COUNT(*) FROM t GROUP BY pk1, a " +
		"CREATE INDEX t_cnt_d_a AS SELECT COUNT(*) FROM t GROUP BY d, a "
	w := mmNewTwin(t, ctx, "/testdb_aggprefixord", "aggprefixord", table, indexes)

	var rows []string
	for pk1 := int64(0); pk1 < 6; pk1++ {
		for pk2 := int64(0); pk2 < 6; pk2++ {
			// a cycles through NULL and 0..4 out of key order so the index's
			// group order differs from insertion order.
			a := "NULL"
			if v := (pk1*7 + pk2*3) % 6; v != 0 {
				a = fmt.Sprintf("%d", 5-v)
			}
			b := fmt.Sprintf("%d", (pk1+pk2)%3)
			d := []string{"1.0", "0.5", "NULL", "2.0"}[(pk1*pk2)%4]
			rows = append(rows, fmt.Sprintf("(%d, %d, %s, %s, %s)", pk1, pk2, a, b, d))
		}
	}
	w.Exec("INSERT INTO t (pk1, pk2, a, b, d) VALUES " + strings.Join(rows, ", "))

	reads := []string{
		"SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a",
		"SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY b, a",
		"SELECT b, a, COUNT(*) FROM t WHERE b = 2 GROUP BY b, a ORDER BY a NULLS FIRST",
		"SELECT b, a, MAX(pk2) FROM t WHERE b = 0 GROUP BY b, a ORDER BY a",
		"SELECT pk1, a, COUNT(*) FROM t WHERE pk1 = 3 GROUP BY pk1, a ORDER BY a",
		"SELECT d, a, COUNT(*) FROM t WHERE d = 1.0 GROUP BY d, a ORDER BY a",
		"SELECT d, a, COUNT(*) FROM t WHERE d = 0.5 GROUP BY d, a ORDER BY a",
		"SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a LIMIT 3",
		"SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a LIMIT 2 OFFSET 1",
		"SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a HAVING COUNT(*) > 1 ORDER BY a",
	}
	// The whole point: every read must be answered BY the aggregate index. A
	// read that fell back to a scan-and-sort would agree with the oracle for a
	// reason unrelated to this change.
	for _, q := range reads {
		plan, err := embedded.PlanPhysicalForTest(q, table+indexes, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", q, err)
		}
		if !reachesAggregateIndex(plan) {
			t.Fatalf("read is not served by the aggregate index, so its order proves nothing here\n  q: %s\n  plan: %s", q, plan.Explain())
		}
	}
	sweep := func(stage string) {
		t.Helper()
		for _, q := range reads {
			gi, ei := mmRows(t, ctx, w.idx, q)
			gn, en := mmRows(t, ctx, w.plain, q)
			if ei != nil || en != nil {
				t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", stage, q, ei, en)
				continue
			}
			// SEQUENCE equality: the ORDER BY key is the only free grouping
			// column, so the order is total over the groups and the indexed
			// side has no sort of its own to fall back on.
			if !mmEqRows(gi, gn) {
				t.Errorf("%s: the aggregate index's group order disagrees with the sorted oracle\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
					stage, q, gi, gn, w.Explain(q))
			}
		}
	}
	sweep("initial")
	for i, stmt := range []string{
		"UPDATE t SET a = 9 WHERE pk1 = 1 AND pk2 = 2",                                   // a new, largest group under b = 0
		"UPDATE t SET a = NULL WHERE b = 1 AND a = 4",                                    // moves rows into the NULL group
		"DELETE FROM t WHERE b = 1 AND a = 3",                                            // empties a group under b = 1
		"INSERT INTO t (pk1, pk2, a, b, d) VALUES (7, 7, 3, 1, 1.0), (7, 8, -1, 1, 0.5)", // revives it, adds a smallest group
		"UPDATE t SET b = 1 WHERE b = 2",                                                 // merges b = 2's groups into b = 1
		"DELETE FROM t WHERE pk1 = 3",                                                    // empties every pk1 = 3 group
	} {
		w.Exec(stmt)
		sweep(fmt.Sprintf("after dml %d (%s)", i, stmt))
	}
}

func reachesAggregateIndex(plan plans.RecordQueryPlan) bool {
	found := false
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if _, ok := p.(*plans.RecordQueryAggregateIndexPlan); ok {
			found = true
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return found
}
