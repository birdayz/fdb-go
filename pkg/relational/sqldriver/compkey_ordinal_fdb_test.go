package sqldriver_test

// Regression test for the intersection/merge-sort comparison-key extractor.
// The multi-aggregate intersection plan (MultiIntersection, built by
// tryMultiAggregateIntersection over two+ aggregate indexes sharing a group
// column) merges its child aggregate-index scans on a comparison key = the
// GROUP column. That key is extracted by evaluating against the child row's
// authoritative ordinal PositionalRow (compKeyEvalArg → FieldValue.Evaluate →
// evaluateOrdinal), not a name-keyed Datum map — the name-keyed reader
// (multiIntersectionCompKeyFunc used to read against) has been deleted, so
// ordinal evaluation is the only path. This test asserts the extractor
// produces correct rows. The schema is unique to this test, so no cached
// plan is shared.
//
// Reachability note (the other three routed sites): the two mergeSortCursor
// sites (isBetter/extractKey) and executor.go's intersectionCompKeyFunc are
// NOT reachable by any executing SQL on the supported surface — UNION
// DISTINCT / INTERSECT are 0AF00 / 42601 reach gaps, ordered UNION ALL plans
// as InMemorySort-over-Union (not a merge-sort union), and the primary-key
// intersection plan is generated but never cost-selected. They exist for the
// correctness contract but carry no executing-SQL pin (there is nothing to
// run). This test pins the one reachable site.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_CompKeyOrdinal(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// Two+ aggregate indexes sharing the group column g → the multi-aggregate
	// intersection (MultiIntersection) whose comparison key is g.
	db := setupPlanShapeDB(t, "ck",
		"CREATE TABLE ga (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_by_g AS SELECT SUM(v) FROM ga GROUP BY g "+
			"CREATE INDEX min_by_g AS SELECT MIN(v) FROM ga GROUP BY g "+
			"CREATE INDEX max_by_g AS SELECT MAX(v) FROM ga GROUP BY g")
	// g=1: v=10,20,30 → SUM=60 MIN=10 MAX=30 ; g=2: v=25,45 → SUM=70 MIN=25 MAX=45
	mwjoMustExec(t, db, ctx, "INSERT INTO ga VALUES (1, 1, 10), (2, 1, 20), (3, 1, 30), (4, 2, 25), (5, 2, 45)")

	// The two-aggregate grouped query plans as MultiIntersection (each aggregate index
	// supplies one column; the intersection merges on the shared group key g). Pin the
	// realization so the test proves the comparison-key extractor actually fires.
	twoAgg := "SELECT g, SUM(v), MAX(v) FROM ga GROUP BY g"
	if plan := planExplainVia(t, ctx, db, twoAgg); !strings.Contains(plan, "MultiIntersection") {
		t.Fatalf("two-aggregate grouped query must plan as MultiIntersection (exercises the comp-key extractor), got: %s", plan)
	}

	rowsGMM := func(t *testing.T, q string, n int) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			cols := make([]int64, n)
			ptrs := make([]any, n)
			for i := range cols {
				ptrs[i] = &cols[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			parts := make([]string, n)
			for i, c := range cols {
				parts[i] = fmt.Sprintf("%d", c)
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	// (1) Two-aggregate MultiIntersection: SUM + MAX merged on group key g.
	t.Run("two_aggregate_sum_max", func(t *testing.T) {
		if got := rowsGMM(t, twoAgg, 3); got != "1|60|30 2|70|45" {
			t.Errorf("g,SUM,MAX = %q, want %q", got, "1|60|30 2|70|45")
		}
	})

	// (2) Three-aggregate MultiIntersection: SUM + MIN + MAX merged on g — three child
	// scans intersected on the same comparison key.
	t.Run("three_aggregate_sum_min_max", func(t *testing.T) {
		threeAgg := "SELECT g, SUM(v), MIN(v), MAX(v) FROM ga GROUP BY g"
		if plan := planExplainVia(t, ctx, db, threeAgg); !strings.Contains(plan, "MultiIntersection") {
			t.Fatalf("three-aggregate grouped query must plan as MultiIntersection, got: %s", plan)
		}
		if got := rowsGMM(t, threeAgg, 4); got != "1|60|10|30 2|70|25|45" {
			t.Errorf("g,SUM,MIN,MAX = %q, want %q", got, "1|60|10|30 2|70|25|45")
		}
	})
}
