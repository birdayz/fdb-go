package testkit

// Second metamorphic axis: ORDER BY / LIMIT / DISTINCT / GROUP BY, plus index
// MAINTENANCE under DML. Two schemas hold identical data and differ only in
// which indexes (value AND aggregate) exist; every query text is run against
// both and the answers must be identical, row-for-row, in order.

import (
	"fmt"
)

func MhEqRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mhHead truncates a row list so one systemic divergence cannot bury the rest
// of the report under thousands of rows.
func MhHead(rows []string) []string {
	if len(rows) <= 25 {
		return rows
	}
	return append(append([]string{}, rows[:25]...), fmt.Sprintf("...(+%d more)", len(rows)-25))
}

func MhFirstDiff(a, b []string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("row %d: idx=%q noidx=%q", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("common prefix equal; lengths %d vs %d", len(a), len(b))
}
