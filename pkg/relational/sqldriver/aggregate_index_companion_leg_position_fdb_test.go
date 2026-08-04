package sqldriver_test

// The multi-aggregate merge bakes its grouping-column pick-ups positionally,
// and the position it must read is the DRIVING leg's span — not leg 0's.
//
// The two coincide in the common case: when no selected aggregate is itself the
// group-existence companion, the companion is PREPENDED as a new leg 0 and
// drives from there. They diverge when the query already selects the
// companion's own COUNT(*) over the same grouping key, because then that leg is
// designated IN PLACE and the driving leg is wherever the select list happens to
// have put it. Reading the grouping key from leg 0 in that case reads it from a
// non-driving aggregate leg, whose absent filler is all-NULL — so every group
// that leg has no entry for loses its KEY, not just its aggregate.
//
// The axis under test is therefore the POSITION of the designated leg, and the
// only thing that varies it is select-list order. Both orders are asserted here
// against the same data; before the fix, `SUM(v), COUNT(*)` returned the
// grouping key as NULL for every all-NULL group while `COUNT(*), SUM(v)` was
// correct, which is exactly the shape that makes this survivable in a suite
// that happens to write its aggregates in the lucky order.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_AggregateIndexCompanionLegPosition_GroupingKeysSurviveDesignatedLegPosition(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_agglegpos")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_agglegpos")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE agglegpos "+
			"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_agglegpos/s WITH TEMPLATE agglegpos")
	dsn := fmt.Sprintf("fdbsql:///testdb_agglegpos?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// THREE groups absent from the SUM index (every v NULL, so SUM writes no
	// entry) and one present in both. More than one absent group is deliberate:
	// with a single such group the corrupted rows collapse to one and the result
	// can still be mistaken for a plausible answer. With three, the bug produces
	// three rows that all claim the same NULL key.
	mwjoMustExec(t, db, ctx, "INSERT INTO ai (pk,g,v) VALUES "+
		"(1,10,1),(2,10,2),"+ // g=10 present in BOTH legs — SUM 3, COUNT(*) 2
		"(3,11,NULL),"+ // g=11 absent from the SUM leg
		"(4,12,NULL),"+ // g=12 absent from the SUM leg
		"(5,13,NULL),(6,13,NULL)") // g=13 absent from the SUM leg, two rows

	// Both select-list orders over the same data. `sum-first` puts the
	// companion COUNT(*) at drivingLeg=1; `count-first` puts it at drivingLeg=0.
	// The grouping VALUES are asserted, not just the aggregates — the defect
	// destroys the key while leaving the counts perfectly correct, so an
	// assertion on aggregates alone stays green with the bug fully present.
	check := func(name, q, want string) {
		t.Run(name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "GroupExistenceMerge(") {
				t.Fatalf("the query is no longer answered by the companion-joined plan, so "+
					"this test no longer covers the designated-leg-position dimension — "+
					"re-arm it, do not relax it.\n  query: %s\n  plan : %s", q, plan)
			}
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var g, first, second sql.NullInt64
				if err := rows.Scan(&g, &first, &second); err != nil {
					t.Fatalf("scan: %v", err)
				}
				out = append(out, fmt.Sprintf("[%s %s %s]",
					legPosNullableInt(g), legPosNullableInt(first), legPosNullableInt(second)))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			sort.Strings(out)
			if got := strings.Join(out, " "); got != want {
				t.Fatalf("multi-aggregate merge diverges from the oracle.\n  query: %s\n"+
					"  got : %s\n  want: %s\n"+
					"The grouping key must be read from the DRIVING leg's span. When the "+
					"selected COUNT(*) is itself the companion it is designated in place, so "+
					"the driving leg sits wherever the select list put it; reading leg 0 "+
					"instead takes the key from a non-driving aggregate leg whose absent "+
					"filler is all-NULL, and every group that leg lacks (g=11,12,13 here) "+
					"reports a NULL key.", q, got, want)
			}
		})
	}

	// g=11,12,13 have no SUM entry: their SUM is NULL and their COUNT(*) comes
	// from the companion. Their KEYS must still be 11, 12, 13.
	check("sum-first-designated-leg-is-1",
		"SELECT g, SUM(v), COUNT(*) FROM ai GROUP BY g",
		"[10 3 2] [11 NULL 1] [12 NULL 1] [13 NULL 2]")
	check("count-first-designated-leg-is-0",
		"SELECT g, COUNT(*), SUM(v) FROM ai GROUP BY g",
		"[10 2 3] [11 1 NULL] [12 1 NULL] [13 2 NULL]")
}

func legPosNullableInt(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", v.Int64)
}
