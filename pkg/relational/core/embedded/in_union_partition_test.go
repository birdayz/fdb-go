package embedded

import "testing"

// TestInUnion_MemoizesThePartitionWhole pins RFC-257 WS-F 4.3 item 3. The
// in-union ranges over its whole ordering partition, as Java's
// memoizeMemberPlansFromOther does, so the push-through-fetch rule sees the
// covering member and builds Java's FETCH(INUNION(COVERING)); Go used to pin
// the one non-covering member and ran the fetch per leg. The in-join is
// disabled because Go's cost model prefers the sorted in-join here (the
// RFC-191 ruling). The second query is the shape the pin was protecting: an
// IN with a residual filter, whose in-union child must still provide the
// merge order (extraction verifies it, checkInUnionChildOrdering).
func TestInUnion_MemoizesThePartitionWhole(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE T (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX I1 AS SELECT col1 FROM T ORDER BY col1"
	for _, c := range []struct{ sql, want string }{
		{
			"SELECT * FROM T WHERE col1 IN (10, 20) ORDER BY col1",
			"Fetch(InUnion(IndexScan(I1, [=] COVERING), bindings=1, ASC))",
		},
		{
			"SELECT * FROM T WHERE col1 IN (10, 20) AND col2 > 0 ORDER BY col1",
			"InUnion(PredicatesFilter(IndexScan(I1, [=]), [1 preds]), bindings=1, ASC)",
		},
	} {
		plan, err := PlanQueryForTestWithDisabledRules(c.sql, schema, nil, []string{"ImplementInJoinRule"})
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if plan != c.want {
			t.Errorf("%s\n  plan %s\n  want %s", c.sql, plan, c.want)
		}
	}
}
