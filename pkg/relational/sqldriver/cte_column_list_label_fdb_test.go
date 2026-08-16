package sqldriver_test

// A CTE column list renames the CTE BODY's columns. The main query is then
// free to rename them again with its own `AS`, and that second name is the
// one the result set carries.
//
// The label is a dimension no CTE test covered: TestFDB_CascadesCTEColumnAliases
// and TestFDB_CTEChainedColumnAliases both read VALUES out of a CTE with a
// column list and never look at rows.Columns().
//
// THIS TEST DOES NOT DISCRIMINATE THE DERIVATION IT WAS WRITTEN FOR, and
// saying so is the point. query.exactLogicalResultType's LogicalCTE case
// applied the column list to the STATEMENT's row rather than the body's;
// TestCTEColumnAliasesRenameTheBodyNotTheStatement is the pin for that, and it
// is red without the fix. These three queries pass either way — the SQL
// surface reaches its result-set labels through a different authority. That
// was measured, not assumed: instrumenting the pre-fix branch to print
// whenever the rename actually changed a name or the arity check disagreed,
// then running the whole sqldriver and embedded suites, produced 0 hits in
// 24777 lines of output (7911 PASS lines as the positive control that the
// search itself was well-formed).
//
// So what this test is FOR is the label contract itself, which nothing else
// states for a CTE column list: if the label authority is ever routed through
// the exact derivation — the direction this work is moving — the contract is
// already written down and a regression lands on it rather than on a user.

import (
	"context"
	"testing"
)

func TestFDB_CTEColumnListDoesNotOverwriteTheMainQueryLabel(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		query    string
		wantCols []string
		wantRows int
	}{
		{
			// The main query renames a column the CTE list already renamed.
			// Applying the list to the statement reported PRODUCT here.
			name: "main_alias_wins_over_the_column_list",
			query: "WITH priced(product, cost) AS (SELECT name, price FROM Item) " +
				"SELECT product AS label FROM priced ORDER BY product",
			wantCols: []string{"LABEL"},
			wantRows: 3,
		},
		{
			// Narrower main than the column list: legal, and the arity check
			// belongs to the body, not to this row.
			name: "main_narrower_than_the_column_list",
			query: "WITH priced(product, cost) AS (SELECT name, price FROM Item) " +
				"SELECT product FROM priced ORDER BY product",
			wantCols: []string{"PRODUCT"},
			wantRows: 3,
		},
		{
			// Wider main than the column list, with one slot renamed again:
			// the two names must not be swapped or collapsed.
			name: "main_wider_with_one_rename",
			query: "WITH priced(product, cost) AS (SELECT name, price FROM Item) " +
				"SELECT product, cost AS amount FROM priced ORDER BY product",
			wantCols: []string{"PRODUCT", "AMOUNT"},
			wantRows: 3,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := cascadesDB.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("query: %v\n  sql: %s", err, tc.query)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			if len(cols) != len(tc.wantCols) {
				t.Fatalf("columns = %v, want %v", cols, tc.wantCols)
			}
			for i, want := range tc.wantCols {
				if cols[i] != want {
					t.Errorf("column %d = %q, want %q", i, cols[i], want)
				}
			}
			// Read through, so a label that is right while the row underneath
			// it is misaligned still trips.
			n := 0
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan: %v", err)
				}
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if n != tc.wantRows {
				t.Errorf("got %d rows, want %d", n, tc.wantRows)
			}
		})
	}
}
