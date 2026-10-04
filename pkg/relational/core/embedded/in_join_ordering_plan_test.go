package embedded

import (
	"strings"
	"testing"
)

// Java's OrderingProperty.visitInJoinPlan: a sorted IN source turns the
// inner's equality binding on the IN value into a sorted key that precedes
// the inner's own ordering, so the InJoin itself satisfies the ORDER BY.
func TestPlanHarness_SortedInJoinSatisfiesTheSort(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE T5 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX I5 AS SELECT col1 FROM T5 ORDER BY col1"
	for _, tc := range []struct {
		sql  string
		want string
	}{
		{"SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1 DESC", "binding DESC"},
		{"SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1 DESC, id", "binding DESC"},
	} {
		plan, err := PlanQueryForTest(tc.sql, ddl, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if strings.Contains(plan, "InMemorySort") || !strings.Contains(plan, "InJoin(") ||
			!strings.Contains(plan, tc.want) {
			t.Fatalf("%s: want a sorted InJoin (%s) and no sort, got %s", tc.sql, tc.want, plan)
		}
	}
}

// An unsorted IN source keeps only the inner's equality-bound keys other than
// the IN value: no order survives the source, so the sort stays.
func TestPlanHarness_UnsortedInJoinClaimsNoOrder(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE T5 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX I5 AS SELECT col1 FROM T5 ORDER BY col1"
	const sql = "SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY id"
	plan, err := PlanQueryForTest(sql, ddl, nil)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if strings.Contains(plan, "InJoin(") && !strings.Contains(plan, "InMemorySort") {
		t.Fatalf("%s: an InJoin cannot order by the inner's key across IN values: %s", sql, plan)
	}
}
