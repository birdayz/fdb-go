package executor

import (
	"reflect"
	"strings"

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
// multi-leg row (RFC-173 item C correct-or-loud).
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
		// RFC-173 item 3: a DOTTED name over a row whose type carries leg
		// boundaries (RecordType.Legs — the clustered box concat / the
		// merged seed layout) resolves per-leg: qualifier → the leg's
		// window, column → FieldIndex WITHIN it. This is the plan-agnostic
		// bridge for the name-model read form ("B.BID") the resolver keeps
		// for non-dup qualified references: the correlated-FlatMap winner
		// serves it from the Datum, and a positional-only row (the
		// clustered null-supplying birth) serves it here — same slot either
		// way. First-match on duplicate leg names mirrors FieldIndex's own
		// first-match rule.
		if di := strings.IndexByte(name, '.'); di > 0 {
			qual, col := name[:di], name[di+1:]
			if len(r.Type.Legs) > 0 {
				for _, leg := range r.Type.Legs {
					if !strings.EqualFold(leg.Name, qual) {
						continue
					}
					end := leg.Start + leg.Width
					if leg.Start < 0 || end > len(r.Type.Fields) {
						break
					}
					for k := leg.Start; k < end; k++ {
						if strings.EqualFold(r.Type.Fields[k].Name, col) {
							return r.Get(k)
						}
					}
					break
				}
			}
			// RFC-173 item D: the unique-leaf-strip heuristic (qualifier
			// matched no leg → strip it and resolve the leaf when UNIQUE) is
			// DELETED. Its one live consumer (lookupJoinKeyPositional's
			// alias-qualified probe) already falls back to the BARE column
			// read, whose FieldIndex first-match returns the identical slot
			// for a unique leaf — so a dotted name that matches neither the
			// flat type nor a leg window is now simply not found. Qualified
			// reads bake at plan time; the leg-window arm above serves the
			// leg-addressed runtime forms.
		}
		return nil, false
	}
	return r.Get(i)
}

// RowValue returns a QueryResult's row as the value the retired name-keyed Datum
// field carried: a bare scalar for a 1-slot `_0` row (a non-record UNNEST
// element), else a name->value map (duplicate output names collapse last-wins).
// EXPORTED for external test/differential consumers migrating off QueryResult.Datum;
// production code reads the PositionalRow by ordinal/name instead. Nil-safe.
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
// insert/update path, keyed by column name). This is NOT the retired name model —
// it is a one-shot projection at a boundary that is inherently name-keyed (a proto
// message sets fields by name). Duplicate output names collapse last-wins (a proto
// record cannot carry two same-named fields anyway).
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
// as DISTINCT fields by position — the RFC-173 Slice-4 "last-leg-wins collision
// fix" — whereas NewRecordType panics on a duplicate name. Positional access is by
// ordinal, so duplicates are unambiguous; FieldIndex (name->ordinal) returns the
// first match, which is why the shadow assert legitimately DIFFERS from the
// last-wins name map on duplicate-named output (the §5 models-must-differ case).
func positionalTypeFromNames(names []string) *values.RecordType {
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
// agreement holds for absent fields. Comparison is reflect.DeepEqual so
// list/bytes/message values compare correctly.
//
// SCOPE: today this is exercised by the per-producer shadow TESTS (each producer's
// positional row vs its name map) — it is NOT wired into production and does not
// run per-row on every plan yet. Slice 1 (which makes ordinal resolution
// authoritative) is where the shadow certification runs in the dual-emission path;
// until then, do not treat this as a live production guard.
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
