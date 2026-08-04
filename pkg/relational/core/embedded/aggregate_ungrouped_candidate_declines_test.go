package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// TestUngroupedAggregateIndexDeclinesCandidacy pins the ROOT CAUSE of a fact
// RFC-209 §5.4 records as unexplained: Go does not reproduce Java's ungrouped
// `SELECT SUM(v) FROM t` divergence, and the reason is that the planner never
// routes an ungrouped aggregate through the aggregate index at all.
//
// The RFC's parenthetical reads "NOT root-caused: why the planner declines.
// AggregateDataAccessRule matches *expressions.GroupByExpression and the
// translator builds one even with zero grouping keys, so the rule looks
// reachable." Both halves of that are true, and they are why the answer was not
// obvious: the rule IS reachable — the CANDIDATE is not.
// tryAggregateIndexCandidate returns nil when the index's grouping count is
// zero, so an ungrouped aggregate index never enters the candidate set and the
// rule has nothing to match against.
//
// This matters because the fact is load-bearing and was unpinned. Java's
// aggregate-empty-table.yamsql has the divergence in one file — :547-550 runs
// the unindexed `select sum(col1) from T1` over an emptied table and expects
// NULL, while :577-580 runs the same query over an indexed T2, plans
// `AISCAN(T2_I5 <,> BY_GROUP ...)` and expects 0. Go agrees with the oracle on
// both only because no index-backed path exists. The moment this guard is
// relaxed, that divergence arms itself: an index-backed ungrouped SUM reads the
// stored accumulator, which an emptied table leaves at 0 where the correct
// answer is NULL.
//
// RFC-209 §5.3(a)'s zero-drop deliberately does NOT cover the ungrouped case
// either (an ungrouped aggregate has exactly one group, which exists whether or
// not the table has rows), so relaxing this guard would need the companion rule
// extended to the ungrouped case FIRST.
func TestUngroupedAggregateIndexDeclinesCandidacy(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TABLE SALES (
  id BIGINT,
  category STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX sum_all AS SELECT SUM(amount) FROM SALES
CREATE INDEX sum_by_cat AS SELECT SUM(amount) FROM SALES GROUP BY category
`
	tmpl, err := BuildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatalf("build schema template: %v", err)
	}
	md := tmpl.Underlying()
	if md == nil {
		t.Fatal("schema template carries no metadata")
	}

	find := func(want string) *recordlayer.Index {
		t.Helper()
		for name, idx := range md.GetAllIndexes() {
			if strings.EqualFold(name, want) {
				return idx
			}
		}
		var names []string
		for n := range md.GetAllIndexes() {
			names = append(names, n)
		}
		t.Fatalf("index %q not found (have %v)", want, names)
		return nil
	}

	// Control: the GROUPED sibling of the same aggregate over the same table
	// MUST produce a candidate. Without it, a change that broke aggregate
	// candidacy outright would satisfy the assertion below while proving
	// nothing about the grouping count.
	grouped := find("sum_by_cat")
	if got := tryAggregateIndexCandidate(grouped, md); got == nil {
		t.Fatal("a GROUPED SUM index produced no aggregate candidate — aggregate candidacy " +
			"is broken generally, so the ungrouped assertion below would hold vacuously")
	}

	ungrouped := find("sum_all")
	gke, ok := ungrouped.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		t.Fatalf("ungrouped SUM index root is %T, want *recordlayer.GroupingKeyExpression", ungrouped.RootExpression)
	}
	if gc := gke.GetGroupingCount(); gc != 0 {
		t.Fatalf("the ungrouped SUM index reports grouping count %d, want 0 — the DDL no longer "+
			"builds `SELECT SUM(v) FROM t` as an UNGROUPED aggregate, so this test is no longer "+
			"testing the shape it names", gc)
	}

	if got := tryAggregateIndexCandidate(ungrouped, md); got != nil {
		t.Fatalf("an UNGROUPED aggregate index now produces a match candidate (%v).\n"+
			"That is a real capability change and it ARMS a divergence Go currently does not "+
			"have: an index-backed ungrouped SUM reads the stored accumulator, which an "+
			"emptied table leaves at 0, where SUM over zero rows is NULL. Java has exactly "+
			"this bug — aggregate-empty-table.yamsql :547-550 (unindexed, NULL) versus "+
			":577-580 (SUM index, 0), same data, same file.\n"+
			"Before allowing this, extend RFC-209's companion group-existence rule to the "+
			"ungrouped case; RFC-209 5.3(a)'s zero-drop deliberately does NOT cover it, because "+
			"an ungrouped aggregate's single group exists whether or not the table has rows.", got)
	}
}
