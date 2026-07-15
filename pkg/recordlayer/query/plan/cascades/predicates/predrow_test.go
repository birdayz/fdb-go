package predicates

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// predRow wraps a name->value map as a values.OrdinalRow for tests. RFC-173 item
// C made the ordinal PositionalRow the SOLE value-eval context (a FieldValue no
// longer resolves against a bare map[string]any, nor by name against the row) —
// every column reference must carry a plan-time ordinal and read row.Get(ordinal)
// positionally. predRow.Get is that positional read over the map's SORTED keys,
// so a field's ordinal is its alphabetical slot (deterministic; Go map iteration
// is randomized). Use pbake below to build a FieldValue whose ordinal matches.
type predRow map[string]any

// sortedKeys returns r's keys in alphabetical order — the positional slot order.
func (r predRow) sortedKeys() []string {
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (r predRow) Get(ord int) (any, bool) {
	keys := r.sortedKeys()
	if ord < 0 || ord >= len(keys) {
		return nil, false
	}
	return r[keys[ord]], true
}

// GetByName is retained to satisfy values.OrdinalRow, but RFC-173 item C retired
// the runtime name path, so production never calls it — the ordinal Get above is
// the sole read path.
// pbake returns a BAKED flat FieldValue whose ordinal is field's slot in the
// SORTED key set of schema — the RFC-173 item C plan-time ordinal bind. predRow.Get
// reads positionally over the same sorted order, so the read hits the right slot.
// The schema is the full set of column names the row(s) carry (a fixed schema
// across a table of rows); pass every column so ordinals are stable. pbake panics
// if field is absent from schema (a test bug).
func pbake(field string, typ values.Type, schema ...string) *values.FieldValue {
	sorted := append([]string(nil), schema...)
	sort.Strings(sorted)
	for i, k := range sorted {
		if k == field {
			return values.NewFieldValueWithResolvedOrdinal(field, i, typ)
		}
	}
	panic("pbake: field not in schema: " + field)
}
