package executor

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

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
