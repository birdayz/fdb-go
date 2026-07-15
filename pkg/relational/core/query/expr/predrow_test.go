package expr_test

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// lookupFold resolves name against m: exact key hit, else case-insensitive
// scan, else (nil, true) — a missing key is SQL NULL (sparse map context).
func lookupFold(m map[string]any, name string) (any, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, true
}

// predRow wraps a name->value map as a values.OrdinalRow for tests. The
// ordinal row is the SOLE value-eval context (a Value/PredicateValue does
// not resolve against a bare map[string]any). GetByName resolves the flat
// field by name; a missing key is SQL NULL (sparse map-context behavior).
// Ordinal reads always miss, so predRow only serves lazy (name-resolving)
// FieldValues — resolver-built values carry a baked ordinal and need
// ordinalPredRow.
type predRow map[string]any

func (r predRow) Get(int) (any, bool) { return nil, false }

var _ values.OrdinalRow = predRow(nil)

// ordinalPredRow implements values.OrdinalRow: Get(ordinal) resolves through
// cols — the scope's declared column order, the same order construction-time
// baking bound ordinals against (sparse map: missing = SQL NULL).
type ordinalPredRow struct {
	cols []string
	m    map[string]any
}

func (r ordinalPredRow) Get(i int) (any, bool) {
	if i < 0 || i >= len(r.cols) {
		return nil, false
	}
	return lookupFold(r.m, r.cols[i])
}

var _ values.OrdinalRow = ordinalPredRow{}

// usersRow wraps a row map in buildScope's USERS declared column order —
// the order resolver-baked ordinals in the buildScope tests point into.
func usersRow(m map[string]any) ordinalPredRow {
	return ordinalPredRow{cols: []string{"ID", "NAME", "ACTIVE", "ADMIN"}, m: m}
}
