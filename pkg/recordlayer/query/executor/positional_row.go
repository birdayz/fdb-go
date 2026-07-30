package executor

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PositionalRow is the typed positional runtime row — the SOLE runtime row:
// field values indexed by ORDINAL, paired with the RecordType that names and
// types each slot. Positional access (Slots[ordinal]) mirrors Java's
// MessageHelpers.getFieldValueForFieldOrdinals; every column reference reads
// its plan-time-baked ordinal (there is no runtime name resolution — the
// Type's names serve plan-time binding and diagnostics).
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

// TypeNames returns the row type's column names in ordinal order — diagnostics
// for values.OrdinalResolutionError (via an optional-interface assertion), so a
// loud resolution miss reports what the row actually carried.
func (r *PositionalRow) TypeNames() []string {
	if r == nil || r.Type == nil {
		return nil
	}
	names := make([]string, len(r.Type.Fields))
	for i, f := range r.Type.Fields {
		names[i] = f.Name
	}
	return names
}

// MultiLeg reports whether this row is a MULTI-LEG composed row (a merged
// concat / clustered box row whose Type carries leg boundaries beyond a
// single whole-row window). Consulted by values.FieldValue's correlated
// fall-through arms: a source-relative baked ordinal cannot be served by a
// multi-leg row without a leg binding — such a read must fail loud rather
// than silently address the wrong leg's slot.
func (r *PositionalRow) MultiLeg() bool {
	if r == nil || r.Type == nil || len(r.Type.Legs) == 0 {
		return false
	}
	if len(r.Type.Legs) == 1 {
		l := r.Type.Legs[0]
		if l.Start == 0 && l.Width == len(r.Type.Fields) {
			return false // a single whole-row window IS the source row
		}
	}
	return true
}

// RowValue returns a QueryResult's row in name-keyed form: a bare scalar for a
// 1-slot `_0` row (a non-record UNNEST element), else a name->value map
// (duplicate output names collapse last-wins).
// EXPORTED for external test/differential consumers only; production code reads
// the PositionalRow by ordinal. Nil-safe.
func RowValue(qr QueryResult) any {
	if qr.Positional == nil {
		return nil
	}
	if isBareScalarRow(qr.Positional) {
		return qr.Positional.Slots[0]
	}
	return positionalToMap(qr.Positional)
}

// positionalToMap projects a PositionalRow to a name->value map, for the LOCAL
// boundaries that build a name-keyed artifact from a row (a proto record in the DML
// insert/update path, keyed by column name). This is NOT runtime name resolution
// — it is a one-shot projection at a boundary that is inherently name-keyed (a proto
// message sets fields by name).
//
// IT IS LOSSY IN TWO WAYS, and a caller that is not itself name-keyed must not use
// it. Slot ORDER is gone (a map has none), and duplicate output names collapse
// LAST-WINS — a row of width n projects to fewer than n entries whenever two
// output columns share a name, which is legal and routine on a merged/unnest row.
// The order loss is the milder of the two: it makes a PERMUTATION of the row
// invisible, so anything asserting on this projection cannot see a mis-bound leg
// window. The last-wins loss is worse, because it discards a value outright rather
// than reordering it.
//
// The DML boundary is safe on both counts by construction (a proto record has no
// column order and cannot carry two same-named fields anyway). Any other consumer
// should read the PositionalRow's Slots directly.
func positionalToMap(pos *PositionalRow) map[string]any {
	if pos == nil || pos.Type == nil {
		return nil
	}
	m := make(map[string]any, len(pos.Type.Fields))
	for i, f := range pos.Type.Fields {
		if i < len(pos.Slots) {
			m[f.Name] = pos.Slots[i]
		}
	}
	return m
}

// positionalTypeFromNames builds the RecordType for a producer's output — one
// field per column in output order (ordinal = position), named by the column name.
// It uses a RAW RecordType (not NewRecordType) on purpose: a producer may emit
// DUPLICATE output names (SELECT a, a; a join projecting two legs' `id`; a covering
// index whose value column repeats a PK column), and the ordinal model keeps those
// as DISTINCT fields by position, whereas NewRecordType panics on a duplicate
// name. Positional access is by
// ordinal, so duplicates are unambiguous; FieldIndex (name->ordinal) returns the
// first match, which is why a name-keyed lookup legitimately differs from
// positional access on duplicate-named output: name lookup sees only the
// first match (or last-wins, for a map built by positionalToMap), while every
// ordinal slot stays independently addressable.
func positionalTypeFromNames(names []string) *values.RecordType {
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.UnknownType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}
