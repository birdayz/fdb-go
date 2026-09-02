package sqldriver_test

// Two aggregates whose operands differ only in the CASE OF A STRING LITERAL must
// compute different things.
//
// They did not. Operand resolution matched a parsed aggregate column to its
// aggregate call by case-folding the operand's RENDERED TEXT — and that text
// carries identifiers (already upper-normalized at that layer) alongside string
// literals, which are DATA. `EqualFold` cannot tell the two apart, so
// `COUNT(CASE WHEN "Region" = 'us' …)` and `COUNT(CASE WHEN "Region" = 'US' …)`
// each matched BOTH calls, each wrote both operand slots, and the second write
// clobbered the first. Last-wins: both columns returned the value of whichever
// aggregate came last (RFC-241).
//
// THE REVERSAL IS NOT DECORATION. With the defect present the pair and its
// reversal disagree about WHICH value both columns take — ('us','US') answered
// [2,2] while ('US','us') answered [0,0] — so a single-order test cannot express
// the defect. It would have passed on one of the two orders by luck.
//
// The last two arms are the controls that localise it. A pair differing by more
// than case answers correctly, so the mechanism is not "two aggregates"; and the
// same case-only pair OUTSIDE an aggregate answers correctly, so it is not CASE
// and not string comparison. Both were green while the defect was live.
//
// The label authority was never wrong: EXPLAIN showed the projection reading
// ordinals #0 and #1 under two correct, distinct names throughout. That is what
// made this invisible — the plan and the result metadata both looked right.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_AggregateOperandDistinguishesLiteralCase(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_agg_literal_case")
	exec := func(db *sql.DB, stmt string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	exec(setup, "CREATE DATABASE /testdb_agg_literal_case")
	exec(setup, "CREATE SCHEMA TEMPLATE agg_literal_case "+
		`CREATE TABLE sales (id BIGINT, "Region" STRING, "Amount" BIGINT, plain BIGINT, PRIMARY KEY (id))`)
	exec(setup, "CREATE SCHEMA /testdb_agg_literal_case/s WITH TEMPLATE agg_literal_case")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_agg_literal_case?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Two 'US' rows and one 'EU'. No row is 'us', so the lower-case arm of every
	// pair below must be empty — 0 for COUNT, NULL for SUM.
	exec(db, "INSERT INTO sales VALUES (1, 'US', 100, 1)")
	exec(db, "INSERT INTO sales VALUES (2, 'US', 200, 2)")
	exec(db, "INSERT INTO sales VALUES (3, 'EU', 300, 3)")

	rowsOf := func(t *testing.T, q string) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		var out []string
		for rows.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			parts := make([]string, len(cells))
			for i, c := range cells {
				parts[i] = fmt.Sprintf("%v", c)
			}
			out = append(out, "["+strings.Join(parts, " ")+"]")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return strings.Join(out, " ")
	}

	for _, tc := range []struct {
		name string
		q    string
		want string
		why  string
	}{
		{
			name: "count_pair_lower_then_upper",
			q: `SELECT COUNT(CASE WHEN "Region" = 'us' THEN 1 END), ` +
				`COUNT(CASE WHEN "Region" = 'US' THEN 1 END) FROM sales`,
			want: "[0 2]",
			why:  "answered [2 2] with the fold: both columns took the LAST aggregate's operand",
		},
		{
			name: "count_pair_upper_then_lower",
			q: `SELECT COUNT(CASE WHEN "Region" = 'US' THEN 1 END), ` +
				`COUNT(CASE WHEN "Region" = 'us' THEN 1 END) FROM sales`,
			want: "[2 0]",
			why: "the REVERSAL. Answered [0 0] with the fold — the two orders disagreed about " +
				"which value both columns took, which is why one order alone cannot pin this",
		},
		{
			name: "sum_pair_case_only",
			q: `SELECT SUM(CASE WHEN "Region" = 'us' THEN "Amount" END), ` +
				`SUM(CASE WHEN "Region" = 'US' THEN "Amount" END) FROM sales`,
			want: "[<nil> 300]",
			why: "answered [300 300] with the fold. SUM over no matching row is NULL, not 0 — " +
				"the empty arm proves the operand really is the 'us' one and not a zeroed slot",
		},
		{
			name: "literal_on_the_left",
			q: `SELECT COUNT(CASE WHEN 'us' = "Region" THEN 1 END), ` +
				`COUNT(CASE WHEN 'US' = "Region" THEN 1 END) FROM sales`,
			want: "[0 2]",
			why:  "the defect is in the operand's rendered text, so it does not care which side the literal sits on",
		},
		{
			name: "grouped_pair",
			q: `SELECT plain, COUNT(CASE WHEN "Region" = 'us' THEN 1 END), ` +
				`COUNT(CASE WHEN "Region" = 'US' THEN 1 END) FROM sales GROUP BY plain ORDER BY plain`,
			want: "[1 0 1] [2 0 1] [3 0 0]",
			why:  "answered [1 1 1] [2 1 1] [3 0 0] with the fold — the grouped path shares the resolution",
		},
		{
			name: "control_literals_differ_by_more_than_case",
			q: `SELECT COUNT(CASE WHEN "Region" = 'EU' THEN 1 END), ` +
				`COUNT(CASE WHEN "Region" = 'US' THEN 1 END) FROM sales`,
			want: "[1 2]",
			why:  "green while the defect was live: the mechanism is the FOLD, not having two aggregates",
		},
		{
			name: "control_same_pair_outside_an_aggregate",
			q: `SELECT id, CASE WHEN "Region" = 'us' THEN 1 END, ` +
				`CASE WHEN "Region" = 'US' THEN 1 END FROM sales ORDER BY id`,
			want: "[1 <nil> 1] [2 <nil> 1] [3 <nil> <nil>]",
			why:  "green while the defect was live: localises it to aggregate operand resolution, not CASE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowsOf(t, tc.q); got != tc.want {
				t.Errorf("%s\n  query: %s\n  got:  %s\n  want: %s\n  %s", tc.name, tc.q, got, tc.want, tc.why)
			}
		})
	}
}

// A derived table reaches the aggregate builder through a DIFFERENT path, one
// that passes a NON-EMPTY strip prefix — `buildSelectShell(op, sq,
// strings.ToUpper(sq.tableName)+".")`. The producer therefore strips a qualifier
// from the operand text that the consumer reconstructs by a different rule, so
// producer and consumer spellings can legitimately differ there.
//
// That makes this the one arm testing that RFC-241's provenance check does NOT
// fire when it should not. Every other assertion in this file checks the fix
// produces the right rows; this one checks it did not turn a correct query into
// a loud error. Correct-or-loud is only correct if "loud" stays reserved for
// actually-wrong states.
func TestFDB_AggregateOperandResolvesThroughADerivedTable(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_agg_derived_strip")
	exec := func(db *sql.DB, stmt string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	exec(setup, "CREATE DATABASE /testdb_agg_derived_strip")
	exec(setup, "CREATE SCHEMA TEMPLATE agg_derived_strip "+
		`CREATE TABLE sales (id BIGINT, "Region" STRING, "Amount" BIGINT, plain BIGINT, PRIMARY KEY (id))`)
	exec(setup, "CREATE SCHEMA /testdb_agg_derived_strip/s WITH TEMPLATE agg_derived_strip")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_agg_derived_strip?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	exec(db, "INSERT INTO sales VALUES (1, 'US', 100, 1)")
	exec(db, "INSERT INTO sales VALUES (2, 'US', 200, 2)")
	exec(db, "INSERT INTO sales VALUES (3, 'EU', 300, 3)")

	for _, tc := range []struct {
		name string
		q    string
		want string
	}{
		{
			name: "qualified_operand_through_a_derived_table",
			q:    `SELECT d.t FROM (SELECT SUM(s."Amount") AS t FROM sales s) d`,
			want: "600",
		},
		{
			name: "grouped_qualified_operand_through_a_derived_table",
			q: `SELECT d.r, d.t FROM (SELECT s."Region" AS r, SUM(s."Amount") AS t ` +
				`FROM sales s GROUP BY s."Region") d ORDER BY d.r`,
			want: "EU/300 US/300",
		},
		{
			name: "qualified_operand_with_having",
			q: `SELECT d.r FROM (SELECT s."Region" AS r, SUM(s."Amount") AS t ` +
				`FROM sales s GROUP BY s."Region" HAVING SUM(s."Amount") > 250) d ORDER BY d.r`,
			want: "EU US",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.q)
			if err != nil {
				t.Fatalf("query %q errored, but this shape is CORRECT and must answer — "+
					"RFC-241's provenance check has become loud on a valid query, which is the "+
					"regression this arm exists to catch: %v", tc.q, err)
			}
			defer rows.Close()
			cols, _ := rows.Columns()
			var out []string
			for rows.Next() {
				cells := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan %q: %v", tc.q, err)
				}
				parts := make([]string, len(cells))
				for i, c := range cells {
					parts[i] = fmt.Sprintf("%v", c)
				}
				out = append(out, strings.Join(parts, "/"))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err %q: %v", tc.q, err)
			}
			if got := strings.Join(out, " "); got != tc.want {
				t.Errorf("%s\n  query: %s\n  got:  %s\n  want: %s", tc.name, tc.q, got, tc.want)
			}
		})
	}
}
