package executor

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file is the RFC-173 ordinal-join executor machinery: the primitives
// the gated wedge's merged positional row is built from — LIVE since W3b,
// N-way flat (and positional-merge birthing) since the S3 fulcrum: the
// translator seeds gated join clusters with the ordinal RC these
// consume. Everything here derives from the W3 pre-code ruling
// (rfcs/173-ordinal-column-resolution.md §4 Slice 2): spans derive from the
// RC (condition 1), leg windows are declared window scaffolding that dies
// when the uppers bake, S3/S4 (condition 2), the window implements
// values.OrdinalRow completely so no new eval arm exists (condition 3), and
// the wrong-slot hazard is pinned red→green (condition 4).

// legSpan is one leg's slot range within the ordinal join's merged
// positional row: the leg quantifier's alias, the leg's own RecordType, and
// the half-open window [Offset, Offset+Width) its columns occupy in the merged
// row. Spans are DERIVED from the ordinal join RC by ordinalJoinSpans at
// cursor construction (review W3 condition 1: the RC is the single authority)
// — never stored or maintained as independent bookkeeping.
type legSpan struct {
	Alias   values.CorrelationIdentifier
	LegType *values.RecordType
	Offset  int
	Width   int
}

// ordinalJoinSpans is the cursor-side WINDOWS-ELIGIBILITY probe: it detects
// whether v is exactly the ordinal join SEED RC (the concatenation of
// full legs, every field a baked leg reference) and derives the leg spans
// + merged type from it. DECLINE-ONLY (ok=false) for every other shape — a
// result value that is not the pristine seed is NOT a planner bug here: the
// pure-wrapper merge the drift assert deliberately allows rewrites the select's
// result value into the parent PROJECTION's RC, which legitimately mixes baked
// leg references (compose-folded through the seed) with computed values, or
// covers a leg partially (`SELECT b.y FROM a JOIN b`), or collapses to a
// single run (review W3a-1 NAK: the earlier any-baked⟹well-formed-or-panic
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
	if spans, mergedType, ok = ordinalJoinSpansOf(v, nil); ok {
		return spans, mergedType, true
	}
	// RFC-173 W4c: the MIXED single-source lateral-unnest seed (baked outer run
	// + a bare-scalar element) is not a pristine all-baked seed, so
	// ordinalJoinSpansOf declines it; derive its windows (outer leg + a
	// synthesized 1-field element leg) so the shadowing element gets its own
	// namespace. Decline-only for every other shape.
	return unnestMixedSeedSpans(v)
}

// unnestMixedSeedSpans derives leg windows for the RFC-173 W4c MIXED single-
// source lateral-unnest ordinal seed (rfc173_w4c_unnest_seed.go): a full baked
// OUTER leg run followed by EXACTLY ONE trailing bare-QuantifiedObjectValue
// element field over a NON-record type — Java's isPrimitive() whole-object
// element (a scalar bound DIRECTLY, never an ofOrdinal baked leg; it binds RAW
// at birth, see ordinalJoinBirth.RawLegs). ordinalJoinSpansOf DECLINES this
// shape (the element is a bare QOV, not a FrontierPinned FieldValue), so without
// windows the FlatMap output resolves references by positional FIRST-match —
// which mis-resolves a name SHARED by the element AS alias and an outer column
// (`FROM t, t.arr AS x` where t also has a column x) to the OUTER duplicate, and
// leaves a qualified outer reference (`t.id`) unresolvable in a wrapping
// name-model join's merged row. This synthesizes the element's OWN 1-field leg
// window (Alias = the element QOV correlation = the unnest AS alias; the leg's
// single column = the RC field's name) so the element and the outer each carry
// their own ALIAS.COL namespace — mirroring the WITH-ORDINALITY inner leg (a
// genuine 2-field record leg, which ordinalJoinSpansOf already windows) and the
// name-model buildUnnestResultValue's AS-alias shadowing. DECLINE-only
// (ok=false) for every other shape: the trailing-bare-QOV-over-non-record
// discriminator never matches a folded projection, a baked+constant fold, or the
// S3 positional-merge RC (whose bare QOVs are over RECORD leg types).
func unnestMixedSeedSpans(v values.Value) (spans []legSpan, mergedType *values.RecordType, ok bool) {
	rc, isRC := v.(*values.RecordConstructorValue)
	if !isRC || len(rc.Fields) < 2 {
		return nil, nil, false
	}
	n := len(rc.Fields)
	// The trailing element leg: a bare QOV over a NON-record type (the RawLeg
	// scalar element, Java's primitive whole-object branch). A struct element
	// (bare QOV over a RecordType) and every baked/constant field decline here —
	// keeping this fallback scoped to exactly the scalar-element mixed seed.
	elemQOV, isQOV := rc.Fields[n-1].Value.(*values.QuantifiedObjectValue)
	if !isQOV {
		return nil, nil, false
	}
	if _, isRecord := elemQOV.Type().(*values.RecordType); isRecord {
		return nil, nil, false
	}
	// The OUTER leg: a single FULL baked run from ordinal 0 over one alias — the
	// same run/coverage discipline ordinalJoinSpansOf enforces per leg.
	mergedFields := make([]values.Field, n)
	var outer *legSpan
	for i := 0; i < n-1; i++ {
		f := rc.Fields[i]
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return nil, nil, false
		}
		alias, legType, ord, resolved := resolveSpanLeaf(fv, nil)
		if !resolved {
			return nil, nil, false
		}
		if outer == nil {
			if ord != 0 {
				return nil, nil, false
			}
			outer = &legSpan{Alias: alias, LegType: legType, Offset: 0, Width: 1}
		} else {
			if alias != outer.Alias || ord != outer.Width {
				return nil, nil, false // a second baked leg or a gap — not this seed
			}
			outer.Width++
		}
		mergedFields[i] = values.Field{Name: f.Name, FieldType: fv.Type(), Ordinal: i}
	}
	if outer == nil || outer.LegType == nil || outer.Width != len(outer.LegType.Fields) {
		return nil, nil, false // partial outer coverage — a folded projection, not the seed
	}
	// Synthesize the element's 1-field leg window so `<AS>.<AS>` resolves
	// alias → element window → the sole column, exactly as datumFromSpans/the
	// name model qualify the element leg.
	elemName := strings.ToUpper(rc.Fields[n-1].Name)
	elemType := elemQOV.Type()
	elemLegType := &values.RecordType{Fields: []values.Field{{Name: elemName, FieldType: elemType, Ordinal: 0}}}
	mergedFields[n-1] = values.Field{Name: elemName, FieldType: elemType, Ordinal: n - 1}
	spans = []legSpan{
		*outer,
		{Alias: elemQOV.Correlation, LegType: elemLegType, Offset: n - 1, Width: 1},
	}
	return spans, &values.RecordType{Fields: mergedFields}, true
}

// ordinalJoinSpansOf is ordinalJoinSpans with FUSED-reference resolution (the
// S3 fulcrum's translated-top shape): a multi-accessor pinned path rooted at a
// MERGE quantifier ([( _i, i), (col, j)] — the partition rule's TranslationMap
// output) resolves to its LEAF leg by descending legRVs — the merge
// quantifier's child result value (the positional-merge RC mapping _i →
// QOV(leg)), the ONLY place the merged-away legs' user aliases survive. With
// legRVs nil, multi-accessor paths decline and this is exactly the S2
// single-accessor probe.
func ordinalJoinSpansOf(v values.Value, legRVs map[values.CorrelationIdentifier]values.Value) (spans []legSpan, mergedType *values.RecordType, ok bool) {
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
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			// Not every field a PINNED baked leg ref — not the seed. Unpinned
			// baked nodes (the recursive-CTE wrap) carry no join-frontier
			// contract; the probe keys on the FrontierPinned bit, not bare
			// bakedness (unification review ruling).
			return nil, nil, false
		}
		alias, legType, ord, resolved := resolveSpanLeaf(fv, legRVs)
		if !resolved {
			return nil, nil, false
		}
		if len(spans) == 0 || spans[len(spans)-1].Alias != alias {
			if ord != 0 {
				return nil, nil, false // run must start at leg ordinal 0
			}
			spans = append(spans, legSpan{Alias: alias, LegType: legType, Offset: i, Width: 1})
		} else {
			cur := &spans[len(spans)-1]
			if ord != cur.Width {
				return nil, nil, false // gap or reorder — not the concat
			}
			cur.Width++
		}
		mergedFields[i] = values.Field{Name: f.Name, FieldType: fv.Type(), Ordinal: i}
	}

	if len(spans) < 2 {
		return nil, nil, false // 1 run = folded single leg (S3 fulcrum lifted the exactly-2 wedge: N-leg flat seeds are live)
	}
	total := 0
	for _, s := range spans {
		if s.Width != len(s.LegType.Fields) {
			return nil, nil, false // partial leg coverage — a folded projection
		}
		total += s.Width
	}
	// Spans-consistency assert (review W3 extra pin). Unreachable given the
	// run construction above — pinned anyway so a future edit that breaks the
	// derivation dies here, not in a downstream window misread.
	if total != len(rc.Fields) {
		panic(fmt.Sprintf("RFC-173 ordinal join spans inconsistent: sum(widths)=%d, RC has %d fields — spans must derive exactly from the RC", total, len(rc.Fields)))
	}
	return spans, &values.RecordType{Fields: mergedFields}, true
}

// resolveSpanLeaf resolves one RV field to its LEAF leg (alias, leg type,
// leg-local ordinal). A single-accessor path is the S2 seed shape: the leg is
// the child QOV itself. A multi-accessor FUSED path descends each intermediate
// accessor through the current quantifier's child RV in legRVs: the pristine
// positional-merge RC's `_i` slot is a bare leg QOV (continue with it), and a
// TRANSLATED merge RV's slot is itself a pinned baked/fused reference (a
// deeper partition round rebased it) — COMPOSE its path into the walk, exactly
// what evaluating the two nodes in sequence does at runtime. Any unresolvable
// step declines (nil legRVs, unrecognized slot shape, out-of-range accessor):
// fail-safe, the caller reports no windows and downstream stays loud rather
// than mis-windowed. The depth cap is a defensive backstop — plan-derived
// legRVs form a tree, so a cycle is impossible by construction.
func resolveSpanLeaf(fv *values.FieldValue, legRVs map[values.CorrelationIdentifier]values.Value) (alias values.CorrelationIdentifier, legType *values.RecordType, legOrd int, ok bool) {
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		return alias, nil, 0, false
	}
	alias = qov.Correlation
	legType, isRT := qov.Type().(*values.RecordType)
	if !isRT {
		return alias, nil, 0, false
	}
	accs := fv.Resolved.Accessors
	for depth := 0; len(accs) > 1; depth++ {
		if depth > 32 {
			return alias, nil, 0, false
		}
		rc, isRC := legRVs[alias].(*values.RecordConstructorValue)
		if !isRC {
			return alias, nil, 0, false
		}
		i := accs[0].Ordinal
		if i < 0 || i >= len(rc.Fields) {
			return alias, nil, 0, false
		}
		switch slot := rc.Fields[i].Value.(type) {
		case *values.QuantifiedObjectValue:
			alias = slot.Correlation
			if legType, isRT = slot.Type().(*values.RecordType); !isRT {
				return alias, nil, 0, false
			}
			accs = accs[1:]
		case *values.FieldValue:
			if slot.Resolved == nil || !slot.Resolved.FrontierPinned {
				return alias, nil, 0, false
			}
			slotQOV, isQ := slot.Child.(*values.QuantifiedObjectValue)
			if !isQ {
				return alias, nil, 0, false
			}
			alias = slotQOV.Correlation
			if legType, isRT = slotQOV.Type().(*values.RecordType); !isRT {
				return alias, nil, 0, false
			}
			composed := make([]values.ResolvedAccessor, 0, len(slot.Resolved.Accessors)+len(accs)-1)
			composed = append(composed, slot.Resolved.Accessors...)
			accs = append(composed, accs[1:]...)
		default:
			return alias, nil, 0, false
		}
	}
	return alias, legType, accs[0].Ordinal, true
}

// joinPlanRV returns a join plan's result value; nil for any other plan.
func joinPlanRV(p plans.RecordQueryPlan) values.Value {
	switch t := p.(type) {
	case *plans.RecordQueryNestedLoopJoinPlan:
		return t.GetResultValue()
	case *plans.RecordQueryFlatMapPlan:
		return t.GetResultValue()
	}
	return nil
}

// addJoinLegRV records one leg quantifier's child result value into legRVs
// (first-seen wins — the top-most binding is the one the top RV's references
// resolve through) and recurses into the leg's own join plan so deeper merge
// levels and nested boxes resolve too.
func addJoinLegRV(out map[values.CorrelationIdentifier]values.Value, alias values.CorrelationIdentifier, leg plans.RecordQueryPlan) {
	lj := unwrapToJoinPlan(leg)
	if lj == nil {
		return
	}
	if rv := joinPlanRV(lj); rv != nil {
		if _, seen := out[alias]; !seen {
			out[alias] = rv
		}
	}
	collectJoinLegRVs(lj, out)
}

// collectJoinLegRVs maps each leg quantifier of a join plan — and,
// recursively, of its join-plan legs — to the result value its rows are born
// from. This is the alias-recovery substrate for translated tops and box
// splicing: the partition rule's TranslationMap erases merged-away leg aliases
// from the upper RV, and a FULL box's leg type is an anonymous concat; both
// survive only in the leg subplans' own result values.
func collectJoinLegRVs(join plans.RecordQueryPlan, out map[values.CorrelationIdentifier]values.Value) {
	switch t := join.(type) {
	case *plans.RecordQueryFlatMapPlan:
		addJoinLegRV(out, t.GetOuterAlias(), t.GetOuter())
		addJoinLegRV(out, t.GetInnerAlias(), t.GetInner())
	case *plans.RecordQueryNestedLoopJoinPlan:
		addJoinLegRV(out, values.NamedCorrelationIdentifier(t.GetOuterAlias()), t.GetOuter())
		addJoinLegRV(out, values.NamedCorrelationIdentifier(t.GetInnerAlias()), t.GetInner())
	}
}

// spliceLegSpans recursively replaces a span whose LEG is itself a join plan
// (a FULL box's gated-join leg, per the fulcrum's ordinalLegColumns concat)
// with the leg's OWN spans offset into the parent row: dotted upper
// references name LEAF table aliases — the intermediate box alias
// (sourceAlias, the buried rightmost table) never appears in a user query.
// The splice is fail-safe: a leg whose RV yields no spans, or whose spans do
// not tile the span's width exactly, keeps the opaque parent span unchanged
// (that width guard also breaks the box-alias/leaf-alias shadowing case —
// sourceAlias names the box after its rightmost LEAF, whose own width can
// never equal the whole box's).
func spliceLegSpans(spans []legSpan, legRVs map[values.CorrelationIdentifier]values.Value) []legSpan {
	return spliceLegSpansDepth(spans, legRVs, 0)
}

// spliceLegSpansDepth carries the defensive recursion cap — the same
// no-cycle-by-construction trust assumption resolveSpanLeaf bounds (plan
// trees are finite; the cap only matters if a future edit breaks that).
func spliceLegSpansDepth(spans []legSpan, legRVs map[values.CorrelationIdentifier]values.Value, depth int) []legSpan {
	if depth > 32 {
		return spans
	}
	out := make([]legSpan, 0, len(spans))
	for _, s := range spans {
		rv := legRVs[s.Alias]
		if rv == nil {
			out = append(out, s)
			continue
		}
		sub, _, ok := ordinalJoinSpansOf(rv, legRVs)
		if !ok {
			out = append(out, s)
			continue
		}
		total := 0
		for _, ss := range sub {
			total += ss.Width
		}
		if total != s.Width {
			out = append(out, s)
			continue
		}
		for _, ss := range spliceLegSpansDepth(sub, legRVs, depth+1) {
			out = append(out, legSpan{Alias: ss.Alias, LegType: ss.LegType, Offset: s.Offset + ss.Offset, Width: ss.Width})
		}
	}
	return out
}

// joinPlanSpans derives the leg windows for a join plan's output row: the
// span probe with fused-reference resolution over the plan's leg RVs, then
// the recursive box splice. Subsumes the bare ordinalJoinSpans probe (a
// pristine S2 seed needs no legRVs and no splice — identical output).
func joinPlanSpans(join plans.RecordQueryPlan) ([]legSpan, bool) {
	rv := joinPlanRV(join)
	if rv == nil {
		return nil, false
	}
	legRVs := make(map[values.CorrelationIdentifier]values.Value)
	collectJoinLegRVs(join, legRVs)
	spans, _, ok := ordinalJoinSpansOf(rv, legRVs)
	if !ok {
		// RFC-173 W4c: the MIXED single-source lateral-unnest seed windows via
		// the dedicated derivation (bare-scalar element leg). It has no fused/box
		// legs, so the spans stand without a splice.
		if ms, _, mok := unnestMixedSeedSpans(rv); mok {
			return ms, true
		}
		return nil, false
	}
	return spliceLegSpans(spans, legRVs), true
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
// (review W3 condition 2): Java has no merged-row-with-leg-views — its uppers
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
// the ordinal join: a reference to a leg alias is bound to that leg's
// window over the merged row, anything else delegates to base. DECLARED WINDOW
// SCAFFOLDING (review W3 condition 2) — it exists only because window-era
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
// NON-EMPTY Datum (review W3a-1 catch): a name-model MERGE-shaped leg
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
		// leg type (PR-447 review catch, superseding the width-only
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
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return true // only PINNED seed refs carry join-leg types
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
// cursor construction (review W3 ruling: detection is the structural
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
	// DatumSpans are the spans the coexistence Datum derives from
	// (datumFromSpans / oracleNameDatum's qualified keys): the SPLICED view —
	// a box leg's window opened to its leaf tables, whose aliases dotted
	// reads actually name. Kept SEPARATE from Spans, which the cursor's own
	// leg ADAPTER resolves against (adaptLegPositional needs the box-level
	// type for the box-alias binding; the spliced leaf window would misfit
	// the leg's whole concat row). Equal to Spans when no splice applies.
	// ONLY the FlatMap cursor splices these (its Datum derives from the
	// birth); the NLJ cursor's Datum is mergeRows/mergeShapeDatum and never
	// consults them — a future NLJ caller of oracleNameDatum/datumFromSpans
	// must splice first or it inherits box-alias qualification silently.
	DatumSpans []legSpan
	// LegTypes are the leg types recovered from the RV's baked references —
	// the adapter's leg types when the RV is folded and Spans are unavailable.
	LegTypes map[values.CorrelationIdentifier]*values.RecordType
	// RawLegs are legs referenced by a BARE QuantifiedObjectValue over a
	// NON-record type — the RFC-142 W4c lateral-unnest bare-scalar (or struct)
	// element leg, whose whole flowed Datum IS the column (Java's isPrimitive()
	// branch: the element is referenced directly, never ofOrdinal — ofOrdinal
	// over a scalar throws). Such a leg must bind its RAW Datum, never be adapted
	// to an OrdinalRow: adaptLegPositional would synthesize an EMPTY positional
	// row for a non-record Datum, so the element would birth NULL (the coexistence
	// Datum masks it today; the positional row — S4's sole authority — would be
	// wrong). Discriminated by leg SHAPE at construction, never a per-plan flag.
	// The record-leg OrdinalRow path (adaptLegPositional) is unchanged.
	RawLegs map[values.CorrelationIdentifier]struct{}
	// OrdinalityLegs are legs whose Datum is a WITH-ORDINALITY Explode row,
	// keyed by the INTERNAL OrdinalFieldName positions (`_0`=element,
	// `_1`=1-based ordinal). Such a leg binds STRICTLY POSITIONALLY (slot i =
	// Datum[_i]), never by the leg type's AS/AT alias NAMES — a user may spell an
	// alias `_0`/`_1` (`FROM t, t.arr AS "_1" AT "_0"`), and a name lookup would
	// route the wrong internal key. Distinguished by PRODUCER CONTEXT (the
	// FlatMap knows its inner is an ordinality Explode — newFlatMapCursor sets
	// this), NOT the Datum SHAPE: a name-model leg whose own columns are aliased
	// `_0`/`_1` is shape-identical but binds correctly by NAME (adaptLegPositional).
	OrdinalityLegs map[values.CorrelationIdentifier]struct{}
}

// newOrdinalJoinBirth probes a join plan's result value at cursor
// construction. nil (disabled) when rv is nil or carries no baked ordinal —
// the name-model cursor path, bit-identical to today. A LOUD error when rv
// contains baked ordinals but is not a *RecordConstructorValue: every shape
// the planner can legitimately produce for an ordinal-birth join is an RC
// (the seed, or the wrapper-merge-folded projection RC — the drift asserts
// pin that); anything else is a planner bug and must die at construction,
// never be silently demoted to the name model.
func newOrdinalJoinBirth(rv values.Value, preds []predicates.QueryPredicate) (*ordinalJoinBirth, error) {
	// Two birth triggers during the S3 window:
	//   - a FrontierPinned baked reference anywhere in the RV (the S2 flat
	//     seed, its folds, and the post-translation MIXED upper shape whose
	//     fields are ofOrdinal-over-innerMerge alongside bare leg QOVs);
	//   - the S3-W2 positional-merge RC (ALL fields bare `_i`-named QOVs —
	//     the lowest merge level carries no baked refs at all, but its rows
	//     must birth positional: the level above reads them by ordinal).
	// Nothing produces the merge shape until the W2 fulcrum — dark.
	if rv == nil || (!values.ContainsBakedOrdinal(rv) && !values.IsPositionalMergeRC(rv)) {
		return nil, nil
	}
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, fmt.Errorf("RFC-173 ordinal join birth: result value contains baked ordinal references but is a %T, want *RecordConstructorValue (seed or folded projection RC) — planner bug", rv)
	}
	spans, _, windowsOK := ordinalJoinSpans(rc)
	// LegTypes come from the RESULT VALUE *and* the join PREDICATES (review
	// PR-447 P1): a folded projection RV can DROP a leg entirely while a
	// baked cross-leg ON predicate still references it — collecting from the
	// RV alone left the dropped leg typeless, and a name-model (Datum-only)
	// row for it adapted to a ZERO-WIDTH binding that blew up the predicate
	// (loud OrdinalResolutionError, "row columns []") on a legitimate plan.
	legTypes := legTypesFromResultValue(rc)
	// Bare QOV fields carry their leg's type directly (the S3 merge shape's
	// `_i` columns and the mixed upper's untranslated leg): without this a
	// bare-QOV leg is typeless and its adapter degrades to the Datum
	// synthesis path even when the leg flows a typed row. Same
	// conflict-impossibility invariant as widenLegTypesFromPlan — every
	// source copies the one planner-constructed typed QOV — asserted the
	// same way (a silent first-wins here would be an inconsistent assertion
	// of a load-bearing invariant, review nit).
	var rawLegs map[values.CorrelationIdentifier]struct{}
	for _, f := range rc.Fields {
		if qov, isQOV := f.Value.(*values.QuantifiedObjectValue); isQOV {
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := legTypes[qov.Correlation]; !seen {
					legTypes[qov.Correlation] = rt
				} else if len(prev.Fields) != len(rt.Fields) {
					panic(fmt.Sprintf("RFC-173: leg %s carries DIVERGENT types (%d vs %d fields) across the RV's bare-QOV and baked-reference sources — all references must copy the one planner-constructed typed QOV (planner bug)", qov.Correlation, len(prev.Fields), len(rt.Fields)))
				}
			} else {
				// A bare QOV over a NON-record type: the W4c lateral-unnest
				// bare-scalar/struct element leg — bind its whole flowed Datum
				// raw (see RawLegs).
				if rawLegs == nil {
					rawLegs = map[values.CorrelationIdentifier]struct{}{}
				}
				rawLegs[qov.Correlation] = struct{}{}
			}
		}
	}
	for _, p := range preds {
		predicates.ReplaceValues(p, func(v values.Value) values.Value {
			fv, isFV := v.(*values.FieldValue)
			if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
				return v
			}
			if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
				if rt, isRT := qov.Type().(*values.RecordType); isRT {
					if _, seen := legTypes[qov.Correlation]; !seen {
						legTypes[qov.Correlation] = rt
					}
				}
			}
			return v
		})
	}
	return &ordinalJoinBirth{
		Enabled:    true,
		RC:         rc,
		OutputType: rcOutputType(rc),
		Spans:      spans,
		WindowsOK:  windowsOK,
		DatumSpans: spans,
		LegTypes:   legTypes,
		RawLegs:    rawLegs,
	}, nil
}

// enabled is the nil-safe Enabled read — a name-model cursor stores a nil
// *ordinalJoinBirth.
func (b *ordinalJoinBirth) enabled() bool { return b != nil && b.Enabled }

// widenLegTypesFromPlan widens LegTypes with every BAKED leg reference found
// in a physical plan tree's predicate surfaces — PredicatesFilter/Filter
// predicates and scan/index comparison operands. The FlatMap half of the
// PR-447 review P1 (@claude final-pass catch): the correlated implementation
// pushes the join's baked ON references INTO the inner plan as SARGs and
// residual filters, so a folded result value that DROPPED a leg leaves the
// birth typeless for it even though the inner plan still references it — a
// Datum-only row for that leg (aggregate-box outer) then adapts to a
// zero-width binding and dies loudly on a legitimate plan. Called by
// newFlatMapCursor with the inner plan; the NLJ path gets the same widening
// directly from its predicate list in newOrdinalJoinBirth.
//
// WINDOW SCAFFOLDING like adaptLegPositional itself (review): the walk exists
// only because folded RVs and Datum-only legs coexist — it dies with the
// adapter in Slice 4 (all-positional legs leave nothing to synthesize). Its
// exact-set plan arms fail SAFE: a missed predicate surface leaves the leg
// typeless → loud zero-width death, never silent.
//
// Multiple sources (RV, join preds, pushed SARGs) can each carry a leg's
// type; this is CONFLICT-IMPOSSIBLE, not precedence — every baked reference
// is a copy of the ONE seed-constructed typed QOV, and every transformation
// preserves marker and type. The width-divergence panic below asserts that
// load-bearing invariant.
func (b *ordinalJoinBirth) widenLegTypesFromPlan(plan plans.RecordQueryPlan) {
	if !b.enabled() || plan == nil {
		return
	}
	collect := func(v values.Value) values.Value {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return v
		}
		if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := b.LegTypes[qov.Correlation]; !seen {
					b.LegTypes[qov.Correlation] = rt
				} else if len(prev.Fields) != len(rt.Fields) {
					panic(fmt.Sprintf("RFC-173: leg %s carries DIVERGENT baked types (%d vs %d fields) across the RV/predicate/SARG sources — all baked references must copy the one seed-constructed typed QOV (planner bug)", qov.Correlation, len(prev.Fields), len(rt.Fields)))
				}
			}
		}
		return v
	}
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, collect)
		}
	}
	// RecordQueryNestedLoopJoinPlan also implements GetPredicates but is
	// DELIBERATELY omitted (review note, arm-parity precedent): baked
	// references exist only in gated joins, and join legs are categorically
	// ineligible in the S2 wedge — a baked-ref-bearing NLJ cannot appear
	// inside a gated flatMap's inner plan. Re-examine at the S3 gate widening.
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		switch t := p.(type) {
		case *plans.RecordQueryPredicatesFilterPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, collect)
			}
		case *plans.RecordQueryFilterPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, collect)
			}
		case *plans.RecordQueryScanPlan:
			for _, cr := range t.GetScanComparisons() {
				if cr.IsEquality() {
					collectComparison(cr.GetEqualityComparison())
				} else if cr.IsInequality() {
					for _, c := range cr.GetInequalityComparisons() {
						collectComparison(c)
					}
				}
			}
		case *plans.RecordQueryIndexPlan:
			for _, cr := range t.GetScanComparisons() {
				if cr.IsEquality() {
					collectComparison(cr.GetEqualityComparison())
				} else if cr.IsInequality() {
					for _, c := range cr.GetInequalityComparisons() {
						collectComparison(c)
					}
				}
			}
		}
		for _, ch := range p.GetChildren() {
			walk(ch)
		}
	}
	walk(plan)
}

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
	for i, f := range b.RC.Fields {
		v, err := f.Value.Evaluate(rowCtx)
		if err != nil {
			return nil, err
		}
		m[f.Name] = v
		if !b.WindowsOK {
			continue
		}
		// Qualified key from the SPAN covering this slot — not the field's
		// child QOV: for a translated top's FUSED field the child is the MERGE
		// quantifier (q$N, never a user-visible alias), while the span carries
		// the resolved LEAF leg alias. For the pristine seed span alias ==
		// child QOV correlation, so the output is unchanged.
		for _, s := range b.DatumSpans {
			if i >= s.Offset && i < s.Offset+s.Width {
				m[strings.ToUpper(s.Alias.Name())+"."+strings.ToUpper(s.LegType.Fields[i-s.Offset].Name)] = v
				break
			}
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
func (b *ordinalJoinBirth) legRows(outerAlias, innerAlias string, outer, inner *QueryResult) (map[values.CorrelationIdentifier]values.OrdinalRow, map[values.CorrelationIdentifier]any, error) {
	legs := make(map[values.CorrelationIdentifier]values.OrdinalRow, 2)
	raw := make(map[values.CorrelationIdentifier]any)
	if err := b.bindLeg(legs, raw, outerAlias, outer); err != nil {
		return nil, nil, err
	}
	if err := b.bindLeg(legs, raw, innerAlias, inner); err != nil {
		return nil, nil, err
	}
	return legs, raw, nil
}

func (b *ordinalJoinBirth) bindLeg(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, alias string, qr *QueryResult) error {
	id := values.NamedCorrelationIdentifier(alias)
	// A RAW leg (a bare-QOV non-record W4c unnest element) binds its whole flowed
	// Datum — never adapted to a (non-record → empty) OrdinalRow. A nil pointer is
	// the deliberately-NULL leg: raw nil, which QOV(inner).Evaluate flows as NULL.
	if _, isRaw := b.RawLegs[id]; isRaw {
		if qr == nil {
			raw[id] = nil
		} else {
			raw[id] = qr.Datum
		}
		return nil
	}
	// A WITH-ORDINALITY Explode leg binds STRICTLY POSITIONALLY: its Datum is
	// keyed by the internal OrdinalFieldName positions (`_0`=element,
	// `_1`=ordinal), so slot i = Datum[_i] — the leg type's AS/AT alias NAMES
	// never participate in the key lookup (a user may spell an alias `_0`/`_1`).
	// See OrdinalityLegs (producer context, set by newFlatMapCursor).
	if _, isOrd := b.OrdinalityLegs[id]; isOrd {
		if qr == nil {
			legs[id] = nil
			return nil
		}
		lt := b.legType(id)
		row := NewPositionalRow(lt)
		if lt != nil {
			if m, isMap := qr.Datum.(map[string]any); isMap {
				for i := range lt.Fields {
					row.Slots[i] = m[values.OrdinalFieldName(i)]
				}
			}
		}
		legs[id] = row
		return nil
	}
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
func (b *ordinalJoinBirth) evaluateLegs(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, base values.CorrelationBinder) (*PositionalRow, error) {
	return evaluateOrdinalJoinRow(b.RC, b.OutputType, &birthLegBinder{legs: legs, raw: raw, base: base})
}

// evaluateBound births the positional row from any pre-built leg binder — the
// zero-rebuild path the NLJ cursor's per-pair twoLegBinder uses (review
// W3a-2: no per-pair map, no per-pair re-adaptation).
func (b *ordinalJoinBirth) evaluateBound(bindings values.CorrelationBinder) (*PositionalRow, error) {
	return evaluateOrdinalJoinRow(b.RC, b.OutputType, bindings)
}

// twoLegBinder is the NLJ cursor's per-pair leg binder: exactly the join's
// two legs, pre-adapted rows plugged in per candidate pair — no map, no
// re-adaptation (the inner rows are a FIXED slice adapted once at cursor
// construction; the outer is adapted once per outer-row advance; review
// W3a-2 structural-perf catch). A nil row IS the deliberately-NULL leg
// (LEFT/FULL padding): (nil, true), contract ruling #3. Non-leg correlations
// delegate to base.
//
// No RAW-leg arm (unlike birthLegBinder): a raw bare-QOV-over-non-record leg is
// the W4c lateral-unnest element, which is ALWAYS a FlatMap seed — the NLJ path
// never carries one, so twoLegBinder's OrdinalRow-only legs are complete for it.
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
	legs, raw, err := b.legRows(outerAlias, innerAlias, outer, inner)
	if err != nil {
		return nil, err
	}
	return b.evaluateLegs(legs, raw, base)
}

// birthLegBinder is the BIRTH-time correlation binder: DIRECT per-leg bindings
// (review W3 ruling: predicates and result-value evaluation need no windows at
// birth — each leg binds to its OWN leg-local row, so both baked (leg ordinal)
// and lazy (leg-relative resolveOrdinal) references read the right slot, even
// for the second leg). A key PRESENT with a nil value is the deliberately-NULL
// leg: GetCorrelationBinding returns (nil, true) and the baked node's
// `return bound, nil` arm yields NULL (contract ruling #3). Anything else
// delegates to base (outer correlations; nil base = unbound).
//
// The map-based binder survives ONLY for flatMap's one-shot evaluate (one
// binder per EMITTED row — no per-candidate cost there); the NLJ's per-pair
// hot path uses the fixed twoLegBinder below (review W3a-2 perf catch).
type birthLegBinder struct {
	legs map[values.CorrelationIdentifier]values.OrdinalRow
	// raw carries bare-QOV non-record legs (the W4c unnest element) bound to
	// their WHOLE flowed Datum — QOV(inner).Evaluate returns the scalar/struct
	// itself, not a positional row. A key present with a nil value is that leg's
	// NULL binding.
	raw  map[values.CorrelationIdentifier]any
	base values.CorrelationBinder
}

// GetCorrelationBinding implements values.CorrelationBinder.
func (b *birthLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if v, present := b.raw[id]; present {
		return v, true // raw leg — the whole flowed Datum (nil = NULL leg)
	}
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

// mergeShapeDatum derives the S3 positional-merge row's coexistence Datum:
// slot i carries leg i's own DATUM — the shape evaluating the bare-QOV merge
// RC over name bindings produces (the §5 oracle side), and the shape the
// partition rule's rebased upper references read through (`_i` root keys).
// Never mergeRows' flat bare/qualified keys: no consumer addresses a merge
// quantifier's row by flat names — the rule rebases every upper reference
// through the merge quantifier, so a flat Datum silently NULLs them all on
// the oracle side (0-row joins). A nil leg Datum (the NULL leg) mirrors the
// name model's reconstructed empty inner row.
func mergeShapeDatum(rc *values.RecordConstructorValue, outerID, innerID values.CorrelationIdentifier, outerDatum, innerDatum any) map[string]any {
	m := make(map[string]any, len(rc.Fields))
	for _, f := range rc.Fields {
		qov, isQOV := f.Value.(*values.QuantifiedObjectValue)
		if !isQOV {
			continue
		}
		var d any
		switch qov.Correlation {
		case outerID:
			d = outerDatum
		case innerID:
			d = innerDatum
		default:
			continue
		}
		if d == nil {
			d = map[string]any{}
		}
		m[f.Name] = d
	}
	return m
}

// downstreamLegWindows computes, ONCE at operator construction, whether a
// consumer's input plan flows the ordinal join's MERGED positional row
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
	join := unwrapToJoinPlan(input)
	if join == nil {
		return nil, false
	}
	return joinPlanSpans(join)
}

// unwrapToJoinPlan walks the EXACT single-child passthrough set down to the
// join plan (FlatMap/NLJ); nil when the chain ends anywhere else. The
// passthrough set is downstreamLegWindows' (see its doc for what is
// deliberately left out and why a miss fails loud, never silently wrong).
func unwrapToJoinPlan(input plans.RecordQueryPlan) plans.RecordQueryPlan {
	for input != nil {
		switch p := input.(type) {
		case *plans.RecordQueryNestedLoopJoinPlan:
			return p
		case *plans.RecordQueryFlatMapPlan:
			return p
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
			return nil
		}
	}
	return nil
}

// innerIsOrdinalityExplode reports whether a FlatMap's inner plan is a
// WITH-ORDINALITY Explode (through the single-child passthrough wrappers a
// WHERE-on-ordinal / LIMIT can add) — the RFC-142 W4c unnest producer signal.
// Such an inner flows a per-row Datum keyed by the internal `_0`/`_1` positions,
// so its leg must bind POSITIONALLY (OrdinalityLegs). Only an ordinality Explode
// qualifies: a non-ordinality Explode (an IN-list) flows a bare scalar (a
// RawLeg, a different path), and any other inner is name-model.
func innerIsOrdinalityExplode(input plans.RecordQueryPlan) bool {
	for input != nil {
		switch p := input.(type) {
		case *plans.RecordQueryExplodePlan:
			return p.IsWithOrdinality()
		case *plans.RecordQueryPredicatesFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryLimitPlan:
			input = p.GetInner()
		default:
			return false
		}
	}
	return false
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
// (review condition 2): dies when uppers bake, S3/S4.
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
