package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestFDB_GroupedSubqueryProjectsAnOuterColumn: a grouped block nested in
// another may read a column of the ENCLOSING block in its select list, bare or
// inside an aggregate. Java's generateGroupBy asks every output expression
// isComposableFrom the grouping keys and aggregates, and a value correlated
// only to outer quantifiers is composable (LogicalOperator.java:436-439, the
// constantCorrelations arm); the corpus pins it (subquery-tests.yamsql and
// documentation-queries/subqueries-documentation-queries.yamsql, "correlations
// are allowed inside a nested subquery with group by").
//
// Three defects stood between Go and that answer, each red here before its fix:
//   - the parser classed the bare column as a grouping-key read, which the
//     grouping check then found among neither the keys nor the block's fields
//     (42703); it is a correlation, computed above the aggregate like any
//     correlated expression (correlatedGroupColumnsToComputed);
//   - a lateral derived table projecting any outer read over its aggregation
//     failed at execution ("current QOV is not this layout's exact carrier
//     handle"): the FlatMap's correlated-comparison normalization relinked the
//     projection but not the aggregation below it, whose grouping keys stayed
//     pinned to an input it no longer read;
//   - an aggregate over the outer column (`MAX(a.x)`) then failed that relink,
//     which refused every operand root the input did not provide, outer
//     correlations included.
//
// The EXISTS shapes are the corpus's own (their group by does not change
// emptiness); the derived-table shapes project the column, so they pin the
// VALUE it carries per outer row, the column's name, and its position among the
// aggregate's outputs. derived_correlated_where_only is the control that
// worked before: an outer read in the WHERE alone.
func TestFDB_GroupedSubqueryProjectsAnOuterColumn(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/FRL/testdb_grouped_outer_column", "groupedoutercol", `
		create table a(ida integer, x integer, primary key(ida))
		create table b(idb integer, q integer, r integer, primary key(idb))
		create index ib as select q from b`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mwjoMustExec(t, db, ctx, "INSERT INTO A VALUES (1, 1), (2, 2), (3, 3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO B VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300)")

	// For outer x, the groups are the q above 10*x: x=1 → q 20, 30; x=2 → q 30;
	// x=3 → none.
	cases := []struct {
		name  string
		query string
		cols  []string
		want  []string
	}{
		{
			"exists_qualified", "select x from a where exists (select a.x, max(idb) from b where q > a.x group by q)",
			[]string{"X"},
			[]string{"1", "2", "3"},
		},
		{
			"exists_bare", "select x from a where exists (select x, max(idb) from b where q > x group by q)",
			[]string{"X"},
			[]string{"1", "2", "3"},
		},
		{
			"derived_qualified", "select x, sq.ax, sq.m from a, (select a.x as ax, max(idb) as m from b where q > a.x * 10 group by q) as sq",
			[]string{"X", "AX", "M"},
			[]string{"1 1 2", "1 1 3", "2 2 3"},
		},
		{
			"derived_bare", "select x, sq.ax, sq.m from a, (select x as ax, max(idb) as m from b where q > x * 10 group by q) as sq",
			[]string{"X", "AX", "M"},
			[]string{"1 1 2", "1 1 3", "2 2 3"},
		},
		{
			"derived_after_the_aggregate", "select x, sq.m, sq.ax from a, (select max(idb) as m, a.x as ax from b where q > a.x * 10 group by q) as sq",
			[]string{"X", "M", "AX"},
			[]string{"1 2 1", "1 3 1", "2 3 2"},
		},
		{
			"derived_beside_a_key", "select sq.q, sq.ax from a, (select q, a.x as ax from b where q > a.x * 10 group by q) as sq",
			[]string{"Q", "AX"},
			[]string{"20 1", "30 1", "30 2"},
		},
		{
			"derived_correlated_where_only", "select x, sq.q, sq.m from a, (select q, max(idb) as m from b where q > a.x * 10 group by q) as sq",
			[]string{"X", "Q", "M"},
			[]string{"1 20 2", "1 30 3", "2 30 3"},
		},
		{
			"derived_expression_control", "select x, sq.z, sq.m from a, (select a.x + 0 as z, max(idb) as m from b where q > a.x * 10 group by q) as sq",
			[]string{"X", "Z", "M"},
			[]string{"1 1 2", "1 1 3", "2 2 3"},
		},
		// The corpus's other two EXISTS shapes read the outer column inside an
		// aggregate; projected, the aggregate of a per-outer-row constant is it.
		{
			"exists_aggregate_of_outer", "select x from a where exists (select max(x), max(idb) from b where q > x group by q)",
			[]string{"X"},
			[]string{"1", "2", "3"},
		},
		{
			"derived_aggregate_of_outer", "select x, sq.mx, sq.m from a, (select max(a.x) as mx, max(idb) as m from b where q > a.x * 10 group by q) as sq",
			[]string{"X", "MX", "M"},
			[]string{"1 1 2", "1 1 3", "2 2 3"},
		},
		// Unaliased, the column keeps its name, as a grouping-key read does.
		{
			"derived_unaliased", "select sq.x, sq.m from a, (select a.x, max(idb) as m from b where q > a.x * 10 group by q) as sq",
			[]string{"X", "M"},
			[]string{"1 2", "1 3", "2 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("%s: %v", tc.query, err)
			}
			cols, got := groupedOuterColumnRows(t, rows)
			if strings.Join(cols, ",") != strings.Join(tc.cols, ",") {
				t.Errorf("columns %v, want %v", cols, tc.cols)
			}
			sort.Strings(got)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("rows %v, want %v", got, tc.want)
			}
		})
	}
}

func groupedOuterColumnRows(t *testing.T, rows *sql.Rows) ([]string, []string) {
	t.Helper()
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
	return cols, out
}
