package executor

import (
	"sort"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RFC-173 cap test helpers: the executor row model is the ordinal PositionalRow
// (the name-keyed map[string]any Datum is deleted). These helpers let tests that
// previously constructed / asserted against a name-keyed Datum keep working by
// building and reading a PositionalRow by column name.

// dmap builds a QueryResult carrying a PositionalRow from a name->value map.
// Column order is sorted by name for determinism; reads are by GetByName, so the
// order is immaterial to correctness.
func dmap(m map[string]any) QueryResult {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	slots := make([]any, len(names))
	for i, n := range names {
		slots[i] = m[n]
	}
	return QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}}
}

// dorder builds a QueryResult PositionalRow with an EXPLICIT column order, for
// tests that depend on ordinal position (duplicate names, or ordinal reads).
func dorder(names []string, slots []any) QueryResult {
	return QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}}
}

// dscalar builds a QueryResult carrying a 1-slot scalar PositionalRow (the
// bare-scalar row shape — `t.arr AS x` flowing a raw value).
func dscalar(v any) QueryResult {
	return QueryResult{Positional: scalarPositionalRow(v)}
}

// dmapPK is dmap with a primary key attached.
func dmapPK(pk tuple.Tuple, m map[string]any) QueryResult {
	qr := dmap(m)
	qr.PrimaryKey = pk
	return qr
}

// rowMap reads a QueryResult's PositionalRow back into a name->value map, for
// tests that previously asserted against the name-keyed Datum. Nil (no
// PositionalRow) reads as a nil map — indexing it yields the zero value, exactly
// as a name-keyed miss did. Nil-safe.
func rowMap(qr QueryResult) map[string]any {
	return positionalToMap(qr.Positional)
}

// rowMapOK is rowMap with an ok flag mirroring the old
// `m, ok := qr.Datum.(map[string]any)` type assertion — true iff the row carries
// a PositionalRow.
func rowMapOK(qr QueryResult) (map[string]any, bool) {
	if qr.Positional == nil {
		return nil, false
	}
	return positionalToMap(qr.Positional), true
}

// rowVal reads a single column by name from a QueryResult's PositionalRow.
func rowVal(qr QueryResult, name string) any {
	if qr.Positional == nil {
		return nil
	}
	v, _ := qr.Positional.GetByName(name)
	return v
}

// rowScalar reads the sole slot of a QueryResult's PositionalRow (a scalar row).
func rowScalar(qr QueryResult) any {
	if qr.Positional == nil || len(qr.Positional.Slots) == 0 {
		return nil
	}
	return qr.Positional.Slots[0]
}
