package sqldriver_test

// NULL semantics of SUM served by an aggregate index.
//
// SQL SUM ignores NULLs and is NULL when a group has no non-NULL values. The
// aggregate index is an atomic accumulator: a NULL contributes nothing, so a
// group whose values were ALWAYS NULL has no key at all and reads back NULL by
// absence. But an ADD that decrements to zero LEAVES THE KEY BEHIND, so a group
// whose last non-NULL value is removed keeps a key holding 0 — indistinguishable
// from a group whose values genuinely sum to zero.
//
// Key presence therefore cannot answer "does this group have a non-NULL value?",
// and that is the question SUM's NULL-ness turns on. The two cases the
// discriminator must separate are pinned side by side throughout this file:
//
//	a group whose last non-NULL value was removed   -> SUM is NULL
//	a group whose non-NULL values sum to zero       -> SUM is 0
//
// The second is the reason a "treat 0 as NULL" repair is wrong, and it is why
// every case below that expects NULL has a zero-summing twin next to it.

import (
	"context"
	"testing"
)

// sumResidualZero is why the cases below are pinned as divergences rather than
// asserted correct, and what closing them needs.
//
// The NULL-ness of SUM turns on "does this group hold a non-NULL value?", and
// nothing the read path can reach answers it. The SUM index is an atomic
// accumulator with one key per group and no per-row entries, so there is nothing
// under it to probe — unlike MIN, where the ordinary subspace still holds every
// value and the stored NULL extremum can be resolved against it at read time.
//
// RFC-209 already names both halves of this ("a stored zero is indistinguishable
// from a vacated group, and an all-NULL group has no entry at all") and repairs
// the first with a COUNT(*) companion, which establishes that the GROUP exists.
// It cannot establish that the group's VALUES exist: COUNT(*) counts rows, and a
// live group of NULL-valued rows has a positive count and no sum.
//
// The missing piece is a second companion — COUNT_NOT_NULL over the summed
// column, on the same grouping key — so the merge can answer NULL for
// notNullCount == 0 and 0 for a group whose values genuinely cancel. That is new
// stored metadata on every SUM schema and a write-path cost on every mutation,
// so it is an owner decision rather than a repair to make in passing.
const sumResidualZero = "SUM's NULL-ness needs a COUNT_NOT_NULL companion that RFC-209 does not " +
	"generate; the COUNT(*) companion it does generate proves the group exists, not that the " +
	"group has values"

func TestFDB_AggregateIndexSum_NullVersusZero(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_sum_nullzero", "sumnz",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g ")

	//  g=1 : values summing to a non-zero total        SUM 12
	//  g=2 : values that legitimately cancel to zero   SUM 0   <- must stay 0
	//  g=3 : a single explicit zero                    SUM 0   <- must stay 0
	//  g=4 : all NULL from the start                   SUM NULL
	//  g=5 : mixed NULL and values                     SUM 4
	//  g=6 : a zero and a NULL                         SUM 0   <- must stay 0
	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, 1, 5), (102, 1, 7), " +
		"(201, 2, 5), (202, 2, -5), " +
		"(301, 3, 0), " +
		"(401, 4, NULL), (402, 4, NULL), " +
		"(501, 5, 4), (502, 5, NULL), " +
		"(601, 6, 0), (602, 6, NULL)")

	sumQ := "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g"
	w.WantPlanContains("SUM reaches the aggregate index", sumQ, "AggregateIndex(SUM")
	w.Want("SUM before any mutation", sumQ,
		[]string{"1|12", "2|0", "3|0", "4|NULL", "5|4", "6|0"})

	// The two removal routes that leave a residual zero key behind. Each is
	// applied to its own group so the states do not interfere.
	//
	// These are the cases the SUM index cannot answer today: see sumResidualZero
	// below for what is missing and why the repair is not a read-side one.
	w.Exec("UPDATE t SET v = NULL WHERE id = 501") // g=5 loses its only value
	w.WantKnownDivergence("after the last non-NULL value is UPDATEd away", sumQ,
		[]string{"1|12", "2|0", "3|0", "4|NULL", "5|0", "6|0"},
		[]string{"1|12", "2|0", "3|0", "4|NULL", "5|NULL", "6|0"}, sumResidualZero)

	w.Exec("DELETE FROM t WHERE id = 301") // g=3 loses its only row entirely
	w.WantKnownDivergence("after the only row of a zero-summing group is DELETEd", sumQ,
		[]string{"1|12", "2|0", "4|NULL", "5|0", "6|0"},
		[]string{"1|12", "2|0", "4|NULL", "5|NULL", "6|0"}, sumResidualZero)

	// A group reduced to zero by CANCELLATION still has non-NULL values, so it
	// stays 0 — this is the case a presence-based discriminator gets wrong in the
	// opposite direction, and it is why "read 0 as NULL" is not the repair.
	w.Exec("UPDATE t SET v = -7 WHERE id = 102") // g=1 now 5 + -7 = -2
	w.WantKnownDivergence("a group summing to a negative total", sumQ,
		[]string{"1|-2", "2|0", "4|NULL", "5|0", "6|0"},
		[]string{"1|-2", "2|0", "4|NULL", "5|NULL", "6|0"}, sumResidualZero)
	w.Exec("UPDATE t SET v = -5 WHERE id = 101") // g=1 now -5 + -7 = -12
	w.Exec("UPDATE t SET v = 5 WHERE id = 101")
	w.Exec("UPDATE t SET v = -5 WHERE id = 102") // g=1 now 5 + -5 = 0, both non-NULL
	w.WantKnownDivergence("a group driven to zero by cancellation stays 0", sumQ,
		[]string{"1|0", "2|0", "4|NULL", "5|0", "6|0"},
		[]string{"1|0", "2|0", "4|NULL", "5|NULL", "6|0"}, sumResidualZero)

	// Restoring a value to a group that had gone NULL.
	w.Exec("UPDATE t SET v = 3 WHERE id = 501")
	w.Want("a value returning to a NULL group", sumQ,
		[]string{"1|0", "2|0", "4|NULL", "5|3", "6|0"})

	// Deleting the NULL row of a mixed group changes nothing.
	w.Exec("DELETE FROM t WHERE id = 502")
	w.Want("deleting a NULL row leaves SUM alone", sumQ,
		[]string{"1|0", "2|0", "4|NULL", "5|3", "6|0"})

	// Emptying a group entirely removes it from the result, rather than making
	// it read as NULL.
	w.Exec("DELETE FROM t WHERE g = 4")
	w.Want("an emptied all-NULL group disappears", sumQ,
		[]string{"1|0", "2|0", "5|3", "6|0"})
	w.Exec("DELETE FROM t WHERE g = 2")
	w.Want("an emptied zero-summing group disappears", sumQ,
		[]string{"1|0", "5|3", "6|0"})
}

// TestFDB_AggregateIndexSum_NullWithCompanionAggregates pins SUM alongside the
// other aggregates over the same groups. The companion changes which index the
// plan drives from, so the NULL-ness decision has to survive each pairing.
func TestFDB_AggregateIndexSum_NullWithCompanionAggregates(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_sum_companion", "sumcomp",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_cntv_g AS SELECT COUNT(v) FROM t GROUP BY g ")

	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, 1, 9), (102, 1, NULL), " +
		"(201, 2, 4), (202, 2, -4), " +
		"(301, 3, NULL)")
	w.Exec("UPDATE t SET v = NULL WHERE id = 101") // g=1 -> residual zero key

	// g=1 : last value nulled away    -> SUM NULL, COUNT(*) 2, COUNT(v) 0
	// g=2 : cancels to zero           -> SUM 0,    COUNT(*) 2, COUNT(v) 2
	// g=3 : all-NULL from the start   -> SUM NULL, COUNT(*) 1, COUNT(v) 0
	w.WantKnownDivergence("SUM alone",
		"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|0", "3|NULL"},
		[]string{"1|NULL", "2|0", "3|NULL"}, sumResidualZero)
	w.WantKnownDivergence("SUM with COUNT(*)",
		"SELECT g, SUM(v), COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0|2", "2|0|2", "3|NULL|1"},
		[]string{"1|NULL|2", "2|0|2", "3|NULL|1"}, sumResidualZero)
	w.WantKnownDivergence("COUNT(*) before SUM",
		"SELECT g, COUNT(*), SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|2|0", "2|2|0", "3|1|NULL"},
		[]string{"1|2|NULL", "2|2|0", "3|1|NULL"}, sumResidualZero)
	// The schema HAS a COUNT(v) index, and it holds exactly the fact the merge
	// needs — yet the answer is still wrong, because nothing wires that index
	// into SUM's NULL-ness decision. This case is the direct evidence for what
	// the repair is: a companion the planner KNOWS about, not merely one that
	// happens to exist.
	w.WantKnownDivergence("SUM with COUNT(v)",
		"SELECT g, SUM(v), COUNT(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0|0", "2|0|2", "3|NULL|0"},
		[]string{"1|NULL|0", "2|0|2", "3|NULL|0"}, sumResidualZero)
	w.WantKnownDivergence("SUM with both counts",
		"SELECT g, SUM(v), COUNT(v), COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0|0|2", "2|0|2|2", "3|NULL|0|1"},
		[]string{"1|NULL|0|2", "2|0|2|2", "3|NULL|0|1"}, sumResidualZero)

	// COUNT(col) is unaffected either way — its answer for a group with no
	// non-NULL values is 0, which is what both a residual zero key and an absent
	// key yield. It is pinned so a repair aimed at SUM cannot move it.
	w.Want("COUNT(v) is 0, never NULL",
		"SELECT g, COUNT(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|2", "3|0"})

	// Single-group spellings, where the scan range is a point.
	// Single-group SUM is served by a scan rather than the aggregate index, so
	// it is CORRECT where the grouped spelling is not — the same query, the same
	// data, two answers depending on the plan. Asserted as correct because it is.
	w.Want("SUM of one residual-zero group", "SELECT SUM(v) FROM t WHERE g = 1", []string{"NULL"})
	w.Want("SUM of one cancelling group", "SELECT SUM(v) FROM t WHERE g = 2", []string{"0"})
	w.Want("SUM of one all-NULL group", "SELECT SUM(v) FROM t WHERE g = 3", []string{"NULL"})
	w.Want("SUM of an absent group", "SELECT SUM(v) FROM t WHERE g = 99", []string{"NULL"})

	// HAVING reads the aggregate after grouping, so it must see the same NULL.
	// HAVING inherits the defect, and this is where it stops being an odd NULL in
	// a column and starts DROPPING and ADDING rows: g=1 is missing from the
	// IS NULL result and present in the = 0 one.
	w.WantKnownDivergence("HAVING SUM IS NULL",
		"SELECT g FROM t GROUP BY g HAVING SUM(v) IS NULL ORDER BY g",
		[]string{"3"}, []string{"1", "3"}, sumResidualZero)
	w.WantKnownDivergence("HAVING SUM = 0",
		"SELECT g FROM t GROUP BY g HAVING SUM(v) = 0 ORDER BY g",
		[]string{"1", "2"}, []string{"2"}, sumResidualZero)
}
