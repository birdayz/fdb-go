package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// sortedRowStrings runs q and returns each row rendered by fmt, sorted, so an
// expected multiset of rows compares as one string.
func SortedRowStrings(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprint(cells))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	return got
}
