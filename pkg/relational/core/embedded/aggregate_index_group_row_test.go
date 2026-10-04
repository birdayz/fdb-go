package embedded

import (
	"strings"
	"testing"
)

const aggregateGroupRowSchema = `CREATE TABLE ga (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM ga GROUP BY g
CREATE INDEX sum_by_g AS SELECT SUM(v) FROM ga GROUP BY g
CREATE TABLE gb (id BIGINT, h BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX cnt_by_h AS SELECT COUNT(*) FROM gb GROUP BY h
CREATE INDEX sum_by_h AS SELECT SUM(v) FROM gb GROUP BY h`

// TestAggregateIndexPublishesTheGroupByRow pins that an aggregate-index plan
// states the GroupBy's own row, so no projection renames the index's columns
// (COUNT(*)) to the GroupBy's (COUNT(1), SUM(GB.V)). Java's aggregate plan
// likewise publishes a result Value chosen at toEquivalentPlan, with nothing
// between it and the block's Map. One query per builder: the single index
// scan, the multi-aggregate intersection, and the group-existence merge (the
// union's second leg keeps its qualified operand name).
func TestAggregateIndexPublishesTheGroupByRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, sql, want string
	}{
		{
			name: "single_index_scan",
			sql:  "SELECT h, COUNT(1) FROM gb GROUP BY h",
			want: "Map(AggregateIndex(COUNT, CNT_BY_H, [H], GB, live_groups_only), {H: _current.H#0, _1: _current.COUNT(1)#1})",
		},
		{
			name: "multi_aggregate_intersection",
			sql:  "SELECT h, SUM(v), COUNT(1) FROM gb GROUP BY h",
			want: "Map(GroupExistenceMerge(AggregateIndex(SUM, SUM_BY_H, [H], GB), AggregateIndex(COUNT, CNT_BY_H, [H], GB, live_groups_only); keys=[H#0], driving=1), {H: _current.H#0, _1: _current.SUM(V)#1, _2: _current.COUNT(1)#2})",
		},
		{
			name: "group_existence_merge",
			sql:  "SELECT g, SUM(ga.v) AS s FROM ga GROUP BY g UNION ALL SELECT h, SUM(gb.v) AS s2 FROM gb GROUP BY h",
			want: "UnorderedUnion(Map(GroupExistenceMerge(AggregateIndex(COUNT, CNT_BY_G, [G], GA, live_groups_only), AggregateIndex(SUM, SUM_BY_G, [G], GA); keys=[G#0], driving=0), {G: _current.G#0, S: _current.SUM(V)#1}), Map(GroupExistenceMerge(AggregateIndex(COUNT, CNT_BY_H, [H], GB, live_groups_only), AggregateIndex(SUM, SUM_BY_H, [H], GB); keys=[H#0], driving=0), {G: _current.H#0, S: _current.SUM(GB.V)#1}))",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := PlanQueryForTest(c.sql, aggregateGroupRowSchema, nil)
			if err != nil {
				t.Fatalf("planning %q: %v", c.sql, err)
			}
			if strings.Contains(got, "Project(") {
				t.Fatalf("%q renames the aggregate row through a projection:\n  %s", c.sql, got)
			}
			if got != c.want {
				t.Fatalf("%q\n  got:  %s\n  want: %s", c.sql, got, c.want)
			}
		})
	}
}
