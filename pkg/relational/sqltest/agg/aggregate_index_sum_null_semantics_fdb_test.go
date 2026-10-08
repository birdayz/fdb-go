package sqltest

// NULL semantics of SUM served by an aggregate index, as Java's index answers.
//
// SQL SUM ignores NULLs and is NULL when a group has no non-NULL values. The
// aggregate index is an atomic accumulator: a NULL contributes nothing, so a
// group whose values were ALWAYS NULL has no key at all and has no row. An ADD
// that decrements to zero LEAVES THE KEY BEHIND, so a group whose last non-NULL
// value (or last row) is removed keeps a key holding 0.
//
// Java reads the index alone (AggregateIndexMatchCandidate has no notion of
// group existence; its own corpus marks the difference as an open bug,
// aggregate-empty-table.yamsql), and Go does the same. Each read below pins
// both answers: Java's from the index, SQL's from the records.

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_AggregateIndexSum_NullVersusZero(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := testkit.NewTwin(t, ctx, "/FRL/testdb_sum_nullzero", "sumnz",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_cntv_g AS SELECT COUNT(v) FROM t GROUP BY g ")

	//  g=1 : values summing to a non-zero total        SUM 12
	//  g=2 : values that legitimately cancel to zero   SUM 0
	//  g=3 : a single explicit zero                    SUM 0
	//  g=4 : all NULL from the start                   SUM NULL, no index key
	//  g=5 : mixed NULL and values                     SUM 4
	//  g=6 : a zero and a NULL                         SUM 0
	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, 1, 5), (102, 1, 7), " +
		"(201, 2, 5), (202, 2, -5), " +
		"(301, 3, 0), " +
		"(401, 4, NULL), (402, 4, NULL), " +
		"(501, 5, 4), (502, 5, NULL), " +
		"(601, 6, 0), (602, 6, NULL)")

	sumQ := "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g"
	w.WantPlanContains("SUM reaches the aggregate index", sumQ, "AggregateIndex(SUM")
	w.WantAggregateIndex("SUM before any mutation", sumQ,
		[]string{"1|12", "2|0", "3|0", "5|4", "6|0"},
		[]string{"1|12", "2|0", "3|0", "4|NULL", "5|4", "6|0"})

	// The two removal routes that leave a residual zero key behind.
	w.Exec("UPDATE t SET v = NULL WHERE id = 501") // g=5 loses its only value
	w.WantAggregateIndex("after the last non-NULL value is UPDATEd away", sumQ,
		[]string{"1|12", "2|0", "3|0", "5|0", "6|0"},
		[]string{"1|12", "2|0", "3|0", "4|NULL", "5|NULL", "6|0"})

	w.Exec("DELETE FROM t WHERE id = 301") // g=3 loses its only row entirely
	w.WantAggregateIndex("after the only row of a zero-summing group is DELETEd", sumQ,
		[]string{"1|12", "2|0", "3|0", "5|0", "6|0"},
		[]string{"1|12", "2|0", "4|NULL", "5|NULL", "6|0"})

	// Cancellation through updates keeps the live accumulator exact.
	w.Exec("UPDATE t SET v = -7 WHERE id = 102") // g=1 now 5 + -7 = -2
	w.WantAggregateIndex("a group summing to a negative total", sumQ,
		[]string{"1|-2", "2|0", "3|0", "5|0", "6|0"},
		[]string{"1|-2", "2|0", "4|NULL", "5|NULL", "6|0"})
	w.Exec("UPDATE t SET v = -5 WHERE id = 101")
	w.Exec("UPDATE t SET v = 5 WHERE id = 101")
	w.Exec("UPDATE t SET v = -5 WHERE id = 102") // g=1 now 5 + -5 = 0
	w.WantAggregateIndex("a group driven to zero by cancellation", sumQ,
		[]string{"1|0", "2|0", "3|0", "5|0", "6|0"},
		[]string{"1|0", "2|0", "4|NULL", "5|NULL", "6|0"})

	// Restoring a value to a group that had gone NULL.
	w.Exec("UPDATE t SET v = 3 WHERE id = 501")
	w.WantAggregateIndex("a value returning to a NULL group", sumQ,
		[]string{"1|0", "2|0", "3|0", "5|3", "6|0"},
		[]string{"1|0", "2|0", "4|NULL", "5|3", "6|0"})

	// Deleting the NULL row of a mixed group changes nothing.
	w.Exec("DELETE FROM t WHERE id = 502")
	w.WantAggregateIndex("deleting a NULL row leaves SUM alone", sumQ,
		[]string{"1|0", "2|0", "3|0", "5|3", "6|0"},
		[]string{"1|0", "2|0", "4|NULL", "5|3", "6|0"})

	// Emptying a group: the all-NULL one never had a key; the zero-summing one
	// keeps its residue.
	w.Exec("DELETE FROM t WHERE g = 4")
	w.WantAggregateIndex("an emptied all-NULL group", sumQ,
		[]string{"1|0", "2|0", "3|0", "5|3", "6|0"},
		[]string{"1|0", "2|0", "5|3", "6|0"})
	w.Exec("DELETE FROM t WHERE g = 2")
	w.WantAggregateIndex("an emptied zero-summing group", sumQ,
		[]string{"1|0", "2|0", "3|0", "5|3", "6|0"},
		[]string{"1|0", "5|3", "6|0"})
}

// TestFDB_AggregateIndexSum_NullWithOtherAggregates pins SUM alongside the
// other aggregates over the same groups: a multi-aggregate read is Java's
// inner intersection of the indexes, so a group missing from one index (an
// all-NULL group has no SUM entry) has no row.
func TestFDB_AggregateIndexSum_NullWithOtherAggregates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := testkit.NewTwin(t, ctx, "/FRL/testdb_sum_companion", "sumcomp",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_cntv_g AS SELECT COUNT(v) FROM t GROUP BY g ")

	w.Exec("INSERT INTO t (id, g, v) VALUES " +
		"(101, 1, 9), (102, 1, NULL), " +
		"(201, 2, 4), (202, 2, -4), " +
		"(301, 3, NULL)")
	w.Exec("UPDATE t SET v = NULL WHERE id = 101") // g=1 -> residual zero keys

	// g=1 : last value nulled away    -> index SUM 0, COUNT(v) 0; SQL SUM NULL
	// g=2 : cancels to zero           -> SUM 0, COUNT(*) 2, COUNT(v) 2
	// g=3 : all-NULL from the start   -> no SUM or COUNT(v) entry; COUNT(*) 1
	w.WantAggregateIndex("SUM alone",
		"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|0"},
		[]string{"1|NULL", "2|0", "3|NULL"})
	w.WantAggregateIndex("SUM with COUNT(*)",
		"SELECT g, SUM(v), COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0|2", "2|0|2"},
		[]string{"1|NULL|2", "2|0|2", "3|NULL|1"})
	w.WantAggregateIndex("SUM with COUNT(v)",
		"SELECT g, SUM(v), COUNT(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0|0", "2|0|2"},
		[]string{"1|NULL|0", "2|0|2", "3|NULL|0"})
	w.WantAggregateIndex("SUM with both counts",
		"SELECT g, SUM(v), COUNT(v), COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0|0|2", "2|0|2|2"},
		[]string{"1|NULL|0|2", "2|0|2|2", "3|NULL|0|1"})
	w.WantAggregateIndex("COUNT(v) alone",
		"SELECT g, COUNT(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|0", "2|2"},
		[]string{"1|0", "2|2", "3|0"})
	w.Want("COUNT(*) alone", "SELECT g, COUNT(*) FROM t GROUP BY g ORDER BY g",
		[]string{"1|2", "2|2", "3|1"})

	// Single-group spellings, read from the grouped index as a point scan
	// (`AISCAN(... [EQUALS ...]) | ON EMPTY NULL`, aggregate-empty-table.yamsql);
	// a group with no entry is the ungrouped empty answer, NULL.
	w.WantAggregateIndex("SUM of one residual-zero group", "SELECT SUM(v) FROM t WHERE g = 1",
		[]string{"0"}, []string{"NULL"})
	w.Want("SUM of one cancelling group", "SELECT SUM(v) FROM t WHERE g = 2", []string{"0"})
	w.Want("SUM of one all-NULL group", "SELECT SUM(v) FROM t WHERE g = 3", []string{"NULL"})
	w.Want("SUM of an absent group", "SELECT SUM(v) FROM t WHERE g = 99", []string{"NULL"})
}

// TestFDB_AggregateIndexSum_ReadAlone: a SUM index answers with no COUNT index
// beside it, as Java's does.
func TestFDB_AggregateIndexSum_ReadAlone(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := testkit.NewTwin(t, ctx, "/FRL/testdb_sum_nocountv", "sumnocv",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_sum_v_g AS SELECT SUM(v) FROM t GROUP BY g ")
	w.Exec("INSERT INTO t (id, g, v) VALUES (101, 1, 9), (102, 1, NULL), (201, 2, 4), (202, 2, -4)")
	w.Exec("UPDATE t SET v = NULL WHERE id = 101")
	q := "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g"
	if plan := w.Explain(q); !strings.Contains(plan, "AggregateIndex(SUM, T_SUM_V_G") {
		t.Errorf("SUM is not read from its index alone\n  q: %s\n  plan: %s", q, plan)
	}
	w.WantAggregateIndex("SUM", q, []string{"1|0", "2|0"}, []string{"1|NULL", "2|0"})
}
