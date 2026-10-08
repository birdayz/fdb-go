package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// runShape renders a query's column names and rows as one comparable string,
// scanning into `any` so a struct-valued cell is REPORTED rather than converted
// into a scan error. That is deliberate: scanning into int64 turns the defect
// into a type complaint, and the point of this matrix is to show what a client
// that does not demand a type silently receives.
func RunShape(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "ERROR(columns): " + err.Error()
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "ERROR(scan): " + err.Error()
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(cells, " "))
	}
	if err := rows.Err(); err != nil {
		return "ERROR(iterate): " + err.Error()
	}
	return strings.Join(cols, ",") + "|" + strings.Join(out, ";")
}
