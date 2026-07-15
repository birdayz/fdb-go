package executor

import (
	"sort"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RFC-173 cap test helpers: the executor row model is the ordinal PositionalRow
// (the name-keyed map[string]any Datum is deleted). These helpers let tests that
// previously constructed / asserted against a name-keyed Datum keep working by
// building and reading a PositionalRow by column name.

// dmap builds a QueryResult carrying a PositionalRow from a name->value map.
// Column order is sorted by name for determinism; test reads resolve the slot
// through the row's Type (getByName below), so the order is immaterial.
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

// getByName is the TEST-ONLY name read-out: it resolves name → ordinal via the
// row's Type (FieldIndex, first-match) and reads that slot. Production has no
// name-keyed read arm (RFC-173 — every reference is baked to an ordinal at
// plan time); tests keep this convenience purely to ASSERT on named columns.
func getByName(pos *PositionalRow, name string) (any, bool) {
	if pos == nil || pos.Type == nil {
		return nil, false
	}
	i, ok := pos.Type.FieldIndex(name)
	if !ok {
		return nil, false
	}
	return pos.Get(i)
}

// rowVal reads a single column by name from a QueryResult's PositionalRow.
func rowVal(qr QueryResult, name string) any {
	if qr.Positional == nil {
		return nil
	}
	v, _ := getByName(qr.Positional, name)
	return v
}

// legRead evaluates the PRODUCTION resolution of an alias-qualified column over
// a leg-windowed positional row: a source-relative BAKED reference
// (QOV(alias).col at the leg-LOCAL ordinal — the resolver's plan-time bind
// against the leg's declared column order) evaluated through
// frontierRowContext's rowLegsBinder. This is what an executing plan does for
// `alias.col`; tests use it to assert leg-window routing end-to-end.
func legRead(pos *PositionalRow, alias, col string) (any, bool) {
	if pos == nil || pos.Type == nil {
		return nil, false
	}
	for _, leg := range pos.Type.Legs {
		if !strings.EqualFold(leg.Name, alias) {
			continue
		}
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(pos.Type.Fields) {
			return nil, false
		}
		for k := leg.Start; k < end; k++ {
			if !strings.EqualFold(pos.Type.Fields[k].Name, col) {
				continue
			}
			fv := values.NewCorrelatedFieldValueWithResolvedOrdinal(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
				col, k-leg.Start, values.UnknownType)
			v, err := fv.Evaluate(frontierRowContext(pos, nil, false))
			if err != nil {
				return nil, false
			}
			return v, true
		}
		return nil, false
	}
	return nil, false
}

// rowScalar reads the sole slot of a QueryResult's PositionalRow (a scalar row).
func rowScalar(qr QueryResult) any {
	if qr.Positional == nil || len(qr.Positional.Slots) == 0 {
		return nil
	}
	return qr.Positional.Slots[0]
}
