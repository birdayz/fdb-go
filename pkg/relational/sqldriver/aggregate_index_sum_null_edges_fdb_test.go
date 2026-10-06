package sqldriver_test

// Edges around the SUM residual zero (aggregate_index_sum_null_semantics_fdb_test.go):
// the aggregates that get the right answer over the same data, and why. Each is
// correct for a STRUCTURAL reason, pinned alongside the rows, so a change that
// removes the structure cannot look like an unrelated improvement.

import (
	"context"
	"strings"
	"testing"
)

// TestFDB_AggregateIndexSum_AvgDoesNotInheritTheDefect is a load-bearing
// NEGATIVE result: AVG over a group whose last non-NULL value was removed
// answers NULL correctly.
//
// The reason is structural, not arithmetic: AVG has no aggregate index at all
// (the DDL generator declines it — "AVG is streamable but not indexable"), so
// AVG is always computed by streaming the rows. That is worth a test precisely
// BECAUSE it is an absence: the day AVG becomes indexable, its residual zero
// reads as the SUM index's does, and the plan assertion below is what will say
// so.
func TestFDB_AggregateIndexSum_AvgDoesNotInheritTheDefect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/FRL/testdb_sum_avg", "sumavg",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_cntv_g AS SELECT COUNT(v) FROM t GROUP BY g ")

	//  g=1 : loses its only value to an UPDATE  -> SUM must be NULL, AVG NULL
	//  g=2 : values that cancel to zero         -> SUM 0,   AVG 0
	//  g=3 : ordinary                           -> SUM 10,  AVG 5
	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, 1, 9), (102, 1, NULL), " +
		"(201, 2, 4), (202, 2, -4), " +
		"(301, 3, 4), (302, 3, 6)")
	w.Exec("UPDATE t SET v = NULL WHERE id = 101")

	avgQ := "SELECT g, AVG(v) FROM t GROUP BY g ORDER BY g"
	if plan := w.Explain(avgQ); strings.Contains(plan, "AggregateIndex") {
		t.Errorf("AVG is now served by an aggregate index. A group whose last non-NULL value was "+
			"removed keeps a zero accumulator. Re-check this suite's AVG expectations before "+
			"accepting the new plan.\n"+
			"  plan: %s", plan)
	}
	// g=1 is the whole point: the group whose last non-NULL value was UPDATEd
	// away answers NULL, as the SUM twin below does over the very same rows. g=2 and g=3 are exact, so their rendering carries no rounding
	// question — AVG over BIGINT renders 0 and 5 rather than 0.0 and 5.0.
	w.Want("AVG ignores NULLs and is NULL with no values", avgQ,
		[]string{"1|NULL", "2|0", "3|5"})

	// The SUM twin of the same query: index-served, its residue read as 0, as
	// Java's index reads it.
	w.WantPlanContains("SUM over the same groups", "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g", "AggregateIndex(SUM")
	w.WantAggregateIndex("SUM over the same groups",
		"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|0", "3|10"},
		[]string{"1|NULL", "2|0", "3|10"})

	// COUNT(v): its SQL answer for a group with no non-NULL values is 0, which
	// is what a residual zero key yields.
	w.Want("COUNT(v) reads the residue as SQL's 0",
		"SELECT g, COUNT(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|2", "3|2"})
	w.Want("COUNT(*) counts rows regardless of their values",
		"SELECT g, COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"1|2", "2|2", "3|2"})
}

// TestFDB_AggregateIndexSum_GroupLifecycle walks a group all the way out and
// back in. The accumulator key survives a group being emptied, so re-creating
// the group has to produce the sum of the NEW rows and not the old key's
// residue plus them.
func TestFDB_AggregateIndexSum_GroupLifecycle(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/FRL/testdb_sum_lifecycle", "sumlc",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g ")

	sumQ := "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g"

	w.Exec("INSERT INTO t (id, g, v) VALUES (1, 1, 5), (2, 1, 7), (3, 2, 100)")
	w.Want("initial", sumQ, []string{"1|12", "2|100"})

	// Empty group 1 entirely: the accumulator decrements to zero and the key is
	// left behind, so Java's index reads it as 0.
	w.Exec("DELETE FROM t WHERE g = 1")
	w.WantAggregateIndex("an emptied group reads its residue", sumQ,
		[]string{"1|0", "2|100"}, []string{"2|100"})

	// Re-create it. The sum must be 3, not 3 plus whatever the old key held.
	w.Exec("INSERT INTO t (id, g, v) VALUES (4, 1, 3)")
	w.Want("a re-created group sums only its new rows", sumQ, []string{"1|3", "2|100"})

	// Move every row of a group to another group: the source empties and the
	// destination gains, both through the same statement.
	w.Exec("UPDATE t SET g = 2 WHERE g = 1")
	w.WantAggregateIndex("moving rows empties the source and grows the target", sumQ,
		[]string{"1|0", "2|103"}, []string{"2|103"})

	// And back, one row at a time.
	w.Exec("UPDATE t SET g = 1 WHERE id = 4")
	w.Want("moving one row back", sumQ, []string{"1|3", "2|100"})

	// A group whose values sum to zero must survive as 0 rather than vanish —
	// this is the case a "drop the group when the accumulator is zero" repair
	// would break, and it is here to make that breakage loud.
	w.Exec("INSERT INTO t (id, g, v) VALUES (5, 3, 8), (6, 3, -8)")
	w.Want("a zero-summing group is present with 0", sumQ,
		[]string{"1|3", "2|100", "3|0"})
	w.Want("and it is not NULL",
		"SELECT g FROM t GROUP BY g HAVING SUM(v) = 0 ORDER BY g", []string{"3"})
}

// TestFDB_AggregateIndexSum_NullGroupKey puts the NULL in the grouping column
// rather than the value, for SUM as the MIN suite does for the extremum.
func TestFDB_AggregateIndexSum_NullGroupKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/FRL/testdb_sum_nullkey", "sumnk",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g ")

	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, NULL, 3), (102, NULL, 4), " +
		"(201, 1, 10), " +
		"(301, NULL, NULL)")

	w.Want("a NULL grouping key accumulates like any other",
		"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"NULL|7", "1|10"})
	w.Want("and counts like any other",
		"SELECT g, COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"NULL|3", "1|1"})

	// Removing one contributor from the NULL group.
	w.Exec("DELETE FROM t WHERE id = 101")
	w.Want("after a delete from the NULL group",
		"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"NULL|4", "1|10"})
}
