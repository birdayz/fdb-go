package sqldriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFDB_EverAggregatesAreServedFromTheirIndexes: a query over min_ever(…) or
// max_ever(…) reads the MIN_EVER / MAX_EVER index Java's
// AggregateIndexExpansionVisitor matches it to (IndexOnlyAggregateValue
// .MinEverFn / MaxEverFn, AggregateIndexExpansionVisitor.java:369-380); no
// streaming accumulator computes these aggregates, so before the candidate
// existed the query had no plan (0AF00). The corpus file
// aggregate-index-tests.yamsql runs the same shapes (`select min_ever(col3) from
// t2`, planned `AISCAN(MV7 <,> BY_GROUP …) | ON EMPTY NULL`).
//
// The ungrouped index is one group, the whole table: over a table that never
// had a row the index holds no entry, and the query still answers its one row,
// NULL. An _EVER extremum is the index's own: a delete does not lower it.
func TestFDB_EverAggregatesAreServedFromTheirIndexes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := setupPlanShapeDB(t, "everindexquery",
		"CREATE TABLE t2 (id BIGINT, col1 BIGINT, col2 BIGINT, col3 BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX mv4 AS SELECT min_ever(col3) FROM t2 GROUP BY col1, col2 "+
			"CREATE INDEX mv5 AS SELECT max_ever(col3) FROM t2 GROUP BY col1, col2 "+
			"CREATE INDEX mv7 AS SELECT min_ever(col3) FROM t2 "+
			"CREATE INDEX mv8 AS SELECT max_ever(col3) FROM t2")

	rowsOf := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			dest := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range dest {
				ptrs[i] = &dest[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			cells := make([]string, len(dest))
			for i, v := range dest {
				cells[i] = fmt.Sprint(v)
			}
			out = append(out, strings.Join(cells, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	check := func(t *testing.T, q string, want ...string) {
		t.Helper()
		if plan := planExplainVia(t, ctx, db, q); !strings.Contains(plan, "AggregateIndex") {
			t.Errorf("%s is not served by its _EVER index: %s", q, plan)
		}
		if got := rowsOf(t, q); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}

	// Empty: the ungrouped queries answer one NULL row, the grouped none.
	check(t, "SELECT min_ever(col3) FROM t2", "<nil>")
	check(t, "SELECT max_ever(col3) FROM t2", "<nil>")
	check(t, "SELECT col1, col2, min_ever(col3) FROM t2 GROUP BY col1, col2")

	mwjoMustExec(t, db, ctx, "INSERT INTO t2 VALUES (1, 1, 1, 100), (2, 1, 1, 1), (3, 1, 2, 2), "+
		"(4, 1, 2, 200), (5, 2, 1, 200), (6, 2, 1, 3), (7, 2, 1, 400)")
	check(t, "SELECT min_ever(col3) FROM t2", "1")
	check(t, "SELECT max_ever(col3) FROM t2", "400")
	check(t, "SELECT col1, col2, min_ever(col3) FROM t2 GROUP BY col1, col2 ORDER BY col1, col2",
		"1 1 1", "1 2 2", "2 1 3")
	check(t, "SELECT col1, col2, max_ever(col3) FROM t2 GROUP BY col1, col2 ORDER BY col1, col2",
		"1 1 100", "1 2 200", "2 1 400")
	// A scan bound on the leading grouping column.
	check(t, "SELECT col2, max_ever(col3) FROM t2 WHERE col1 = 2 GROUP BY col1, col2", "1 400")

	// The extremum is the index's: deleting the rows that set it does not lower
	// it, and deleting every row leaves the ungrouped entry in place.
	mwjoMustExec(t, db, ctx, "DELETE FROM t2 WHERE id = 7")
	check(t, "SELECT max_ever(col3) FROM t2", "400")
	mwjoMustExec(t, db, ctx, "DELETE FROM t2")
	check(t, "SELECT min_ever(col3) FROM t2", "1")
	check(t, "SELECT col1, col2, max_ever(col3) FROM t2 GROUP BY col1, col2 ORDER BY col1, col2",
		"1 1 100", "1 2 200", "2 1 400")
}
