package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// `SELECT *` under GROUP BY was refused wholesale, and the refusal was minted
// before anything knew what the star expands to.
//
// Java runs the two steps in the opposite order from Go, and the order is the
// entire rule. The star expands FIRST, against the FROM-side operators
// (ExpressionVisitor.java:145-147 -> SemanticAnalyzer.expandStar:321-368), and
// every EXPANDED item is validated against the grouping keys afterwards
// (LogicalOperator.java:436-439, isComposableFrom). Go refused at SELECT-list
// classification, which is pure parse-tree work with no FROM clause in hand, so
// the star was never expanded at all.
//
// The consequence is that one shape's correct answer stood in for another's:
//
//	select * from T1 group by col1                          -- 42803, correct
//	select * from (select col1 from T1) as X group by col1  -- 42803, WRONG
//
// The first genuinely expands to ungrouped columns. The second expands to
// exactly the grouping key, and Java answers it
// (groupby-tests.yamsql:92-96). A blanket refusal is right for the first and
// wrong for the second, and nothing could tell them apart without expanding.
//
// This is the gap booked as engine-gap:star-group-by-expansion (CQ-72). It was
// invisible for as long as it existed because groupby-tests.yamsql was SKIPPED
// ENTIRELY at queries=0 — its schema template indexes a three-segment nested
// path, so CREATE SCHEMA TEMPLATE died and the whole file went dark. A skipped
// file reports no failures and reads exactly like a passing one.
//
// The test drives BOTH directions on purpose. An expansion that is merely
// permissive passes the positive arms and silently accepts
// `select * from T1 group by col1`, which is a wrong-rows bug rather than an
// error-code one: without the per-column check the GROUP BY is dropped and
// every source row comes back.
func TestFDB_StarUnderGroupByExpandsBeforeItIsValidated(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_stargb")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_stargb")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE stargb_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_stargb/s WITH TEMPLATE stargb_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_stargb?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Two groups with five and three members. The row COUNT is what separates a
	// real grouping from a dropped one: a GROUP BY silently ignored returns
	// eight rows, and every one of its COL1 values is still 10 or 20, so a
	// value-only assertion would pass with the grouping gone.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES "+
		"(1, 10, 1), (2, 10, 2), (3, 10, 3), (4, 10, 4), (5, 10, 5), "+
		"(6, 20, 6), (7, 20, 7), (8, 20, 8)")

	// queryCols returns the result's COLUMN NAMES alongside its rows. The names
	// are load-bearing here and are not checkable from the values: the aliased
	// arm below turns on the output being COL1 rather than Y.
	queryCols := func(t *testing.T, q string) ([]string, []int64) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		names, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return names, out
	}

	sortedEqual := func(t *testing.T, q string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%q: got %d rows %v, want %d rows %v — a row count that is "+
				"neither the group count nor an error is a DROPPED GROUP BY, not a "+
				"near miss", q, len(got), got, len(want), want)
		}
		seen := map[int64]int{}
		for _, v := range got {
			seen[v]++
		}
		for _, v := range want {
			if seen[v] == 0 {
				t.Fatalf("%q: got %v, want %v", q, got, want)
			}
			seen[v]--
		}
	}

	t.Run("derived table whose expansion IS the grouping key", func(t *testing.T) {
		// The defect's exact shape. The star expands to COL1 alone, which is the
		// grouping key, so there is nothing ungrouped to refuse.
		const q = "SELECT * FROM (SELECT col1 FROM t1) AS X GROUP BY col1"
		names, got := queryCols(t, q)
		sortedEqual(t, q, got, 10, 20)
		if len(names) != 1 || !strings.EqualFold(names[0], "COL1") {
			t.Fatalf("%q: columns = %v, want [COL1]", q, names)
		}
	})

	t.Run("group key alias does not rename the expansion", func(t *testing.T) {
		// `AS Y` names the GROUP KEY, not the projected column, and the star is
		// blind to it. Java files an aliased grouping item as an
		// EphemeralExpression (ExpressionVisitor.java:252-256) and splices it
		// into the operator output for NAME RESOLUTION only
		// (QueryVisitor.java:281-286); expandStar's nonEphemeralVisible filter
		// (Expressions.java:163-166) drops it again before expanding. So the
		// output is COL1.
		//
		// This arm is what proves the expansion reads the SOURCE and not the
		// GROUP BY: an implementation that expanded the star from the grouping
		// keys would answer the right VALUES here and name the column Y.
		const q = "SELECT * FROM (SELECT col1 FROM t1) AS X GROUP BY col1 AS Y"
		names, got := queryCols(t, q)
		sortedEqual(t, q, got, 10, 20)
		if len(names) != 1 || !strings.EqualFold(names[0], "COL1") {
			t.Fatalf("%q: columns = %v, want [COL1] — the group-by alias must not "+
				"reach the star expansion", q, names)
		}
	})

	// The refusals. These are the same 42803 as before the fix, but they are now
	// minted PER EXPANDED COLUMN by validateGroupByProjection instead of by a
	// blanket refusal on the star. Losing them is the wrong-rows direction of
	// this change, not a cosmetic regression.
	refused := []struct {
		name string
		q    string
	}{
		{
			// The base table expands to ID, COL1, COL2; ID and COL2 are not
			// grouped. groupby-tests.yamsql:74-75 asserts GROUPING_ERROR.
			name: "base table expands to ungrouped columns",
			q:    "SELECT * FROM t1 GROUP BY col1",
		},
		{
			// A derived table is not a free pass either — this one carries a
			// second column that is not the grouping key. Without this arm an
			// implementation that skipped validation for derived sources
			// entirely would pass every other arm.
			name: "derived table carrying an ungrouped column",
			q:    "SELECT * FROM (SELECT col1, col2 FROM t1) AS X GROUP BY col1",
		},
		{
			// Named explicitly rather than via a star, so the two channels are
			// pinned to the same verdict. groupby-tests.yamsql:139-140.
			name: "named ungrouped column",
			q:    "SELECT col2 FROM (SELECT col1, col2 FROM t1) AS X GROUP BY col1",
		},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.q)
			if err == nil {
				defer rows.Close()
				n := 0
				for rows.Next() {
					n++
				}
				t.Fatalf("%q: expected 42803, got %d rows — an accepted star over "+
					"ungrouped columns means the GROUP BY was dropped, which returns "+
					"every source row rather than one per group", tc.q, n)
			}
			if !strings.Contains(err.Error(), "42803") {
				t.Fatalf("%q: got %v, want 42803 (GROUPING_ERROR)", tc.q, err)
			}
		})
	}
}
