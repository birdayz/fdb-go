package executor

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file is the RFC-173 Slice 2 ordinal-join executor machinery: the
// primitives the 2-way wedge's merged positional row is built from — LIVE
// since W3b (the translator seeds gated 2-way joins with the ordinal RC these
// consume). Everything here derives from the W3 pre-code ruling
// (rfcs/173-ordinal-column-resolution.md §4 Slice 2): spans derive from the
// RC (condition 1), leg windows are declared window scaffolding that dies
// when the uppers bake, S3/S4 (condition 2), the window implements
// values.OrdinalRow completely so no new eval arm exists (condition 3), and
// the wrong-slot hazard is pinned red→green (condition 4).

// legSpan is one leg's slot range within the 2-way ordinal join's merged
// positional row: the leg quantifier's alias, the leg's own RecordType, and
// the half-open window [Offset, Offset+Width) its columns occupy in the merged
// row. Spans are DERIVED from the ordinal join RC by ordinalJoinSpans at
// cursor construction (Graefe W3 condition 1: the RC is the single authority)
// — never stored or maintained as independent bookkeeping.
type legSpan struct {
	Alias   values.CorrelationIdentifier
	LegType *values.RecordType
	Offset  int
	Width   int
}

// ordinalJoinSpans is the cursor-side WINDOWS-ELIGIBILITY probe: it detects
// whether v is exactly the 2-way ordinal join SEED RC (the concatenation of
// two full legs, every field a baked leg reference) and derives the leg spans
// + merged type from it. DECLINE-ONLY (ok=false) for every other shape — a
// result value that is not the pristine seed is NOT a planner bug here: the
// pure-wrapper merge the drift assert deliberately allows rewrites the select's
// result value into the parent PROJECTION's RC, which legitimately mixes baked
// leg references (compose-folded through the seed) with computed values, or
// covers a leg partially (`SELECT b.y FROM a JOIN b`), or collapses to a
// single run (Graefe W3a-1 NAK: the earlier any-baked⟹well-formed-or-panic
// boundary false-positived on exactly those plans). Downstream consumers use
// this probe to decide whether LEG WINDOWS apply to the join's output row —
// windows are only meaningful when the output IS the leg concatenation; a
// folded projection's output is a plain frontier row and gets none.
//
// Loud seed validation lives where the shape IS guaranteed by construction:
// assertOrdinalJoinSeed, called by the W3b translator at the seed (and by its
// pins). Cursor-side ORDINAL-BIRTH detection (does this join evaluate its
// result value with leg bindings) is values.ContainsBakedOrdinal — deep,
// rewrite-invariant — not this probe.
//
// The derived spans carry per-leg Offset/Width over the merged row, and
// mergedType is a RAW *RecordType (NOT NewRecordType — duplicate names across
// legs are legal and preserved verbatim) with one field per RC field: the RC
// field's name, the baked FieldValue's type, ordinal = position.
func ordinalJoinSpans(v values.Value) (spans []legSpan, mergedType *values.RecordType, ok bool) {
	rc, isRC := v.(*values.RecordConstructorValue)
	if !isRC {
		return nil, nil, false
	}
	if len(rc.Fields) == 0 {
		return nil, nil, false
	}
	mergedFields := make([]values.Field, len(rc.Fields))
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			return nil, nil, false // not every field a baked leg ref — not the seed
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return nil, nil, false
		}
		legType, isRT := qov.Type().(*values.RecordType)
		if !isRT {
			return nil, nil, false
		}
		ord := fv.Resolved.Ordinal
		if len(spans) == 0 || spans[len(spans)-1].Alias != qov.Correlation {
			if ord != 0 {
				return nil, nil, false // run must start at leg ordinal 0
			}
			spans = append(spans, legSpan{Alias: qov.Correlation, LegType: legType, Offset: i, Width: 1})
		} else {
			cur := &spans[len(spans)-1]
			if ord != cur.Width {
				return nil, nil, false // gap or reorder — not the concat
			}
			cur.Width++
		}
		mergedFields[i] = values.Field{Name: f.Name, FieldType: fv.Type(), Ordinal: i}
	}

	if len(spans) != 2 {
		return nil, nil, false // the wedge is 2-way; 1 run = folded single leg, 3+ = S3
	}
	total := 0
	for _, s := range spans {
		if s.Width != len(s.LegType.Fields) {
			return nil, nil, false // partial leg coverage — a folded projection
		}
		total += s.Width
	}
	// Spans-consistency assert (Graefe W3 extra pin). Unreachable given the
	// run construction above — pinned anyway so a future edit that breaks the
	// derivation dies here, not in a downstream window misread.
	if total != len(rc.Fields) {
		panic(fmt.Sprintf("RFC-173 ordinal join spans inconsistent: sum(widths)=%d, RC has %d fields — spans must derive exactly from the RC", total, len(rc.Fields)))
	}
	return spans, &values.RecordType{Fields: mergedFields}, true
}

// assertOrdinalJoinSeed delegates to values.AssertOrdinalJoinSeed — the LOUD
// seed-shape validator RELOCATED to the values package (RFC-173 S2 W3b) so
// the translator, the caller of record, can invoke it at the seed without an
// executor import. Semantics unchanged; the executor-side pins keep
// exercising it through this name.
func assertOrdinalJoinSeed(rc *values.RecordConstructorValue) {
	values.AssertOrdinalJoinSeed(rc)
}

// legWindowRow is a leg-relative view over the join's merged positional row:
// leg ordinal i reads merged slot Offset+i. It is DECLARED WINDOW SCAFFOLDING
// (Graefe W3 condition 2): Java has no merged-row-with-leg-views — its uppers
// reference the join quantifier after plan-time rewriting — and these windows
// exist only because window-era uppers still reference LEGS directly
// (FieldValue(QOV(leg), col)) across the join boundary. They DIE when the
// uppers bake (S3 flip + S4 deletions) and must not ossify into "the runtime
// shape of quantifier bindings".
//
// It implements values.OrdinalRow COMPLETELY (condition 3) — Get leg-relative,
// GetByName leg-LOCAL (resolved against the leg's own type, so a lazy leg
// reference stays correct even when the merged type carries the same name at a
// different absolute slot) — so it slots into the existing evaluateCorrelated
// binder arm with no new eval arm, and a miss stays loud (the (nil,false)
// return becomes an OrdinalResolutionError in evaluateOrdinal). TypeNames
// feeds that error's diagnostics via the values.ordinalRowNames optional
// interface, reporting the LEG's columns (what this window actually exposes).
type legWindowRow struct {
	parent  values.OrdinalRow
	legType *values.RecordType
	offset  int
	width   int
}

// Get reads the leg-relative ordinal: merged slot offset+ord. Out-of-range leg
// ordinals return (nil, false) — evaluateOrdinal turns that into a loud
// values.OrdinalResolutionError, never a silent NULL.
func (w *legWindowRow) Get(ord int) (any, bool) {
	if ord < 0 || ord >= w.width {
		return nil, false
	}
	return w.parent.Get(w.offset + ord)
}

// GetByName resolves a leg-LOCAL column name against the LEG's own type, then
// reads leg-relative — the merged type (with its absolute slots and possible
// duplicate names) is never consulted.
func (w *legWindowRow) GetByName(name string) (any, bool) {
	if w.legType == nil {
		return nil, false
	}
	i, found := w.legType.FieldIndex(name)
	if !found {
		return nil, false
	}
	return w.Get(i)
}

// TypeNames returns the LEG type's column names — the diagnostics the
// values.OrdinalResolutionError enrichment (values.ordinalRowNames) reads via
// optional-interface assertion.
func (w *legWindowRow) TypeNames() []string {
	if w.legType == nil {
		return nil
	}
	return typeFieldNames(w.legType)
}

// legWindowBinder is the coexistence-window correlation binder for uppers over
// the 2-way ordinal join: a reference to a leg alias is bound to that leg's
// window over the merged row, anything else delegates to base. DECLARED WINDOW
// SCAFFOLDING (Graefe W3 condition 2) — it exists only because window-era
// uppers reference legs across the join boundary; when the uppers bake against
// the merged type (S3 flip, S4 deletions) the windows die with the name model.
// Must not ossify into the permanent runtime shape of quantifier bindings.
type legWindowBinder struct {
	base  values.CorrelationBinder
	spans []legSpan
	row   values.OrdinalRow
}

// GetCorrelationBinding implements values.CorrelationBinder: a span alias gets
// its leg window over the merged row; any other alias delegates to base (nil
// base: unbound).
func (b *legWindowBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	for _, s := range b.spans {
		if s.Alias == id {
			return &legWindowRow{parent: b.row, legType: s.LegType, offset: s.Offset, width: s.Width}, true
		}
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// adaptLegPositional is the RFC-173 sanctioned row-FORMAT adapter at the
// join-input boundary: it bridges NAME-model legs (aggregate/union box outputs
// that do not emit a positional row yet) into the ordinal join. A leg that
// already carries a positional row flows it through untouched; otherwise a
// *PositionalRow is synthesized from the name-keyed Datum by the LEG type's
// field names (slot i = datum[Fields[i].Name]; a missing key is a nil slot =
// SQL NULL); a nil or non-map Datum yields an all-nil row of the leg's width.
//
// LOUD when the synthesis matches ZERO of the leg's columns against a
// NON-EMPTY Datum (Torvalds W3a-1 catch): a name-model MERGE-shaped leg
// carries dotted-qualified keys ("A.ID") the bare leg-type names never match,
// so the silent path would all-NULL the leg — indistinguishable from a
// legitimate all-NULL row. The W2 gate makes such legs unreachable for gated
// joins (name-model join/exists selects are poison or enclosed), so this
// error is a belt-and-braces tripwire on that argument, not a supported path.
// A row whose keys are PRESENT with nil values (a genuine all-NULL row)
// matches normally and stays silent.
//
// This is format-only bridging — correlation-SEMANTIC bridging (an ordinal
// join consumed as a leg of a name-model merge select) is explicitly out of
// scope and prevented upstream by the W2 cluster-arity gate (RFC-173 §4
// Slice 2 coexistence scoping).
func adaptLegPositional(qr QueryResult, legType *values.RecordType) (values.OrdinalRow, error) {
	if qr.Positional != nil {
		// The passthrough requires ORDERED per-slot name agreement with the
		// leg type (Torvalds PR-447 catch, superseding the width-only
		// tripwire): a COVERING-INDEX leg's positional row is INDEX-shaped —
		// buildCoveringRow types it value-columns-then-PK ([V, ID]), not
		// table order ([ID, V]) — same width, different layout, and a baked
		// leg ordinal would silently read the wrong slot. A non-aligned
		// positional row is a LEGITIMATE plan shape (not a gate breach): fall
		// through to Datum synthesis (covering Datums carry the correct bare
		// UPPER keys), with the zero-match tripwire below as the final guard.
		if legType == nil || positionalMatchesLegType(qr.Positional, legType) {
			return qr.Positional, nil
		}
	}
	row := NewPositionalRow(legType)
	if legType == nil {
		return row, nil
	}
	if m, isMap := qr.Datum.(map[string]any); isMap {
		matched := 0
		for i, f := range legType.Fields {
			if v, present := m[f.Name]; present {
				row.Slots[i] = v
				matched++
			}
		}
		if matched == 0 && len(m) > 0 && len(legType.Fields) > 0 {
			return nil, fmt.Errorf("RFC-173 leg adapter: name-model leg row carries NONE of the leg type's %d columns %v (row keys: %d, dotted/merge-shaped?) — a gated join must not consume a name-model merge leg (W2 gate breach or leg-type mismatch)", len(legType.Fields), typeFieldNames(legType), len(m))
		}
	}
	return row, nil
}

// positionalMatchesLegType reports whether a leg's pre-existing positional
// row is LAYOUT-IDENTICAL to the seed's leg type: same width AND the same
// column name at every ordinal (case-insensitive) — the condition under which
// baked leg ordinals read the right slots. Order-sensitive by design: a
// covering-index row [V, ID] over a table typed [ID, V] must NOT pass.
func positionalMatchesLegType(row *PositionalRow, legType *values.RecordType) bool {
	if row.Type == nil || len(row.Type.Fields) != len(legType.Fields) || len(row.Slots) != len(legType.Fields) {
		return false
	}
	for i := range legType.Fields {
		if !strings.EqualFold(row.Type.Fields[i].Name, legType.Fields[i].Name) {
			return false
		}
	}
	return true
}

// typeFieldNames lists a RecordType's field names for diagnostics.
func typeFieldNames(rt *values.RecordType) []string {
	names := make([]string, len(rt.Fields))
	for i, f := range rt.Fields {
		names[i] = f.Name
	}
	return names
}

// evaluateOrdinalJoinRow births the join's merged positional row: each field
// of the ordinal join RC (a BAKED leg reference) is evaluated with the legs
// bound through bindings, writing merged slot i. A NULL LEG is expressed by
// the binder returning (nil, true) for that leg's correlation — the baked
// node's evaluateCorrelated `return bound, nil` arm yields nil, so the leg's
// slots come out NULL (contract ruling #3: appendNullLeg ≡ evaluating the
// merged RC with the leg QOV bound to nil; the null extension falls out, no
// ad-hoc per-row types).
//
// This is the PRIMITIVE: DisablePositionalEmission (the §5 name-model oracle)
// is respected by CALLERS (the W3b cursor birth sites, which are oracle-
// registry entries), not here.
func evaluateOrdinalJoinRow(rc *values.RecordConstructorValue, mergedType *values.RecordType, bindings values.CorrelationBinder) (*PositionalRow, error) {
	if len(rc.Fields) != len(mergedType.Fields) {
		panic(fmt.Sprintf("RFC-173 ordinal join row: RC has %d fields but merged type has %d — the merged type must derive from this RC (ordinalJoinSpans)", len(rc.Fields), len(mergedType.Fields)))
	}
	row := NewPositionalRow(mergedType)
	evalCtx := &values.RowEvalContext{Correlations: bindings}
	for i, f := range rc.Fields {
		v, err := f.Value.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		row.Slots[i] = v
	}
	return row, nil
}

// --- W3a-2: cursor-side birth wiring ----------------------------------------

// rcOutputType derives an RC's OUTPUT row type: a RAW *RecordType (duplicate
// names allowed and preserved verbatim — positional access is by ordinal) with
// one field per RC field: the RC field's name, the field Value's flowed type,
// ordinal = position. For the pristine ordinal join SEED this equals
// ordinalJoinSpans' mergedType (each field is a baked leg reference and its
// Type() is the leg column's type); for a FOLDED result value (the pure-wrapper
// merge's projection RC — baked refs mixed with computed values/constants) it
// is the projection's output row type, which has no leg windows but is still
// the single authoritative type of the birthed positional row.
func rcOutputType(rc *values.RecordConstructorValue) *values.RecordType {
	fields := make([]values.Field, len(rc.Fields))
	for i, f := range rc.Fields {
		var ft values.Type = values.UnknownType
		if f.Value != nil {
			ft = f.Value.Type()
		}
		fields[i] = values.Field{Name: f.Name, FieldType: ft, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

// legTypesFromResultValue collects the LEG types a (possibly folded) ordinal
// result value references: every BAKED FieldValue whose child is a
// *QuantifiedObjectValue flowing a *RecordType contributes correlation →
// leg RecordType. These are the leg types adaptLegPositional needs when the
// result value is a FOLDED projection RC (ordinalJoinSpans declines, so no
// spans carry the leg types). A leg folded away entirely is ABSENT from the
// map — no baked reference to it can exist in the RC, so evaluating the RC
// never consults its binding and no adapter is needed.
func legTypesFromResultValue(rv values.Value) map[values.CorrelationIdentifier]*values.RecordType {
	legs := make(map[values.CorrelationIdentifier]*values.RecordType)
	values.WalkValue(rv, func(n values.Value) bool {
		fv, isFV := n.(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			return true
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return true
		}
		if rt, isRT := qov.Type().(*values.RecordType); isRT {
			legs[qov.Correlation] = rt
		}
		return true
	})
	return legs
}

// ordinalJoinBirth is the per-cursor ordinal-BIRTH state, computed ONCE at
// cursor construction (Graefe W3 ruling: detection is the structural
// ContainsBakedOrdinal probe on the plan's result value — emergent from the
// representation, nothing for S4 to delete). Enabled marks the cursor as an
// ordinal birth site: its emitted rows carry a positional row evaluated from
// the RC with per-leg bindings, DUAL with the untouched name-model Datum
// (the coexistence-window invariant — the name side retires in Slice 4),
// gated per emission on the §5 DisablePositionalEmission oracle.
type ordinalJoinBirth struct {
	// Enabled is true iff the result value contains a baked ordinal reference
	// (values.ContainsBakedOrdinal, deep). A nil/lazy result value yields a nil
	// *ordinalJoinBirth instead — use the nil-safe enabled().
	Enabled bool
	// RC is the result value as the RC the birth evaluates per-field.
	RC *values.RecordConstructorValue
	// OutputType is the birthed positional row's single authoritative type
	// (rcOutputType; == ordinalJoinSpans' mergedType for the pristine seed).
	OutputType *values.RecordType
	// Spans + WindowsOK: the decline-only leg-window eligibility probe
	// (ordinalJoinSpans — pristine seed only). WindowsOK false for a folded
	// RV: the output is a plain projection row, downstream gets no windows.
	Spans     []legSpan
	WindowsOK bool
	// LegTypes are the leg types recovered from the RV's baked references —
	// the adapter's leg types when the RV is folded and Spans are unavailable.
	LegTypes map[values.CorrelationIdentifier]*values.RecordType
}

// newOrdinalJoinBirth probes a join plan's result value at cursor
// construction. nil (disabled) when rv is nil or carries no baked ordinal —
// the name-model cursor path, bit-identical to today. A LOUD error when rv
// contains baked ordinals but is not a *RecordConstructorValue: every shape
// the planner can legitimately produce for an ordinal-birth join is an RC
// (the seed, or the wrapper-merge-folded projection RC — the drift asserts
// pin that); anything else is a planner bug and must die at construction,
// never be silently demoted to the name model.
func newOrdinalJoinBirth(rv values.Value) (*ordinalJoinBirth, error) {
	if rv == nil || !values.ContainsBakedOrdinal(rv) {
		return nil, nil
	}
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, fmt.Errorf("RFC-173 ordinal join birth: result value contains baked ordinal references but is a %T, want *RecordConstructorValue (seed or folded projection RC) — planner bug", rv)
	}
	spans, _, windowsOK := ordinalJoinSpans(rc)
	return &ordinalJoinBirth{
		Enabled:    true,
		RC:         rc,
		OutputType: rcOutputType(rc),
		Spans:      spans,
		WindowsOK:  windowsOK,
		LegTypes:   legTypesFromResultValue(rc),
	}, nil
}

// enabled is the nil-safe Enabled read — a name-model cursor stores a nil
// *ordinalJoinBirth.
func (b *ordinalJoinBirth) enabled() bool { return b != nil && b.Enabled }

// oracleNameDatum is the §5 NAME-MODEL ORACLE's row for an ordinal-birth
// flatMap: DisablePositionalEmission promises "the pre-RFC-173 name model
// end-to-end", but the PLAN still carries the ordinal RC (planning is not
// oracle-gated), whose bare-named fields evaluate to a bare-keys-only map —
// while the pre-flip ANCHORED seed RC (NewAnchoredJoinRecord) carried an
// ALIAS.COL field per leg column PLUS the bare field at the last leg carrying
// that name. Downstream name-model consumers (a projection over a sort reading
// "U.NAME", the sort comparator's dotted keys) resolve against those qualified
// keys, so evaluating the ordinal RC directly silently NULLed every dotted
// read in the oracle phase (the dualwindow differential's first live catch on
// W3b).
//
// Reconstruct the anchored key set with genuine NAME-model resolution: each RC
// field evaluates over the name-keyed row context (the baked references read
// their display name through values.OracleBakedNameFallback — the same lookup
// the anchored RC's lazy FieldValue(QOV(leg), col) performed), written under
// the bare name (in-order map writes = the anchored last-leg-wins) and, for a
// PRISTINE seed (WindowsOK), under the leg-qualified ALIAS.COL key. A FOLDED
// projection RV gets bare keys only — its pre-flip name-model counterpart is
// the projection map, which never carried qualified keys. No TYPE.COL keys:
// the pre-flip flatMap Datum never had them either (qualifyTypeFallback is
// mergeRows machinery — the NLJ path, whose oracle Datum is mergeRows
// unchanged). TEST-ONLY (oracle callers); dies with the oracle in Slice 4.
func (b *ordinalJoinBirth) oracleNameDatum(rowCtx any) (map[string]any, error) {
	m := make(map[string]any, 2*len(b.RC.Fields))
	for _, f := range b.RC.Fields {
		v, err := f.Value.Evaluate(rowCtx)
		if err != nil {
			return nil, err
		}
		m[f.Name] = v
		if !b.WindowsOK {
			continue
		}
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			continue
		}
		if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
			m[strings.ToUpper(qov.Correlation.Name())+"."+strings.ToUpper(f.Name)] = v
		}
	}
	return m, nil
}

// legType resolves the adapter's leg type for a leg alias: from the spans when
// the RV is the pristine seed (WindowsOK), else from the RV's baked references
// (LegTypes), else nil (no baked reference names this leg — the adapter then
// only passes a positional row through or yields a zero-width row).
func (b *ordinalJoinBirth) legType(id values.CorrelationIdentifier) *values.RecordType {
	if b.WindowsOK {
		for _, s := range b.Spans {
			if s.Alias == id {
				return s.LegType
			}
		}
	}
	return b.LegTypes[id]
}

// legRows adapts the two join legs into the BIRTH-time binding map: alias →
// values.OrdinalRow via adaptLegPositional (positional legs flow through;
// name-model legs synthesize by leg type). A NIL QueryResult pointer is the
// NULL leg (LEFT/FULL null padding): its alias maps to nil, PRESENT — the
// binder then returns (nil, true) and the leg's baked references evaluate to
// NULL (contract ruling #3; the null extension falls out of evaluation).
func (b *ordinalJoinBirth) legRows(outerAlias, innerAlias string, outer, inner *QueryResult) (map[values.CorrelationIdentifier]values.OrdinalRow, error) {
	legs := make(map[values.CorrelationIdentifier]values.OrdinalRow, 2)
	if err := b.bindLeg(legs, outerAlias, outer); err != nil {
		return nil, err
	}
	if err := b.bindLeg(legs, innerAlias, inner); err != nil {
		return nil, err
	}
	return legs, nil
}

func (b *ordinalJoinBirth) bindLeg(legs map[values.CorrelationIdentifier]values.OrdinalRow, alias string, qr *QueryResult) error {
	id := values.NamedCorrelationIdentifier(alias)
	if qr == nil {
		legs[id] = nil // the deliberately-NULL leg: present, bound to nil
		return nil
	}
	row, err := adaptLegPositional(*qr, b.legType(id))
	if err != nil {
		return err
	}
	legs[id] = row
	return nil
}

// evaluateLegs births the positional row from already-adapted leg bindings —
// the SINGLE eval path (evaluateOrdinalJoinRow) under a birthLegBinder. Split
// from evaluate so the NLJ cursor can share one legRows adaptation between
// the predicate context and the birth.
func (b *ordinalJoinBirth) evaluateLegs(legs map[values.CorrelationIdentifier]values.OrdinalRow, base values.CorrelationBinder) (*PositionalRow, error) {
	return evaluateOrdinalJoinRow(b.RC, b.OutputType, &birthLegBinder{legs: legs, base: base})
}

// evaluateBound births the positional row from any pre-built leg binder — the
// zero-rebuild path the NLJ cursor's per-pair twoLegBinder uses (Torvalds
// W3a-2: no per-pair map, no per-pair re-adaptation).
func (b *ordinalJoinBirth) evaluateBound(bindings values.CorrelationBinder) (*PositionalRow, error) {
	return evaluateOrdinalJoinRow(b.RC, b.OutputType, bindings)
}

// twoLegBinder is the NLJ cursor's per-pair leg binder: exactly the join's
// two legs, pre-adapted rows plugged in per candidate pair — no map, no
// re-adaptation (the inner rows are a FIXED slice adapted once at cursor
// construction; the outer is adapted once per outer-row advance; Torvalds
// W3a-2 structural-perf catch). A nil row IS the deliberately-NULL leg
// (LEFT/FULL padding): (nil, true), contract ruling #3. Non-leg correlations
// delegate to base.
type twoLegBinder struct {
	outerID, innerID values.CorrelationIdentifier
	outer, inner     values.OrdinalRow
	base             values.CorrelationBinder
}

func (b *twoLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	switch id {
	case b.outerID:
		if b.outer == nil {
			return nil, true // the NULL leg
		}
		return b.outer, true
	case b.innerID:
		if b.inner == nil {
			return nil, true // the NULL leg
		}
		return b.inner, true
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// evaluate is the one-shot birth: adapt both legs (nil pointer = NULL leg),
// then evaluate the RC per-field into a PositionalRow under OutputType. base
// resolves outer correlations beyond the two legs (may be nil).
//
// Callers respect executor.DisablePositionalEmission (the §5 oracle): this is
// the primitive, oracle gating lives at the birth sites.
func (b *ordinalJoinBirth) evaluate(outerAlias, innerAlias string, outer, inner *QueryResult, base values.CorrelationBinder) (*PositionalRow, error) {
	legs, err := b.legRows(outerAlias, innerAlias, outer, inner)
	if err != nil {
		return nil, err
	}
	return b.evaluateLegs(legs, base)
}

// birthLegBinder is the BIRTH-time correlation binder: DIRECT per-leg bindings
// (Graefe W3 ruling: predicates and result-value evaluation need no windows at
// birth — each leg binds to its OWN leg-local row, so both baked (leg ordinal)
// and lazy (leg-relative resolveOrdinal) references read the right slot, even
// for the second leg). A key PRESENT with a nil value is the deliberately-NULL
// leg: GetCorrelationBinding returns (nil, true) and the baked node's
// `return bound, nil` arm yields NULL (contract ruling #3). Anything else
// delegates to base (outer correlations; nil base = unbound).
//
// The map-based binder survives ONLY for flatMap's one-shot evaluate (one
// binder per EMITTED row — no per-candidate cost there); the NLJ's per-pair
// hot path uses the fixed twoLegBinder below (Torvalds W3a-2 perf catch).
type birthLegBinder struct {
	legs map[values.CorrelationIdentifier]values.OrdinalRow
	base values.CorrelationBinder
}

// GetCorrelationBinding implements values.CorrelationBinder.
func (b *birthLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if row, present := b.legs[id]; present {
		if row == nil {
			return nil, true // NULL leg — untyped nil, the sanctioned nil-binding
		}
		return row, true
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// correlationBase adapts an *EvaluationContext into a CorrelationBinder,
// avoiding the typed-nil-interface trap (a nil *EvaluationContext stored in a
// non-nil interface would be called with a nil receiver).
func correlationBase(ec *EvaluationContext) values.CorrelationBinder {
	if ec == nil {
		return nil
	}
	return ec
}

// datumFromPositional derives the coexistence name-keyed Datum FROM the
// birthed positional row: map keyed by OutputType field names, LAST-WINS on
// duplicate names — matching RecordConstructorValue.Evaluate's in-order map
// writes, so the name model sees the same (conflated-on-dups) row it always
// did while the positional row keeps the duplicates distinct by ordinal. Used
// by ordinal-birth flatMap cursors, whose result value can no longer be
// evaluated over name contexts (baked reads there are loud errors).
func datumFromPositional(row *PositionalRow) map[string]any {
	if row == nil || row.Type == nil {
		return map[string]any{}
	}
	m := make(map[string]any, len(row.Type.Fields))
	for i, f := range row.Type.Fields {
		if i < len(row.Slots) {
			m[f.Name] = row.Slots[i]
		}
	}
	return m
}

// datumFromSpans derives the coexistence-window Datum for a SEED-shaped
// ordinal join row: bare column keys (last-wins across legs, exactly
// datumFromPositional/mergeRows semantics) PLUS the qualified "ALIAS.COL"
// keys per leg — the key set the name-model anchored RC / mergeRows always
// produced, which downstream name-model consumers (sort comparators'
// Datum fallback, streaming-aggregate group keys and operands, HAVING
// filters) resolve dotted references against. The W3b flip's live catch:
// bare-only Datums silently NULLed every dotted read over a gated FlatMap
// join (wrong sort order, empty HAVING output). Folded projection RVs keep
// bare-only datumFromPositional — their name-model counterpart is the
// projection map, which never carried qualified keys. Retires with the
// name map in Slice 4.
func datumFromSpans(row *PositionalRow, spans []legSpan) map[string]any {
	m := datumFromPositional(row)
	for _, s := range spans {
		prefix := strings.ToUpper(s.Alias.Name()) + "."
		for i := 0; i < s.Width; i++ {
			if v, ok := row.Get(s.Offset + i); ok {
				m[prefix+strings.ToUpper(s.LegType.Fields[i].Name)] = v
			}
		}
	}
	// Table-TYPE-qualified fallback keys (qualifyTypeFallback semantics:
	// fill-if-absent, only when the type name differs from the alias, only
	// for base-table legs — RecordName is set exactly there).
	for _, s := range spans {
		if s.LegType == nil || s.LegType.RecordName == "" || strings.EqualFold(s.LegType.RecordName, s.Alias.Name()) {
			continue
		}
		prefix := strings.ToUpper(s.LegType.RecordName) + "."
		for i := 0; i < s.Width; i++ {
			key := prefix + strings.ToUpper(s.LegType.Fields[i].Name)
			if _, exists := m[key]; exists {
				continue
			}
			if v, ok := row.Get(s.Offset + i); ok {
				m[key] = v
			}
		}
	}
	return m
}

// downstreamLegWindows computes, ONCE at operator construction, whether a
// consumer's input plan flows the 2-way ordinal join's MERGED positional row
// — i.e. whether leg windows apply to the rows this operator reads. It
// unwraps single-child PASSTHROUGH plans (operators whose cursors re-emit
// input QueryResults verbatim: sorts reorder, limit/skip/distinct/filters
// drop rows — none rewrites the row) down to the join, then derives the spans
// from the join's result value (ordinalJoinSpans — pristine seed only; a
// folded RV's output is a plain projection row, no windows).
//
// The passthrough set is deliberately EXACT, not permissive. Left out on
// purpose: FirstOrDefault/DefaultOnEmpty (fabricate a default row),
// InJoin (re-executes under bindings), FetchFromPartialRecord (row
// transform), unions/intersections (multi-child), projection/map/aggregation
// (row rewrites — projection/map are themselves dispatch sites). Missing a
// genuine passthrough UNDER-provides windows, which fails LOUD downstream
// (OrdinalResolutionError/BakedNameContextError), never silently wrong.
func downstreamLegWindows(input plans.RecordQueryPlan) ([]legSpan, bool) {
	for input != nil {
		switch p := input.(type) {
		case *plans.RecordQueryNestedLoopJoinPlan:
			spans, _, ok := ordinalJoinSpans(p.GetResultValue())
			return spans, ok
		case *plans.RecordQueryFlatMapPlan:
			spans, _, ok := ordinalJoinSpans(p.GetResultValue())
			return spans, ok
		case *plans.RecordQuerySortPlan:
			input = p.GetInner()
		case *plans.RecordQueryInMemorySortPlan:
			input = p.GetInner()
		case *plans.RecordQueryLimitPlan:
			input = p.GetInner()
		case *plans.RecordQueryDistinctPlan:
			input = p.GetInner()
		case *plans.RecordQueryTypeFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryPredicatesFilterPlan:
			input = p.GetInner()
		default:
			return nil, false
		}
	}
	return nil, false
}

// legWindowRowContext builds the downstream per-row eval context over the
// join's merged positional row: like EvaluationContext.RowContextPositional,
// but Correlations is the legWindowBinder — a window-era upper's direct leg
// reference (FieldValue(QOV(leg), col), lazy or baked) resolves LEG-LOCALLY
// through its window, an outer correlation delegates to the base
// EvaluationContext, and only an unbound (join-quantifier) reference falls to
// the bare merged row. Applied UNCONDITIONALLY on windowed inputs — even with
// no param/subquery/outer binding in play — because the leg references NEED
// the Correlations bindings (the bare merged row misreads them leg-relative:
// the W3 wrong-slot hazard).
func legWindowRowContext(pos values.OrdinalRow, ec *EvaluationContext, spans []legSpan) *values.RowEvalContext {
	rc := &values.RowEvalContext{
		Positional:   &spanAwareRow{parent: pos, spans: spans},
		Correlations: &legWindowBinder{base: correlationBase(ec), spans: spans, row: pos},
	}
	if ec != nil {
		rc.Binder = ec
		rc.ScalarSubqueries = ec.scalarSubqueries
	}
	return rc
}

// spanAwareRow is the merged positional row with QUALIFIED-name routing: the
// name model's physical plans reference merged-row columns as flat DOTTED
// names ("A.ID" — the executor pipeline's qualified merged-row keys), which
// carry no leg QOV for the Correlations windows to catch. GetByName splits a
// dotted name at its first dot and resolves alias → leg window → leg-local
// column, so window-era flat dotted references (projections, filters, sort
// keys) stay correct over the ordinal merged row; bare names keep the merged
// type's first-match (unchanged Slice 1 semantics). Ordinal access passes
// through untouched. DECLARED WINDOW SCAFFOLDING like the windows themselves
// (Graefe condition 2): dies when uppers bake, S3/S4.
type spanAwareRow struct {
	parent values.OrdinalRow
	spans  []legSpan
}

func (r *spanAwareRow) Get(ord int) (any, bool) { return r.parent.Get(ord) }

func (r *spanAwareRow) GetByName(name string) (any, bool) {
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		alias, col := name[:dot], name[dot+1:]
		// Alias namespace first (the name model's qualifyAlias precedence)…
		for _, s := range r.spans {
			if strings.EqualFold(s.Alias.Name(), alias) {
				w := &legWindowRow{parent: r.parent, legType: s.LegType, offset: s.Offset, width: s.Width}
				return w.GetByName(col)
			}
		}
		// …then the leg's TABLE-type namespace ("PA.ID" over `FROM PA AS s`)
		// — the ordinal counterpart of qualifyTypeFallback, keyed on the leg
		// type's RecordName (set only for base-table scan legs, exactly the
		// rows the name model wrote TYPE.COL fallback keys for).
		for _, s := range r.spans {
			if s.LegType != nil && s.LegType.RecordName != "" &&
				!strings.EqualFold(s.Alias.Name(), s.LegType.RecordName) &&
				strings.EqualFold(s.LegType.RecordName, alias) {
				w := &legWindowRow{parent: r.parent, legType: s.LegType, offset: s.Offset, width: s.Width}
				return w.GetByName(col)
			}
		}
		return nil, false
	}
	return r.parent.GetByName(name)
}

// TypeNames feeds OrdinalResolutionError diagnostics (values.ordinalRowNames).
func (r *spanAwareRow) TypeNames() []string {
	if tn, ok := r.parent.(interface{ TypeNames() []string }); ok {
		return tn.TypeNames()
	}
	return nil
}
