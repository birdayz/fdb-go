package sqldriver_test

// NULL semantics of MIN/MAX served by a PERMUTED_MIN / PERMUTED_MAX aggregate
// index.
//
// SQL MIN/MAX IGNORE NULLs: they are the extremum of the non-NULL values, and
// NULL only when there are no non-NULL values at all. The permuted index stores
// one entry per group holding the extremum *including* NULL — NULL sorts before
// every value, so for MIN it always wins, and a group holding one NULL reads
// back as NULL however many real values sit beside it. MAX is unaffected for
// the same reason: NULL sorts lowest, so it never wins a MAX.
//
// The asymmetry is why MAX cases are here in equal number. They are not padding:
// the read-side repair applies to MIN only, and a repair that "fixed" MAX too
// would be wrong. These cases are what makes that direction checkable.
//
// The dimension this file adds to the existing MIN/MAX coverage is the MIXED
// group — one holding both a NULL and a non-NULL value at the moment the query
// runs. Existing fixtures have all-NULL groups and NULL-free groups; the one
// that builds a mixed group mutates it to all-NULL before asserting.

import (
	"context"
	"testing"
)

func TestFDB_AggregateIndexMinMax_MixedNullGroups(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_minmax_mixed", "mmmix",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_v_g AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max_v_g AS SELECT MAX(v) FROM t GROUP BY g ")

	//  g=1 : NULL-free                          MIN 3   MAX 9
	//  g=2 : mixed, NULL first in key order     MIN 5   MAX 5
	//  g=3 : mixed, several NULLs               MIN 2   MAX 8
	//  g=4 : all NULL                           MIN NULL MAX NULL
	//  g=5 : single non-NULL row                MIN 7   MAX 7
	//  g=6 : single NULL row                    MIN NULL MAX NULL
	//  g=7 : mixed with negatives and zero      MIN -4  MAX 0
	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, 1, 3), (102, 1, 9), (103, 1, 5), " +
		"(201, 2, 5), (202, 2, NULL), " +
		"(301, 3, NULL), (302, 3, 8), (303, 3, NULL), (304, 3, 2), (305, 3, NULL), " +
		"(401, 4, NULL), (402, 4, NULL), " +
		"(501, 5, 7), " +
		"(601, 6, NULL), " +
		"(701, 7, 0), (702, 7, NULL), (703, 7, -4), (704, 7, -1)")

	minQ := "SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g"
	maxQ := "SELECT g, MAX(v) FROM t GROUP BY g ORDER BY g"
	w.WantPlanContains("MIN reaches the aggregate index", minQ, "AggregateIndex(MIN")
	w.WantPlanContains("MAX reaches the aggregate index", maxQ, "AggregateIndex(MAX")

	w.Want("MIN ignores NULLs", minQ, []string{
		"1|3", "2|5", "3|2", "4|NULL", "5|7", "6|NULL", "7|-4",
	})
	w.Want("MAX ignores NULLs", maxQ, []string{
		"1|9", "2|5", "3|8", "4|NULL", "5|7", "6|NULL", "7|0",
	})

	// Both extrema in one query: the plan intersects two aggregate indexes, a
	// different read path from either alone.
	w.Want("MIN and MAX together",
		"SELECT g, MIN(v), MAX(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|3|9", "2|5|5", "3|2|8", "4|NULL|NULL", "5|7|7", "6|NULL|NULL", "7|-4|0"})

	// A single group selected by an equality on the grouping key: the scan range
	// is a point rather than the whole index, which is a distinct range path.
	w.Want("MIN of one mixed group",
		"SELECT MIN(v) FROM t WHERE g = 3", []string{"2"})
	w.Want("MAX of one mixed group",
		"SELECT MAX(v) FROM t WHERE g = 3", []string{"8"})
	w.Want("MIN of an all-NULL group",
		"SELECT MIN(v) FROM t WHERE g = 4", []string{"NULL"})
	w.Want("MIN of an absent group",
		"SELECT MIN(v) FROM t WHERE g = 99", []string{"NULL"})

	// A range over the grouping key, so several groups come from one scan.
	w.Want("MIN over a group range",
		"SELECT g, MIN(v) FROM t WHERE g >= 2 AND g <= 4 GROUP BY g ORDER BY g",
		[]string{"2|5", "3|2", "4|NULL"})

	// HAVING reads the aggregate again after grouping.
	w.Want("HAVING on MIN",
		"SELECT g, MIN(v) FROM t GROUP BY g HAVING MIN(v) > 0 ORDER BY g",
		[]string{"1|3", "2|5", "3|2", "5|7"})
	w.Want("HAVING MIN IS NULL",
		"SELECT g FROM t GROUP BY g HAVING MIN(v) IS NULL ORDER BY g",
		[]string{"4", "6"})
}

// TestFDB_AggregateIndexMinMax_NullTransitions walks a group through every
// transition that changes whether its extremum is NULL. The stored permuted
// entry is a read-modify-write of the current extremum, so each transition
// exercises maintenance as well as the read.
func TestFDB_AggregateIndexMinMax_NullTransitions(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_minmax_trans", "mmtr",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_v_g AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max_v_g AS SELECT MAX(v) FROM t GROUP BY g ")

	minQ := "SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g"
	maxQ := "SELECT g, MAX(v) FROM t GROUP BY g ORDER BY g"

	w.Exec("INSERT INTO t (id, g, v) VALUES (1, 1, 5), (2, 1, 9)")
	w.Want("NULL-free start", minQ, []string{"1|5"})

	// A NULL arrives beside real values: the extremum must not move.
	w.Exec("INSERT INTO t (id, g, v) VALUES (3, 1, NULL)")
	w.Want("after a NULL is inserted, MIN is unchanged", minQ, []string{"1|5"})
	w.Want("after a NULL is inserted, MAX is unchanged", maxQ, []string{"1|9"})

	// A smaller real value arrives beside the NULL.
	w.Exec("INSERT INTO t (id, g, v) VALUES (4, 1, 2)")
	w.Want("a smaller value beside a NULL becomes MIN", minQ, []string{"1|2"})

	// The current MIN is UPDATEd to NULL: the next-smallest non-NULL takes over.
	w.Exec("UPDATE t SET v = NULL WHERE id = 4")
	w.Want("after the MIN row is nulled, MIN falls back", minQ, []string{"1|5"})

	// The current MIN row is DELETEd.
	w.Exec("DELETE FROM t WHERE id = 1")
	w.Want("after the MIN row is deleted, MIN falls back", minQ, []string{"1|9"})

	// Deleting a NULL row must not disturb anything.
	w.Exec("DELETE FROM t WHERE id = 3")
	w.Want("deleting a NULL row leaves MIN alone", minQ, []string{"1|9"})
	w.Want("deleting a NULL row leaves MAX alone", maxQ, []string{"1|9"})

	// The last non-NULL value goes away, leaving only NULLs: now MIN is NULL.
	w.Exec("UPDATE t SET v = NULL WHERE id = 2")
	w.Want("with only NULLs left, MIN is NULL", minQ, []string{"1|NULL"})
	w.Want("with only NULLs left, MAX is NULL", maxQ, []string{"1|NULL"})

	// A real value returns to a group that had gone all-NULL.
	w.Exec("INSERT INTO t (id, g, v) VALUES (5, 1, 42)")
	w.Want("a value returning to an all-NULL group becomes MIN", minQ, []string{"1|42"})
	w.Want("a value returning to an all-NULL group becomes MAX", maxQ, []string{"1|42"})

	// A NULL row moving BETWEEN groups must not carry an extremum with it.
	w.Exec("INSERT INTO t (id, g, v) VALUES (6, 2, NULL), (7, 2, 8)")
	w.Want("second group mixed", minQ, []string{"1|42", "2|8"})
	w.Exec("UPDATE t SET g = 1 WHERE id = 6")
	w.Want("after a NULL row moves groups, neither MIN moves", minQ, []string{"1|42", "2|8"})

	// Emptying a group entirely: it must disappear, not read as NULL.
	w.Exec("DELETE FROM t WHERE g = 2")
	w.Want("an emptied group is gone", minQ, []string{"1|42"})
}

// TestFDB_AggregateIndexMinMax_TypesAndCompositeKeys widens the defect's axes:
// the value column's TYPE (the tuple encoding of NULL is type-independent, so
// every type must behave alike) and a multi-column grouping key (which changes
// the permuted key layout).
func TestFDB_AggregateIndexMinMax_TypesAndCompositeKeys(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_minmax_types", "mmty",
		"CREATE TABLE t (id BIGINT, g1 BIGINT, g2 BIGINT, vi BIGINT, vd DOUBLE, vs STRING, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_vi AS SELECT MIN(vi) FROM t GROUP BY g1, g2 "+
			"CREATE INDEX t_max_vi AS SELECT MAX(vi) FROM t GROUP BY g1, g2 "+
			"CREATE INDEX t_min_vd AS SELECT MIN(vd) FROM t GROUP BY g1 "+
			"CREATE INDEX t_max_vd AS SELECT MAX(vd) FROM t GROUP BY g1 ")

	w.Exec("INSERT INTO t (id, g1, g2, vi, vd, vs) VALUES " +
		"(1, 1, 1, 5,    2.5,  'b'), " +
		"(2, 1, 1, NULL, NULL, NULL), " +
		"(3, 1, 1, 3,    -1.5, 'a'), " +
		"(4, 1, 2, NULL, NULL, NULL), " +
		"(5, 1, 2, 7,    0.0,  'z'), " +
		"(6, 2, 1, NULL, NULL, NULL)")

	w.Want("composite grouping key, MIN ignores NULLs",
		"SELECT g1, g2, MIN(vi) FROM t GROUP BY g1, g2 ORDER BY g1, g2",
		[]string{"1|1|3", "1|2|7", "2|1|NULL"})
	w.Want("composite grouping key, MAX ignores NULLs",
		"SELECT g1, g2, MAX(vi) FROM t GROUP BY g1, g2 ORDER BY g1, g2",
		[]string{"1|1|5", "1|2|7", "2|1|NULL"})

	// DOUBLE: a negative value must beat a NULL, and 0.0 must not be confused
	// with the absence of a value.
	w.Want("DOUBLE MIN ignores NULLs",
		"SELECT g1, MIN(vd) FROM t GROUP BY g1 ORDER BY g1",
		[]string{"1|-1.5", "2|NULL"})
	w.Want("DOUBLE MAX ignores NULLs",
		"SELECT g1, MAX(vd) FROM t GROUP BY g1 ORDER BY g1",
		[]string{"1|2.5", "2|NULL"})
}
