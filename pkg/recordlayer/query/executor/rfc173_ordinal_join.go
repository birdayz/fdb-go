package executor

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// This file is the RFC-173 Slice 2 W3a ordinal-join executor substrate: the
// primitives the 2-way wedge's merged positional row is built from. It is DARK
// at this stage — no production code path constructs the ordinal join RC yet;
// W3b (the translator seed flip) is what makes these live. Everything here
// derives from the W3 pre-code ruling (rfcs/173-ordinal-column-resolution.md
// §4 Slice 2): spans derive from the RC (condition 1), leg windows are
// declared window scaffolding (condition 2), the window implements
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

// ordinalJoinSpans is the strict detector + deriver for the RFC-173 2-way
// ordinal join RC (Graefe W3 ruling: structural probe, emergent from the
// representation — no plan flag for Slice 4 to delete). It returns
// ok=false when v is not an ordinal join seed at all — not a
// *RecordConstructorValue, or an RC with ZERO baked fields (every name-model
// RC, anchored or plain, lands here and stays on the name path).
//
// But an RC with AT LEAST ONE baked field MUST be a well-formed ordinal join
// RC; anything else PANICS — a malformed ordinal seed is a planner bug, and a
// silent demotion to the name model would hide it (the exact failure mode the
// drift asserts exist to kill). Well-formed means: every field's Value is a
// BAKED *FieldValue whose Child is a *QuantifiedObjectValue flowing a
// *RecordType; the fields form EXACTLY TWO consecutive runs grouped by QOV
// correlation (the 2-way wedge); within each run the baked ordinals are
// exactly 0..width-1 ascending; and each run covers its leg type in full
// (width == len(LegType.Fields)).
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
	anyBaked := false
	for _, f := range rc.Fields {
		if fv, isFV := f.Value.(*values.FieldValue); isFV && fv.Resolved != nil {
			anyBaked = true
			break
		}
	}
	if !anyBaked {
		return nil, nil, false // name-model RC — not an ordinal join seed
	}

	mergedFields := make([]values.Field, len(rc.Fields))
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: field %d (%q) is %T (baked=%v) — an ordinal seed bakes EVERY leg column; a mixed baked/lazy RC is a planner bug", i, f.Name, f.Value, isFV && fv.Resolved != nil))
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: field %d (%q) is baked over a %T child, want *QuantifiedObjectValue (the leg reference)", i, f.Name, fv.Child))
		}
		legType, isRT := qov.Type().(*values.RecordType)
		if !isRT {
			panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: field %d (%q) leg %s flows %T, want *RecordType", i, f.Name, qov.Correlation, qov.Type()))
		}
		ord := fv.Resolved.Ordinal
		if len(spans) == 0 || spans[len(spans)-1].Alias != qov.Correlation {
			// A new leg run begins — its baked ordinals must start at 0.
			if ord != 0 {
				panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: leg %s run starts at field %d with baked ordinal %d, want 0 — run ordinals must be exactly 0..width-1 ascending", qov.Correlation, i, ord))
			}
			spans = append(spans, legSpan{Alias: qov.Correlation, LegType: legType, Offset: i, Width: 1})
		} else {
			cur := &spans[len(spans)-1]
			if ord != cur.Width {
				panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: leg %s field %d baked at ordinal %d, want %d — run ordinals must be exactly 0..width-1 ascending (no gaps, no reorders)", qov.Correlation, i, ord, cur.Width))
			}
			cur.Width++
		}
		mergedFields[i] = values.Field{Name: f.Name, FieldType: fv.Type(), Ordinal: i}
	}

	if len(spans) != 2 {
		panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: %d leg runs — the ordinal wedge is 2-way, exactly two consecutive leg runs required (N-way is Slice 3)", len(spans)))
	}
	total := 0
	for _, s := range spans {
		if s.Width != len(s.LegType.Fields) {
			panic(fmt.Sprintf("RFC-173 ordinal join RC malformed: leg %s run covers %d columns but its leg type has %d — the seed concatenates FULL legs", s.Alias, s.Width, len(s.LegType.Fields)))
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
	names := make([]string, len(w.legType.Fields))
	for i, f := range w.legType.Fields {
		names[i] = f.Name
	}
	return names
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
// This is format-only bridging — correlation-SEMANTIC bridging (an ordinal
// join consumed as a leg of a name-model merge select) is explicitly out of
// scope and prevented upstream by the W2 cluster-arity gate (RFC-173 §4
// Slice 2 coexistence scoping).
func adaptLegPositional(qr QueryResult, legType *values.RecordType) values.OrdinalRow {
	if qr.Positional != nil {
		return qr.Positional
	}
	row := NewPositionalRow(legType)
	if legType == nil {
		return row
	}
	if m, isMap := qr.Datum.(map[string]any); isMap {
		for i, f := range legType.Fields {
			if v, present := m[f.Name]; present {
				row.Slots[i] = v
			}
		}
	}
	return row
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
