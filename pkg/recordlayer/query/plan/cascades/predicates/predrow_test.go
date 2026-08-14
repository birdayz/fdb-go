package predicates

import (
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// predRow wraps a name->value map as a values.OrdinalRow for tests. The
// ordinal PositionalRow is the SOLE value-eval context: a FieldValue never
// resolves against a bare map[string]any, nor by name against the row —
// every column reference carries a plan-time ordinal and reads
// row.Get(ordinal) positionally. predRow.Get is that positional read over the
// map's SORTED keys, so a field's ordinal is its alphabetical slot
// (deterministic; Go map iteration is randomized). Use pbake below to build a
// FieldValue whose ordinal matches.
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

// pbake returns a BAKED flat FieldValue whose ordinal is field's slot in the
// SORTED key set of schema. predRow.Get reads positionally over the same
// sorted order, so the read hits the right slot. The schema is the full set
// of column names the row(s) carry (a fixed schema across a table of rows);
// pass every column so ordinals are stable. pbake panics if field is absent
// from schema (a test bug).
func pbake(t testing.TB, field string, typ values.Type, schema ...string) values.Value {
	t.Helper()
	sorted := append([]string(nil), schema...)
	sort.Strings(sorted)
	fields := make([]values.Field, len(sorted))
	for i, name := range sorted {
		fieldType := values.NullableLong
		if name == field {
			fieldType = typ
		}
		fields[i] = values.Field{Name: name, FieldType: fieldType, Ordinal: i}
	}
	rowType := values.NewRecordType("pred_row_"+strings.Join(sorted, "_"), false, fields)
	root := mustQOV(t, values.NamedCorrelationIdentifier("pred_row_"+strings.Join(sorted, "_")), rowType)
	for i, k := range sorted {
		if k == field {
			request, err := values.FieldByNameAndOrdinal(field, i)
			if err != nil {
				t.Fatalf("pbake request: %v", err)
			}
			resolved, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
			if err != nil {
				t.Fatalf("pbake resolve: %v", err)
			}
			return resolved
		}
	}
	t.Fatalf("pbake: field not in schema: %s", field)
	return nil
}
