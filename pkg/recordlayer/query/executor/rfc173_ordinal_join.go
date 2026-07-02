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

// assertOrdinalJoinSeed is the LOUD seed-shape validator: the W3b translator
// calls it on every ordinal join RC it builds, where the pristine shape IS
// guaranteed by construction — every field a baked reference over a leg QOV
// flowing a RecordType, exactly two consecutive full-coverage runs with
// ordinals 0..width-1. Any violation panics: at the SEED a malformed ordinal
// RC is unconditionally a planner bug (Graefe W3a-1 NAK fix: strictness lives
// seed-time, where legitimate result-value rewrites — wrapper merges, folded
// projections, partial coverage — cannot yet have happened; the cursor-side
// probe above must decline those, never panic).
func assertOrdinalJoinSeed(rc *values.RecordConstructorValue) {
	if rc == nil || len(rc.Fields) == 0 {
		panic("RFC-173 ordinal join seed malformed: empty RC")
	}
	type run struct {
		alias   values.CorrelationIdentifier
		legType *values.RecordType
		width   int
	}
	var runs []run
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) is %T (baked=%v) — the seed bakes EVERY leg column", i, f.Name, f.Value, isFV && fv.Resolved != nil))
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) is baked over a %T child, want *QuantifiedObjectValue (the leg reference)", i, f.Name, fv.Child))
		}
		legType, isRT := qov.Type().(*values.RecordType)
		if !isRT {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) leg %s flows %T, want *RecordType", i, f.Name, qov.Correlation, qov.Type()))
		}
		ord := fv.Resolved.Ordinal
		if len(runs) == 0 || runs[len(runs)-1].alias != qov.Correlation {
			if ord != 0 {
				panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: leg %s run starts at field %d with baked ordinal %d, want 0 — run ordinals must be exactly 0..width-1 ascending", qov.Correlation, i, ord))
			}
			runs = append(runs, run{alias: qov.Correlation, legType: legType, width: 1})
		} else {
			cur := &runs[len(runs)-1]
			if ord != cur.width {
				panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: leg %s field %d baked at ordinal %d, want %d — run ordinals must be exactly 0..width-1 ascending (no gaps, no reorders)", qov.Correlation, i, ord, cur.width))
			}
			cur.width++
		}
	}
	if len(runs) != 2 {
		panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: %d leg runs — the ordinal wedge seed is 2-way, exactly two consecutive leg runs required (N-way is Slice 3)", len(runs)))
	}
	for _, r := range runs {
		if r.width != len(r.legType.Fields) {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: leg %s run covers %d columns but its leg type has %d — the seed concatenates FULL legs", r.alias, r.width, len(r.legType.Fields)))
		}
	}
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
		return qr.Positional, nil
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
