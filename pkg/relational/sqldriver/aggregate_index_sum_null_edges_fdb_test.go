package sqldriver_test

// Edges around the SUM residual-zero divergence (see sumResidualZero): the
// aggregates that DO get the right answer over the same data, and why.
//
// The point of pinning the neighbours of a known defect is that a repair will
// be built next to them. Two of the three here are correct today for STRUCTURAL
// reasons rather than by luck, and each pins the structure alongside the rows —
// otherwise a change that removes the structure looks like an unrelated
// improvement right up until the answer moves.

import (
	"context"
	"strings"
	"testing"
)

// TestFDB_AggregateIndexSum_AvgDoesNotInheritTheDefect is a load-bearing
// NEGATIVE result: AVG over a group whose last non-NULL value was removed
// answers NULL correctly, where SUM over the identical data answers 0.
//
// The reason is structural, not arithmetic: AVG has no aggregate index at all
// (the DDL generator declines it — "AVG is streamable but not indexable"), so
// AVG is always computed by streaming the rows, which is the path that gets
// NULL right. That is worth a test precisely BECAUSE it is an absence: the day
// AVG becomes indexable, it inherits SUM's residual-zero problem on the same
// day, and the plan assertion below is what will say so.
func TestFDB_AggregateIndexSum_AvgDoesNotInheritTheDefect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_sum_avg", "sumavg",
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
		t.Errorf("AVG is now served by an aggregate index. It therefore inherits the residual-zero "+
			"defect SUM has (sumResidualZero): a group whose last non-NULL value was removed keeps "+
			"a zero accumulator, and nothing distinguishes it from a group that genuinely averages "+
			"to zero. Re-check this suite's AVG expectations before accepting the new plan.\n"+
			"  plan: %s", plan)
	}
	// g=1 is the whole point: the group whose last non-NULL value was UPDATEd
	// away answers NULL here, where the SUM twin below answers 0 over the very
	// same rows. g=2 and g=3 are exact, so their rendering carries no rounding
	// question — AVG over BIGINT renders 0 and 5 rather than 0.0 and 5.0.
	w.Want("AVG ignores NULLs and is NULL with no values", avgQ,
		[]string{"1|NULL", "2|0", "3|5"})

	// The SUM twin of the same query, for contrast: identical data, identical
	// grouping, and the answer differs because the SUM path is index-served.
	w.WantKnownDivergence("SUM over the same groups",
		"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|0", "3|10"},
		[]string{"1|NULL", "2|0", "3|10"}, sumResidualZero)

	// COUNT(v) is immune for a different structural reason: its SQL answer for a
	// group with no non-NULL values is 0, which is exactly what a residual zero
	// key and an absent key BOTH yield, so the ambiguity that breaks SUM has no
	// observable consequence here.
	w.Want("COUNT(v) is immune",
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
	w := mmNewTwin(t, ctx, "/testdb_sum_lifecycle", "sumlc",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g ")

	sumQ := "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g"

	w.Exec("INSERT INTO t (id, g, v) VALUES (1, 1, 5), (2, 1, 7), (3, 2, 100)")
	w.Want("initial", sumQ, []string{"1|12", "2|100"})

	// Empty group 1 entirely: the accumulator decrements to zero and the key is
	// left behind, but COUNT(*) is zero so the group must disappear.
	w.Exec("DELETE FROM t WHERE g = 1")
	w.Want("an emptied group disappears", sumQ, []string{"2|100"})

	// Re-create it. The sum must be 3, not 3 plus whatever the old key held.
	w.Exec("INSERT INTO t (id, g, v) VALUES (4, 1, 3)")
	w.Want("a re-created group sums only its new rows", sumQ, []string{"1|3", "2|100"})

	// Move every row of a group to another group: the source empties and the
	// destination gains, both through the same statement.
	w.Exec("UPDATE t SET g = 2 WHERE g = 1")
	w.Want("moving rows empties the source and grows the target", sumQ, []string{"2|103"})

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
	w := mmNewTwin(t, ctx, "/testdb_sum_nullkey", "sumnk",
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
