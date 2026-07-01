package executor

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PositionalRow is the RFC-173 P2 typed positional runtime row: field values
// indexed by ORDINAL, paired with the RecordType that names and types each slot.
// It is the ordinal-model counterpart to the legacy name-keyed map[string]any
// (QueryResult.Datum).
//
// During the migration it is emitted ALONGSIDE the name-keyed map (dark/dual),
// with a field-for-field shadow assert (a later P2 increment), until the ordinal
// model becomes authoritative in Slice 1+ and the name map is retired. Positional
// access (Slots[ordinal]) mirrors Java's MessageHelpers.getFieldValueForFieldOrdinals;
// name access resolves the ordinal via RecordType.FieldIndex (P1's sound
// list-position lookup), so the two representations agree by construction on a
// well-formed row — that agreement is what the shadow assert pins.
type PositionalRow struct {
	// Type gives each slot its name and type; Slots[i] is the value of the field
	// at ordinal i. len(Slots) == len(Type.Fields) for a well-formed row.
	Type  *values.RecordType
	Slots []any
}

// NewPositionalRow builds a row for typ with every slot nil (SQL NULL). Slots is
// sized to the field count so Get/Set are position-safe. A nil typ yields an
// empty row (zero slots).
func NewPositionalRow(typ *values.RecordType) *PositionalRow {
	n := 0
	if typ != nil {
		n = len(typ.Fields)
	}
	return &PositionalRow{Type: typ, Slots: make([]any, n)}
}

// Get returns the value at the given ordinal plus an in-range flag. Nil-safe.
func (r *PositionalRow) Get(ordinal int) (any, bool) {
	if r == nil || ordinal < 0 || ordinal >= len(r.Slots) {
		return nil, false
	}
	return r.Slots[ordinal], true
}

// Set writes v at the given ordinal, returning false (no-op) if out of range.
func (r *PositionalRow) Set(ordinal int, v any) bool {
	if r == nil || ordinal < 0 || ordinal >= len(r.Slots) {
		return false
	}
	r.Slots[ordinal] = v
	return true
}

// GetByName resolves name -> ordinal via the row's RecordType (FieldIndex, P1's
// sound list-position lookup) then reads that slot. This is the bridge the P2
// shadow assert uses to compare positional access against the legacy name-keyed
// map[string]any. Returns (nil, false) for an unknown name, an anonymous field
// (empty name never matches), or a nil row/type.
func (r *PositionalRow) GetByName(name string) (any, bool) {
	if r == nil || r.Type == nil {
		return nil, false
	}
	i, ok := r.Type.FieldIndex(name)
	if !ok {
		return nil, false
	}
	return r.Get(i)
}

// positionalRowFromMap builds a PositionalRow for typ by reading each named field
// from the legacy name-keyed map — the dual-emission bridge every row producer
// uses to emit the positional row ALONGSIDE its existing map[string]any during the
// dark/dual migration window. A field absent from the map becomes a nil slot (SQL
// NULL), matching the map's missing-key = NULL semantics; an anonymous field
// (empty name) is left nil (not name-addressable). Field names follow the
// upper-case identifier-folding convention the map keys already use.
func positionalRowFromMap(typ *values.RecordType, m map[string]any) *PositionalRow {
	row := NewPositionalRow(typ)
	if typ == nil || m == nil {
		return row
	}
	for i, f := range typ.Fields {
		if f.Name == "" {
			continue
		}
		if v, ok := m[f.Name]; ok {
			row.Slots[i] = v
		}
	}
	return row
}

// projectionPositionalType builds the RecordType for a projection's output — one
// field per column in projection order (ordinal = position), named by the column
// name. It uses a RAW RecordType (not NewRecordType) on purpose: a projection may
// emit DUPLICATE output names (SELECT a, a; a join projecting two legs' `id`), and
// the ordinal model keeps those as DISTINCT fields by position — the RFC-173
// Slice-4 "last-leg-wins collision fix" — whereas NewRecordType panics on a
// duplicate name. Positional access is by ordinal, so duplicates are unambiguous;
// FieldIndex (name->ordinal) returns the first match, which is why the shadow
// assert legitimately DIFFERS from the last-wins name map on duplicate-named
// projections (the §5 models-must-differ case).
func projectionPositionalType(names []string) *values.RecordType {
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.UnknownType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

// shadowMismatch is the RFC-173 P2 dual-mode shadow assert: it returns the name of
// the first field where the positional row DISAGREES with the name-keyed map, or
// "" if they agree on every named field of the row's type. A field the map omits
// reads as nil on both sides (map missing-key = NULL, unset slot = nil), so
// agreement holds for absent fields. Used to certify — per row, on every plan —
// that the positional representation faithfully mirrors the name-keyed map before
// the ordinal model is made authoritative (RFC-173 §5, execution-based validation).
// Comparison is reflect.DeepEqual so list/bytes/message values compare correctly.
func shadowMismatch(row *PositionalRow, m map[string]any) string {
	if row == nil || row.Type == nil {
		return ""
	}
	for i, f := range row.Type.Fields {
		if f.Name == "" {
			continue
		}
		pos, _ := row.Get(i)
		named := m[f.Name] // absent -> nil
		if !reflect.DeepEqual(pos, named) {
			return f.Name
		}
	}
	return ""
}
