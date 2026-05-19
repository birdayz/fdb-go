package embedded

import (
	"database/sql/driver"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// colRef carries structured column identity: a (table, column) pair.
// Mirrors Java's Identifier{name, qualifier} / FieldValue(QOV(correlation), field).
// The table part is empty for unqualified references.
type colRef struct {
	table string // table alias or "" for unqualified
	col   string // bare column name
}

// parseColRef splits a flat "TABLE.COL" string into a structured colRef.
// Unqualified names produce colRef{"", "COL"}.
func parseColRef(s string) colRef {
	if dot := strings.LastIndex(s, "."); dot >= 0 {
		return colRef{table: s[:dot], col: s[dot+1:]}
	}
	return colRef{col: s}
}

// qualified returns "TABLE.COL" or just "COL" if table is empty.
func (r colRef) qualified() string {
	if r.table == "" {
		return r.col
	}
	return r.table + "." + r.col
}

// bare returns the unqualified column name.
func (r colRef) bare() string {
	return r.col
}

// isQualified returns true when the reference has a table qualifier.
func (r colRef) isQualified() bool {
	return r.table != ""
}

// mapLookup resolves a column reference against a row map. Tries the
// qualified form first ("TABLE.COL"), then falls back to bare ("COL").
// Returns (value, true) on hit, (nil, false) on miss.
func mapLookup(row map[string]driver.Value, ref colRef) (driver.Value, bool) {
	if ref.table != "" {
		if v, ok := row[ref.qualified()]; ok {
			return v, true
		}
	}
	if v, ok := row[ref.col]; ok {
		return v, true
	}
	return nil, false
}

// mapLookupStr resolves a flat column name string against a row map.
// Tries the full string first, then strips the table qualifier and
// retries the bare column. Convenience wrapper for code paths that
// still carry flat strings.
func mapLookupStr(row map[string]driver.Value, key string) (driver.Value, bool) {
	if v, ok := row[key]; ok {
		return v, true
	}
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		if v, ok := row[key[dot+1:]]; ok {
			return v, true
		}
	}
	return nil, false
}

// mapLookupChecked is like mapLookup but checks for ambiguousColumnMarker.
// Returns the resolved value or an ErrCodeAmbiguousColumn error.
func mapLookupChecked(row map[string]driver.Value, ref colRef) (driver.Value, error) {
	check := func(v driver.Value) (driver.Value, error) {
		if m, ok := v.(ambiguousColumnMarker); ok {
			return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"column reference %q is ambiguous", m.Col)
		}
		return v, nil
	}
	if ref.table != "" {
		if v, ok := row[ref.qualified()]; ok {
			return check(v)
		}
	}
	if v, ok := row[ref.col]; ok {
		return check(v)
	}
	return nil, nil
}

// mapLookupStrChecked is the flat-string variant of mapLookupChecked.
func mapLookupStrChecked(row map[string]driver.Value, key string) (driver.Value, error) {
	return mapLookupChecked(row, parseColRef(key))
}
