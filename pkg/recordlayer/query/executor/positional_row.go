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
	// Layout is the selected immutable physical address space for this row.
	// It is deliberately separate from Type: logical type equality never
	// compares layout provenance.
	Layout values.OrdinalLayout
	// LayoutPresence distinguishes an unmatched null-supplying source from a
	// matched source whose complete row happens to contain SQL NULLs. It is
	// immutable values-owned row metadata and is never encoded as a logical
	// column.
	LayoutPresence values.WindowMatchPresence
	// transportKind records the semantic kind of this positional envelope.
	// The zero value deliberately means RECORD: virtually every producer emits
	// a real row, and existing/manual row literals must remain record-valued.
	// Only scalarPositionalRowOfType sets Scalar for the executor's private
	// one-slot wrapper around a non-record datum. Field names cannot carry this
	// distinction: an authored anonymous one-field record can legitimately have
	// the exact same `_0` shape.
	transportKind values.OrdinalCarrierKind
}

// OrdinalRecordType exposes the exact logical record declaration to the
// driver-boundary rowstruct adapter. It does not expose or alter the physical
// OrdinalLayout; public STRUCT materialization needs only the row's immutable
// field types and ordinals.
func (r *PositionalRow) OrdinalRecordType() *values.RecordType {
	if r == nil {
		return nil
	}
	return r.Type
}

// OrdinalRowKind exposes whether this positional value is a genuine record or
// the executor's private wrapper around a scalar. It is public only through the
// rowstruct boundary interface; runtime binding continues to use the selected
// plan's OrdinalLayout authority.
func (r *PositionalRow) OrdinalRowKind() values.OrdinalCarrierKind {
	if r == nil {
		return values.OrdinalCarrierInvalid
	}
	if r.transportKind == values.OrdinalCarrierScalar {
		return values.OrdinalCarrierScalar
	}
	return values.OrdinalCarrierRecord
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

// NewLayoutPositionalRow builds a positional row owned by one exact physical
// layout. The layout's current carrier type must equal typ; the same admitted
// layout pointer is retained for runtime binder admission.
func NewLayoutPositionalRow(typ *values.RecordType, layout values.OrdinalLayout) (*PositionalRow, error) {
	if typ == nil || layout == nil || layout.CarrierKind() != values.OrdinalCarrierRecord {
		return nil, &values.ResolutionError{
			ErrorCode: values.LayoutCarrierMismatch,
			Path:      "positional.layout",
			Detail:    "record row requires a record OrdinalLayout and non-nil type",
		}
	}
	carrier := layout.Carrier()
	if carrier == nil || !typ.Equals(carrier.FlowedType()) {
		return nil, &values.ResolutionError{
			ErrorCode: values.LayoutCarrierMismatch,
			Path:      "positional.layout",
			Detail:    "row type and layout carrier type disagree",
		}
	}
	row := NewPositionalRow(typ)
	row.Layout = layout
	return row, nil
}

// AttachOrdinalLayout returns a row carrying layout as its physical address
// authority. The input row is never mutated: pass-through plans may share a
// child QueryResult while exposing a different parent property, and publishing
// that property must not rewrite the child's carrier in place.
//
// The selected layout must describe exactly the row's logical record type.
// Dynamic/erased carriers therefore fail at the plan boundary instead of
// falling back to the legacy ambient positional interpretation.
func (r *PositionalRow) AttachOrdinalLayout(layout values.OrdinalLayout, carrierType values.Type) (*PositionalRow, error) {
	// carrierType is supplied by the caller rather than re-derived from
	// layout.Carrier() here, because deriving it is NOT free and NOT per-row
	// work: FlowedType thaws a whole fresh Type graph out of the carrier's
	// exact handle, and the carrier is fixed for an entire cursor. Deriving it
	// here made a scan allocate one Type graph per row and then deep-compare
	// against it — measured as the largest single item this method contributed
	// to the row path.
	if err := r.checkLayoutAttachable(layout, carrierType); err != nil {
		return nil, err
	}
	// The identity fast path returns the row WITHOUT going through finishAttach,
	// and that is presence-EQUIVALENT rather than presence-skipping: finishAttach
	// clears LayoutPresence only when the prior layout is not RawEqual to the new
	// one, RawEqual is reflexive, and here the two are the same object — so it
	// would preserve presence exactly as this does. Asserting presence is nil here
	// instead would forbid a legitimate state: a row that carries presence FOR THIS
	// LAYOUT must keep it.
	//
	// What GUARDS that is TestFirstOrDefault_RecordNullKeepsExactCarrierAndAbsence
	// and TestFirstOrDefaultAsFlatMapOuterPreservesWholeObjectPresence: clearing
	// presence here reddens both, with an absent record reading as PRESENT — a wrong
	// outer-join answer, not a crash. TestIdentityAttachIsPresenceEquivalent states
	// the reflexivity argument but never sets LayoutPresence, so it survives that
	// mutation and is not the guard; cite these two.
	if r.Layout == layout {
		return r, nil
	}
	copyRow := *r
	copyRow.Slots = append([]any(nil), r.Slots...)
	copyRow.Layout = layout
	return r.finishAttach(&copyRow, layout), nil
}

// finishAttach applies the layout to target and settles presence. r is the row
// the layout is being attached FROM, which is target itself for the in-place
// caller and target's source for the copying one.
func (r *PositionalRow) finishAttach(target *PositionalRow, layout values.OrdinalLayout) *PositionalRow {
	priorLayout := r.Layout
	target.Layout = layout
	// Presence is meaningful only for the exact same physical window
	// structure. Independently constructed but RawEqual plan/executor layouts
	// may use different owner-current handles; switching to the published plan
	// handle preserves the source-window match facts. A genuinely different
	// layout discards them rather than misapplying another address space's
	// unmatched/matched bits.
	//
	// priorLayout is read BEFORE the assignment above, because the in-place
	// caller passes target == r: comparing r.Layout here would compare the
	// layout being attached against itself and preserve presence bits from
	// another address space.
	if priorLayout == nil || !priorLayout.RawEqual(layout) {
		target.LayoutPresence = nil
	}
	return target
}

// checkLayoutAttachable is the shared guard for both attach forms.
func (r *PositionalRow) checkLayoutAttachable(layout values.OrdinalLayout, carrierType values.Type) error {
	if r == nil || r.Type == nil || layout == nil || layout.CarrierKind() != values.OrdinalCarrierRecord {
		return &values.ResolutionError{
			ErrorCode: values.LayoutCarrierMismatch,
			Path:      "positional.layout",
			Detail:    "record row requires a concrete record OrdinalLayout",
		}
	}
	if carrierType == nil || !r.Type.Equals(carrierType) {
		return &values.ResolutionError{
			ErrorCode: values.LayoutCarrierMismatch,
			Path:      "positional.layout",
			Detail:    "row type and layout carrier type disagree",
		}
	}
	return nil
}

// OrdinalLayout returns the exact immutable physical layout attached to this
// carrier. A legacy/unadmitted row returns nil.
func (r *PositionalRow) OrdinalLayout() values.OrdinalLayout {
	if r == nil {
		return nil
	}
	return r.Layout
}

// wholeObjectBinding returns the row's whole quantified object. A
// FirstOrDefault empty arm still carries a correctly typed/width positional
// shell so physical layout validation remains exact; its LayoutPresence marks
// that the complete object is SQL NULL. The explicitAbsent result lets callers
// distinguish that state from a matched row whose every slot is nil.
func (r *PositionalRow) wholeObjectBinding() (value any, explicitAbsent bool, err error) {
	if r == nil {
		return nil, false, nil
	}
	if r.LayoutPresence == nil {
		return r, false, nil
	}
	if r.Layout == nil {
		return nil, false, &values.ResolutionError{
			ErrorCode: values.LayoutCarrierMismatch,
			Path:      "positional.layout",
			Detail:    "row match presence requires an exact ordinal layout",
		}
	}
	matched, known, matchErr := values.OrdinalCarrierMatchState(r.Layout, r.LayoutPresence)
	if matchErr != nil {
		return nil, false, matchErr
	}
	if known && !matched {
		return nil, true, nil
	}
	return r, false, nil
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
// ordinal, so duplicates are unambiguous; a NAME-keyed lookup on the same type is
// not, which is why it legitimately reaches less of the row than positional access
// does. FieldIndexUnique DECLINES on a duplicated name (it answers only when the
// name matches exactly one field), and a map built by positionalToMap collapses
// duplicates last-wins — while every ordinal slot stays independently addressable.
func positionalTypeFromNames(names []string) *values.RecordType {
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.UnknownType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}
