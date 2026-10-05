package sqldriver_test

// Rows behind TestAggregateIndexReverseScan and TestAggregateMergeKeepsFixedPrefixOrdering
// (embedded): a descending request
// over an aggregate index's groups is served by the reverse scan, the
// group-existence merge and the multi-aggregate intersection running their
// legs in reverse. The indexed schema answers what the unindexed one answers,
// in the requested order, through DML that empties, revives and NULLs groups
// (the merge's outer arms in descending key order).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_AggregateIndexReverseScan(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const table = "CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, col3 BIGINT, PRIMARY KEY (id)) "
	const indexes = "CREATE INDEX mv_sum AS SELECT SUM(col2) FROM t1 GROUP BY col1 " +
		"CREATE INDEX mv_cnt AS SELECT COUNT(*) FROM t1 GROUP BY col1 " +
		"CREATE INDEX mv_cntv AS SELECT COUNT(col2) FROM t1 GROUP BY col1 " +
		"CREATE INDEX mv_cnt13 AS SELECT COUNT(*) FROM t1 GROUP BY col1, col3 " +
		"CREATE INDEX mv_sum13 AS SELECT SUM(col2) FROM t1 GROUP BY col1, col3 " +
		"CREATE INDEX mv_cntv13 AS SELECT COUNT(col2) FROM t1 GROUP BY col1, col3 "
	w := mmNewTwin(t, ctx, "/testdb_aggreverse", "aggreverse", table, indexes)

	var rows []string
	for id := int64(0); id < 40; id++ {
		col1 := fmt.Sprint(id % 7)
		if id%9 == 4 {
			col1 = "NULL"
		}
		col2 := fmt.Sprint((id*3)%5 - 2)
		if id%7 == 6 {
			col2 = "NULL" // group 6 holds no non-NULL col2: SUM is NULL
		}
		rows = append(rows, fmt.Sprintf("(%d, %s, %s, %d)", id, col1, col2, id%3))
	}
	w.Exec("INSERT INTO t1 (id, col1, col2, col3) VALUES " + strings.Join(rows, ", "))

	reads := []struct {
		sql     string
		reverse int // reverse aggregate scans and merges in the plan
	}{
		{"SELECT col1, COUNT(*) FROM t1 GROUP BY col1 ORDER BY col1 DESC", 1},
		{"SELECT col1, SUM(col2) FROM t1 GROUP BY col1 ORDER BY col1 DESC", 1},
		{"SELECT col1, SUM(col2), COUNT(*) FROM t1 GROUP BY col1 ORDER BY col1 DESC", 3},
		{"SELECT col1, col3, COUNT(*) FROM t1 WHERE col1 = 2 GROUP BY col1, col3 ORDER BY col3 DESC", 1},
		{"SELECT col1, col3, COUNT(*) FROM t1 GROUP BY col1, col3 ORDER BY col1 DESC, col3 DESC", 1},
		// The merge keeps its legs' fixed col1 (TestAggregateMergeKeepsFixedPrefixOrdering).
		{"SELECT col1, col3, SUM(col2) FROM t1 WHERE col1 = 2 GROUP BY col1, col3 ORDER BY col3", 0},
		{"SELECT col1, col3, SUM(col2) FROM t1 WHERE col1 = 2 GROUP BY col1, col3 ORDER BY col3 DESC", 1},
		{"SELECT col1, col3, SUM(col2), COUNT(*) FROM t1 WHERE col1 = 3 GROUP BY col1, col3 ORDER BY col3 DESC", 3},
	}
	for _, r := range reads {
		plan, err := embedded.PlanPhysicalForTest(r.sql, table+indexes, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", r.sql, err)
		}
		reverse := 0
		var walk func(p plans.RecordQueryPlan)
		walk = func(p plans.RecordQueryPlan) {
			switch n := p.(type) {
			case *plans.RecordQueryAggregateIndexPlan:
				if n.IsReverse() {
					reverse++
				}
			case *plans.RecordQueryMultiIntersectionOnValuesPlan:
				if n.IsReverse() {
					reverse++
				}
			}
			for _, c := range p.GetChildren() {
				walk(c)
			}
		}
		walk(plan)
		if _, sorted := aggregateIndexAndSortIn(plan); sorted || reverse != r.reverse {
			t.Fatalf("want %d reverse aggregate scans and no sort\n  q: %s\n  plan: %s", r.reverse, r.sql, plan.Explain())
		}
	}
	sweep := func(stage string) {
		t.Helper()
		for _, r := range reads {
			gi, ei := mmRows(t, ctx, w.idx, r.sql)
			gn, en := mmRows(t, ctx, w.plain, r.sql)
			if ei != nil || en != nil {
				t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", stage, r.sql, ei, en)
				continue
			}
			if len(gn) < 2 {
				t.Errorf("%s: the unindexed twin answers %d rows, too few to show an order\n  q: %s", stage, len(gn), r.sql)
			}
			if !mmAggregateIndexRowsAgree(gi, gn, mmTrailingAggregates(r.sql)) {
				t.Errorf("%s: the reverse aggregate scan disagrees with the unindexed twin\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
					stage, r.sql, gi, gn, w.Explain(r.sql))
			}
		}
	}
	sweep("initial")
	for i, stmt := range []string{
		"DELETE FROM t1 WHERE col1 = 5",                               // vacates a group: its residue stays
		"UPDATE t1 SET col2 = NULL WHERE col1 = 3",                    // an all-NULL SUM group reads the residue
		"INSERT INTO t1 (id, col1, col2, col3) VALUES (100, 5, 7, 2)", // revives the vacated group
		"UPDATE t1 SET col1 = 9 WHERE id < 6",                         // a new largest group
		"DELETE FROM t1 WHERE col1 IS NULL",                           // drops the NULL group
	} {
		w.Exec(stmt)
		sweep(fmt.Sprintf("after dml %d (%s)", i, stmt))
	}
}
