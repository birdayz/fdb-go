package executor

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file is the ordinal-join executor machinery: the primitives the
// gated join's merged positional row is built from — the translator seeds
// gated join clusters (N-way flat) with the ordinal RC these consume, and
// merge levels build positional rows. Design invariants: spans derive from
// the RC (the RC is the single authority — never independent bookkeeping);
// leg windows resolve quantifier-addressed leg references over the merged
// row (Java's quantifier binding; needed because Go's physical join output
// is one flat concat); the window implements values.OrdinalRow completely
// so no new eval arm exists; the wrong-slot hazard is pinned by tests.

// legSpan is one leg's slot range within the ordinal join's merged
// positional row: the leg quantifier's alias, the leg's own RecordType, and
// the half-open window [Offset, Offset+Width) its columns occupy in the merged
// row. Spans are DERIVED from the ordinal join RC by ordinalJoinSpans at
// cursor construction (the RC is the single authority)
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
// single run (an any-baked⟹well-formed-or-panic
// boundary would false-positive on exactly those plans). Downstream consumers use
// this probe to decide whether LEG WINDOWS apply to the join's output row —
// windows are only meaningful when the output IS the leg concatenation; a
// folded projection's output is a plain frontier row and gets none.
//
// Loud seed validation lives where the shape IS guaranteed by construction:
// assertOrdinalJoinSeed, called by the translator at the seed (and by its
// pins). Cursor-side ORDINAL-BUILD detection (does this join evaluate its
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
	// The MIXED single-source lateral-unnest seed (baked outer run
	// + a bare-scalar element) is not a pristine all-baked seed, so
	// ordinalJoinSpansOf declines it; derive its windows (outer leg + a
	// synthesized 1-field element leg) so the shadowing element gets its own
	// namespace. Decline-only for every other shape.
	return unnestMixedSeedSpans(v)
}

// unnestMixedSeedSpans derives leg windows for the MIXED single-
// source lateral-unnest ordinal seed (built by the planner for a lateral
// UNNEST without an ORDINALITY clause): a full baked
// OUTER leg run followed by EXACTLY ONE trailing bare-QuantifiedObjectValue
// element field over a NON-record type — Java's isPrimitive() whole-object
// element (a scalar bound DIRECTLY, never an ofOrdinal baked leg; it binds RAW
// at build, see ordinalJoinBuild.RawLegs). ordinalJoinSpansOf DECLINES this
// shape (the element is a bare QOV, not a FrontierPinned FieldValue), so without
// windows the FlatMap output resolves references by positional FIRST-match —
// which mis-resolves a name SHARED by the element AS alias and an outer column
// (`FROM t, t.arr AS x` where t also has a column x) to the OUTER duplicate, and
// leaves a qualified outer reference (`t.id`) without its leg namespace in a
// wrapping join's merged row. This synthesizes the element's OWN 1-field leg
// window (Alias = the element QOV correlation = the unnest AS alias; the leg's
// single column = the RC field's name) so the element and the outer each carry
// their own ALIAS.COL namespace — mirroring the WITH-ORDINALITY inner leg (a
// genuine 2-field record leg, which ordinalJoinSpansOf already windows) and
// RFC-142's AS-alias shadowing rule. DECLINE-only
// (ok=false) for every other shape: the trailing-bare-QOV-over-non-record
// discriminator never matches a folded projection, a baked+constant fold, or the
// positional-merge RC (whose bare QOVs are over RECORD leg types).
func unnestMixedSeedSpans(v values.Value) (spans []legSpan, mergedType *values.RecordType, ok bool) {
	rc, isRC := v.(*values.RecordConstructorValue)
	if !isRC || len(rc.Fields) < 2 {
		return nil, nil, false
	}
	n := len(rc.Fields)
	// ELEMENT-ANYWHERE: the mixed scalar element (a bare QOV over a NON-record type —
	// the RawLeg whole-object element, Java's primitive branch) can appear at ANY FROM
	// position (`FROM A, B, A.arr AS x` trailing, `FROM A, A.arr AS x, B` mid-list
	// splitting the leg run, `FROM A.arr AS x, B` leading). One OR MORE full baked leg
	// runs surround it: each run consecutive same-alias, ordinal 0..width-1, fully
	// covered; a new alias starts a new run; a DUPLICATE alias (a run split across the
	// element) declines. The non-record guard on the element is LOAD-BEARING against
	// the positional-merge RC (a bare QOV over a RECORD leg type — it must NOT be
	// treated as the element). Cross-agreement twin of OrdinalSeedLegWindows'
	// element-anywhere walk — bit-for-bit, pinned by
	// TestMixedSeedSpanLayoutCrossAgreement.
	mergedFields := make([]values.Field, n)
	var spansList []legSpan
	seen := map[string]struct{}{}
	curIdx := -1
	hasElement := false
	coverageOK := func() bool {
		if curIdx < 0 {
			return true
		}
		s := &spansList[curIdx]
		return s.LegType != nil && s.Width == len(s.LegType.Fields)
	}
	for i := 0; i < n; i++ {
		f := rc.Fields[i]
		if elemQOV, isQOV := f.Value.(*values.QuantifiedObjectValue); isQOV {
			if _, isRecord := elemQOV.Type().(*values.RecordType); !isRecord {
				if !coverageOK() {
					return nil, nil, false // an unfinished leg run before the element
				}
				if _, dup := seen[elemQOV.Correlation.String()]; dup {
					return nil, nil, false
				}
				seen[elemQOV.Correlation.String()] = struct{}{}
				elemName := strings.ToUpper(f.Name)
				elemLegType := &values.RecordType{Fields: []values.Field{{Name: elemName, FieldType: elemQOV.Type(), Ordinal: 0}}}
				mergedFields[i] = values.Field{Name: elemName, FieldType: elemQOV.Type(), Ordinal: i}
				spansList = append(spansList, legSpan{Alias: elemQOV.Correlation, LegType: elemLegType, Offset: i, Width: 1})
				curIdx = -1
				hasElement = true
				continue
			}
		}
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return nil, nil, false
		}
		alias, legType, ord, resolved := resolveSpanLeaf(fv, f.Name, nil)
		if !resolved {
			return nil, nil, false
		}
		if curIdx < 0 || alias != spansList[curIdx].Alias {
			if ord != 0 || !coverageOK() {
				return nil, nil, false
			}
			if _, dup := seen[alias.String()]; dup {
				return nil, nil, false // a split run — not pristine
			}
			seen[alias.String()] = struct{}{}
			spansList = append(spansList, legSpan{Alias: alias, LegType: legType, Offset: i, Width: 1})
			curIdx = len(spansList) - 1
		} else {
			if ord != spansList[curIdx].Width {
				return nil, nil, false // a gap within the run
			}
			spansList[curIdx].Width++
		}
		mergedFields[i] = values.Field{Name: f.Name, FieldType: fv.Type(), Ordinal: i}
	}
	if !coverageOK() || !hasElement || len(spansList) < 2 {
		return nil, nil, false // partial last leg, no element (a pristine seed), or a lone span
	}
	return spansList, &values.RecordType{Fields: mergedFields, Legs: mergedLegsOfSpans(spansList)}, true
}

// ordinalJoinSpansOf is ordinalJoinSpans with FUSED-reference resolution (the
// translated-top shape after a positional merge): a multi-accessor pinned path rooted at a
// MERGE quantifier ([( _i, i), (col, j)] — the partition rule's TranslationMap
// output) resolves to its LEAF leg by descending legRVs — the merge
// quantifier's child result value (the positional-merge RC mapping _i →
// QOV(leg)), the ONLY place the merged-away legs' user aliases survive. With
// legRVs nil, multi-accessor paths decline and this is exactly the
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
	// A leg alias never legitimately RECURS: a seed is a concat of DISTINCT
	// contiguous legs. Rejecting a re-appearing alias (a split run) makes this
	// run-LIST walk accept-equivalent to values.OrdinalSeedLegWindows' run-MAP,
	// whose dup-alias check declines the same shape — closing the last (unreachable,
	// but real) cross-agreement drift surface: a 1-field-leg `[A,B,A]` would
	// otherwise be accepted here (each 1-field run trivially full-coverage) while
	// values declines it. Pinned as a both-decline case in the cross-agreement fixture.
	seen := make(map[values.CorrelationIdentifier]struct{}, len(rc.Fields))
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			// Not every field a PINNED baked leg ref — not the seed. Unpinned
			// baked nodes (the recursive-CTE wrap) carry no join-frontier
			// contract; the probe keys on the FrontierPinned bit, not bare
			// bakedness.
			return nil, nil, false
		}
		alias, legType, ord, resolved := resolveSpanLeaf(fv, f.Name, legRVs)
		if !resolved {
			return nil, nil, false
		}
		if len(spans) == 0 || spans[len(spans)-1].Alias != alias {
			if ord != 0 {
				return nil, nil, false // run must start at leg ordinal 0
			}
			if _, dup := seen[alias]; dup {
				return nil, nil, false // a split run (leg recurs) — not the concat
			}
			seen[alias] = struct{}{}
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
		return nil, nil, false // 1 run = folded single leg, not a concat (N-leg flat seeds are live)
	}
	total := 0
	for _, s := range spans {
		if s.Width != len(s.LegType.Fields) {
			return nil, nil, false // partial leg coverage — a folded projection
		}
		total += s.Width
	}
	// Spans-consistency assert (defensive pin). Unreachable given the
	// run construction above — pinned anyway so a future edit that breaks the
	// derivation dies here, not in a downstream window misread.
	if total != len(rc.Fields) {
		panic(fmt.Sprintf("ordinal join spans inconsistent: sum(widths)=%d, RC has %d fields — spans must derive exactly from the RC", total, len(rc.Fields)))
	}
	return spans, &values.RecordType{Fields: mergedFields, Legs: mergedLegsOfSpans(spans)}, true
}

// mergedLegsOfSpans builds the merged type's leg boundaries from the derived spans
// — each plain run 1:1, each clustered BOX run its BURIED subs only (the run name
// is its rightmost leaf's; a run-level entry would shadow that leaf with the whole
// concat; see rcOutputType). So a buried-leg reference ("B.BID") binds its OWN
// window through the row's Legs metadata (rowLegsBinder / legWindowBinder /
// rowSlotForLegColumn). Mirrors the values-side finalizeSeedWindows — the
// cross-agreement invariant (independent
// walks drift, and layout drift is wrong-offset wrong-rows). Shared by the pristine
// and mixed span derivations so a box outer's boundaries agree in both.
//
// NOTE (mixed seed): this includes the trailing ELEMENT leg, whereas rcOutputType
// (the build ROW type) omits the bare-QOV element. The two "merged type" notions
// disagree on the element leg — harmless today (nothing compares them; every
// consumer reads only .Fields, and the leg-window binders resolve over the
// rcOutputType-built row, not this span-probe type). If a future consumer ever
// treats this .Legs as authoritative for an rcOutputType-built row, reconcile.
func mergedLegsOfSpans(spans []legSpan) []values.RecordTypeLeg {
	var mergedLegs []values.RecordTypeLeg
	for _, s := range spans {
		if len(s.LegType.Legs) > 0 {
			for _, sub := range s.LegType.Legs {
				mergedLegs = append(mergedLegs, values.RecordTypeLeg{Name: sub.Name, Start: s.Offset + sub.Start, Width: sub.Width})
			}
			continue
		}
		mergedLegs = append(mergedLegs, values.RecordTypeLeg{Name: strings.ToUpper(s.Alias.Name()), Start: s.Offset, Width: s.Width})
	}
	return mergedLegs
}

// resolveSpanLeaf resolves one RV field to its LEAF leg (alias, leg type,
// leg-local ordinal). A single-accessor path is the plain seed shape: the leg is
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
func resolveSpanLeaf(fv *values.FieldValue, fieldName string, legRVs map[values.CorrelationIdentifier]values.Value) (alias values.CorrelationIdentifier, legType *values.RecordType, legOrd int, ok bool) {
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
	// A TERMINAL slot that is a bare NON-RECORD QOV of a
	// quantifier we hold a legRV for is the gathered unnest's MIXED element
	// carried through a partition collapse — unnestMixedSeedSpans' trailing-
	// element synthesis lifted one level (the merge translated the seed's
	// direct-QOV element into this single-accessor pinned ref). Synthesize the
	// element's 1-field leg: alias = the SLOT QOV's correlation (the AS alias,
	// never the merge alias), the sole column named from the enclosing RC
	// field (fieldName — the same naming authority as the top-level
	// synthesis). The discriminator is the SLOT SHAPE: the non-record guard is
	// load-bearing against whole-leg record slots (the pristine positional-
	// merge RC), which must keep resolving as merge-leg runs.
	term := accs[0].Ordinal
	if rc, isRC := legRVs[alias].(*values.RecordConstructorValue); isRC && term >= 0 && term < len(rc.Fields) {
		if slotQOV, isQ := rc.Fields[term].Value.(*values.QuantifiedObjectValue); isQ {
			if _, isRecord := slotQOV.Type().(*values.RecordType); !isRecord {
				elemName := strings.ToUpper(fieldName)
				return slotQOV.Correlation, &values.RecordType{Fields: []values.Field{
					{Name: elemName, FieldType: slotQOV.Type(), Ordinal: 0},
				}}, 0, true
			}
		}
	}
	return alias, legType, term, true
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
// (a FULL box's gated-join leg, per ordinalLegColumns' concat)
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
// pristine seed needs no legRVs and no splice — identical output).
func joinPlanSpans(join plans.RecordQueryPlan) ([]legSpan, bool) {
	spans, _, ok := joinPlanSpansTyped(join)
	return spans, ok
}

// joinPlanSpansTyped is joinPlanSpans that ALSO returns the merged row's
// RecordType (which ordinalJoinSpansOf already computes and joinPlanSpans
// discards). The identity-FlatMap positional pass-through
// adapts a propagated outer against this merged type (a no-op on a seed-layout
// row, but LOUD on a mismatch — never a silent wrong-slot read).
func joinPlanSpansTyped(join plans.RecordQueryPlan) ([]legSpan, *values.RecordType, bool) {
	rv := joinPlanRV(join)
	if rv == nil {
		return nil, nil, false
	}
	legRVs := make(map[values.CorrelationIdentifier]values.Value)
	collectJoinLegRVs(join, legRVs)
	spans, mergedType, ok := ordinalJoinSpansOf(rv, legRVs)
	if !ok {
		// The MIXED single-source lateral-unnest seed windows via
		// the dedicated derivation (bare-scalar element leg). It has no fused/box
		// legs, so the spans stand without a splice.
		if ms, mt, mok := unnestMixedSeedSpans(rv); mok {
			return ms, mt, true
		}
		return nil, nil, false
	}
	return spliceLegSpans(spans, legRVs), mergedType, true
}

// assertOrdinalJoinSeed delegates to values.AssertOrdinalJoinSeed — the LOUD
// seed-shape validator, which lives in the values package so
// the translator, the caller of record, can invoke it at the seed without an
// executor import. Semantics unchanged; the executor-side pins keep
// exercising it through this name.
func assertOrdinalJoinSeed(rc *values.RecordConstructorValue) {
	values.AssertOrdinalJoinSeed(rc)
}

// legWindowRow is a leg-relative view over the join's merged positional row:
// leg ordinal i reads merged slot Offset+i. A correlated baked reference
// (FieldValue(QOV(leg), col) carrying a LEG-LOCAL ordinal) bound to a leg of a
// merged/concatenated row reads its source's own slot through this window —
// Java's rewire-by-ordinal semantics for the two-level NLJ→FlatMap lowering.
//
// It implements values.OrdinalRow (Get leg-relative), so it slots into the
// existing evaluateCorrelated binder arm with no new eval arm, and a miss
// stays loud (the (nil,false) return becomes an OrdinalResolutionError in
// evaluateOrdinal). TypeNames feeds that error's diagnostics via the
// values.ordinalRowNames optional interface, reporting the LEG's columns
// (what this window actually exposes).
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

// TypeNames returns the LEG type's column names — the diagnostics the
// values.OrdinalResolutionError enrichment (values.ordinalRowNames) reads via
// optional-interface assertion.
func (w *legWindowRow) TypeNames() []string {
	if w.legType == nil {
		return nil
	}
	return typeFieldNames(w.legType)
}

// legWindowBinder is the correlation binder for uppers over the ordinal join:
// a reference to a leg alias is bound to that leg's window over the merged
// row, anything else delegates to base. This is the runtime half of Java's
// rewire-by-ordinal semantics for Go's two-level NLJ→FlatMap lowering: a
// baked QOV(leg).col reference carries a LEG-LOCAL ordinal, and the binder
// supplies the LEG's own slice of the merged row for that ordinal to read.
type legWindowBinder struct {
	base  values.CorrelationBinder
	spans []legSpan
	row   values.OrdinalRow
}

// GetCorrelationBinding implements values.CorrelationBinder: a span alias gets
// its leg window over the merged row; any other alias delegates to base (nil
// base: unbound). Routing precedence (a source-relative baked QOV(leg).col
// carries a LEG-LOCAL ordinal, so the window must be the LEG's OWN slice):
//  1. a span alias match serves the BOX-LEAF sub-window when the span is a
//     clustered box named by its rightmost leaf (the whole-concat window would
//     misread a leg-local ordinal against an earlier buried leg's slots);
//  2. a BURIED leg inside any span serves its sub-window at the buried offset.
func (b *legWindowBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	alias := id.Name()
	for _, s := range b.spans {
		if !strings.EqualFold(s.Alias.Name(), alias) {
			continue
		}
		if s.LegType != nil {
			if w, ok := buriedLegWindow(b.row, s, alias); ok {
				return w, true
			}
		}
		return &legWindowRow{parent: b.row, legType: s.LegType, offset: s.Offset, width: s.Width}, true
	}
	// A BURIED leg inside a clustered box span — ordered AFTER the span
	// aliases (a top-level alias always wins its own name).
	for _, s := range b.spans {
		if s.LegType == nil {
			continue
		}
		if w, ok := buriedLegWindow(b.row, s, alias); ok {
			return w, true
		}
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// buriedLegWindow serves the sub-window of the buried leg named `alias` within
// a clustered box span's flat concat (RecordType.Legs bounds), or ok=false.
// Malformed bounds are a MISS (never the run-wide window — a whole-concat read
// would silently serve another leg's slots; the caller's miss disposition is
// loud downstream).
func buriedLegWindow(row values.OrdinalRow, s legSpan, alias string) (*legWindowRow, bool) {
	for _, bl := range s.LegType.Legs {
		if !strings.EqualFold(bl.Name, alias) {
			continue
		}
		end := bl.Start + bl.Width
		if bl.Start < 0 || end > len(s.LegType.Fields) {
			return nil, false
		}
		sub := &values.RecordType{Nullable: s.LegType.Nullable, Fields: s.LegType.Fields[bl.Start:end]}
		return &legWindowRow{parent: row, legType: sub, offset: s.Offset + bl.Start, width: bl.Width}, true
	}
	return nil, false
}

// adaptLegPositional is the sanctioned row-FORMAT adapter at the
// join-input boundary: it bridges a leg row whose positional LAYOUT differs
// from the leg type the join's ordinals were baked against (an INDEX-shaped
// covering row vs. the logical table-shaped leg type — see the permutation
// note in the body). A leg whose layout already matches flows through
// untouched; otherwise the row's slots are gathered into legType order via
// the row's own plan-produced RecordType (a missing column is a nil slot =
// SQL NULL); a row with no positional yields an all-nil row of the leg's
// width.
//
// LOUD when the gather matches ZERO of the leg's columns against a NON-EMPTY
// MERGE-shaped row: such a row carries dotted-qualified names ("A.ID") the
// bare leg-type names never match, so the silent path would all-NULL the leg
// — indistinguishable from a legitimate all-NULL row. The translation-time
// cluster-arity gate makes such legs unreachable for gated joins, so this
// error is a belt-and-braces tripwire on that invariant, not a supported path.
// A row whose columns are PRESENT with nil values (a genuine all-NULL row)
// matches normally and stays silent.
//
// This is format-only bridging — correlation-SEMANTIC bridging (an ordinal
// join consumed as a leg of a wider merge select) is explicitly out of
// scope and prevented upstream by the cluster-arity gate.
func adaptLegPositional(qr QueryResult, legType *values.RecordType) (values.OrdinalRow, error) {
	if qr.Positional != nil {
		// The passthrough requires ORDERED per-slot name agreement with the
		// leg type (a width-only check is not
		// enough): a COVERING-INDEX leg's positional row is INDEX-shaped —
		// buildCoveringRow types it value-columns-then-PK ([V, ID]), not
		// table order ([ID, V]) — same width, different layout, and a baked
		// leg ordinal would silently read the wrong slot. A non-aligned
		// positional row is a LEGITIMATE plan shape (not a gate breach): fall
		// through to the per-layout gather below (rowSlotForLegColumn binds each leg
		// column against the row's own plan-produced type), with the zero-match tripwire as
		// the final guard.
		if legType == nil || positionalMatchesLegType(qr.Positional, legType) {
			return qr.Positional, nil
		}
	}
	row := NewPositionalRow(legType)
	if legType == nil {
		return row, nil
	}
	// Layout PERMUTATION at the leg boundary: gather the row's slots into
	// legType order via the row's own plan-produced RecordType (FieldIndex,
	// first-match — the same rule every plan-time bake uses). A documented
	// residual of Go's two-layout seed: gated-join legs are seeded with the
	// LOGICAL (table-shaped) leg type while a physical leg may emit an
	// INDEX-shaped row (buildCoveringRow types value-columns-then-PK), so the
	// two layouts are permutations of each other. Java has no such adapter —
	// its planner rebinds every FieldValue ordinal against the physical
	// quantifier's actual flowed type (translateCorrelations), so the baked
	// ordinal IS the physical slot; retiring this gather requires Go's seed to
	// bake against the chosen physical leg layout the same way. Until then the
	// bind is per-layout TYPE metadata, never a per-value name probe, and a
	// zero-match merge-shaped row is loud below.
	if qr.Positional != nil {
		matched := 0
		if qr.Positional.Type != nil {
			for i, f := range legType.Fields {
				if idx, ok := rowSlotForLegColumn(qr.Positional.Type, f.Name); ok {
					v, _ := qr.Positional.Get(idx)
					row.Slots[i] = v
					matched++
				}
			}
		}
		if matched == 0 && len(qr.Positional.Slots) > 0 && len(legType.Fields) > 0 {
			switch {
			case rowIsMergeShaped(qr.Positional):
				// LOUD for a genuinely MERGE-SHAPED row (leg windows or dotted-
				// qualified field names) that matched nothing:
				// a merge leg wrongly consumed by a gated join.
				return nil, fmt.Errorf("leg adapter: merge-shaped leg row carries NONE of the leg type's %d columns %v (row width: %d) — a gated join must not consume a merge-shaped leg (cluster-arity gate breach or leg-type mismatch)", len(legType.Fields), typeFieldNames(legType), len(qr.Positional.Slots))
			case len(qr.Positional.Slots) == len(legType.Fields):
				// A SIMPLE row whose column names are DISJOINT from the leg type
				// but SAME WIDTH — e.g. a materialized computed-scalar leg named by
				// its expression `(AMOUNT + 0)` consumed as the anonymous `_0`
				// positional leg. ofOrdinal reads by SLOT, so position is the
				// authority: map slot-for-slot.
				copy(row.Slots, qr.Positional.Slots)
			default:
				// A simple row of a different width that matches nothing (e.g. a
				// bare-scalar `_0` UNNEST element bound as a redundant gather leg)
				// degrades to an all-nil leg silently.
			}
		}
	}
	return row, nil
}

// rowSlotForLegColumn binds one leg-type column name to a slot of the source
// row's plan-produced RecordType: the flat FieldIndex first, then — for a
// DOTTED name over a row whose type carries leg boundaries (RecordType.Legs,
// the merged concat / clustered box layout) — the per-leg window: qualifier →
// the leg's window, column → FieldIndex WITHIN it. The dotted arm serves the
// correlated-scalar seed legs, whose seed leg types name columns literally
// `LEG.COL` while the physical leg emits the merged row those names address.
// First-match on duplicate leg names mirrors FieldIndex's own first-match rule.
func rowSlotForLegColumn(rt *values.RecordType, name string) (int, bool) {
	if i, ok := rt.FieldIndex(name); ok {
		return i, true
	}
	di := strings.IndexByte(name, '.')
	if di <= 0 || len(rt.Legs) == 0 {
		return 0, false
	}
	qual, col := name[:di], name[di+1:]
	for _, leg := range rt.Legs {
		if !strings.EqualFold(leg.Name, qual) {
			continue
		}
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(rt.Fields) {
			break
		}
		for k := leg.Start; k < end; k++ {
			if strings.EqualFold(rt.Fields[k].Name, col) {
				return k, true
			}
		}
		break
	}
	return 0, false
}

// rowIsMergeShaped reports whether a positional row is a JOIN-MERGE output — it
// carries leg windows (Type.Legs) or dotted-qualified field names. Such a row's
// bare columns are last-leg-wins leftovers, so a gated join adapting it to a
// bare leg type that matches nothing is a wrong-source hazard (loud). A
// simple row (a scan/element leg) that matches nothing is a benign redundant
// binding that degrades to an all-nil leg.
func rowIsMergeShaped(pos *PositionalRow) bool {
	if pos == nil || pos.Type == nil {
		return false
	}
	if len(pos.Type.Legs) > 0 {
		return true
	}
	for _, f := range pos.Type.Fields {
		if strings.IndexByte(f.Name, '.') >= 0 {
			return true
		}
	}
	return false
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

// evaluateOrdinalJoinRow builds the join's merged positional row: each field
// of the ordinal join RC (a BAKED leg reference) is evaluated with the legs
// bound through bindings, writing merged slot i. A NULL LEG is expressed by
// the binder returning (nil, true) for that leg's correlation — the baked
// node's evaluateCorrelated `return bound, nil` arm yields nil, so the leg's
// slots come out NULL (appendNullLeg ≡ evaluating the
// merged RC with the leg QOV bound to nil; the null extension falls out, no
// ad-hoc per-row types).
func evaluateOrdinalJoinRow(rc *values.RecordConstructorValue, mergedType *values.RecordType, bindings values.CorrelationBinder) (*PositionalRow, error) {
	if len(rc.Fields) != len(mergedType.Fields) {
		panic(fmt.Sprintf("ordinal join row: RC has %d fields but merged type has %d — the merged type must derive from this RC (ordinalJoinSpans)", len(rc.Fields), len(mergedType.Fields)))
	}
	row := NewPositionalRow(mergedType)
	// A FOLDED result value (the B1 existential wrap) can carry an uncorrelated
	// ScalarSubqueryValue — a per-query constant the executor pre-evaluates and
	// binds by alias. Thread that map onto the build eval context so the scalar
	// resolves here (else ScalarSubqueryValue.Evaluate is loud: UnboundScalarSubquery).
	// The pristine ordinal-join SEED (baked leg refs only) carries none, so this is
	// nil for the NLJ path — harmless.
	evalCtx := &values.RowEvalContext{Correlations: bindings, ScalarSubqueries: scalarSubqueriesFromBinder(bindings)}
	for i, f := range rc.Fields {
		v, err := f.Value.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		row.Slots[i] = v
	}
	return row, nil
}

// --- cursor-side build wiring ------------------------------------------------

// rcOutputType derives an RC's OUTPUT row type: a RAW *RecordType (duplicate
// names allowed and preserved verbatim — positional access is by ordinal) with
// one field per RC field: the RC field's name, the field Value's flowed type,
// ordinal = position. For the pristine ordinal join SEED this equals
// ordinalJoinSpans' mergedType (each field is a baked leg reference and its
// Type() is the leg column's type); for a FOLDED result value (the pure-wrapper
// merge's projection RC — baked refs mixed with computed values/constants) it
// is the projection's output row type, which has no leg windows but is still
// the single authoritative type of the built positional row.
func rcOutputType(rc *values.RecordConstructorValue) *values.RecordType {
	fields := make([]values.Field, len(rc.Fields))
	for i, f := range rc.Fields {
		var ft values.Type = values.UnknownType
		if f.Value != nil {
			ft = f.Value.Type()
		}
		fields[i] = values.Field{Name: f.Name, FieldType: ft, Ordinal: i}
	}
	// The built row type carries leg boundaries — each
	// baked leg run plus every BURIED leg a clustered box leg's type records
	// (RecordType.Legs) — so a buried-leg reference ("B.BID") binds its OWN
	// window through the row's Legs metadata (rowLegsBinder) on rows that
	// build positional-only (the clustered null-supplying pad).
	var legs []values.RecordTypeLeg
	lastCorr := ""
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV {
			lastCorr = ""
			continue
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			lastCorr = ""
			continue
		}
		corr := strings.ToUpper(qov.Correlation.Name())
		if corr == lastCorr {
			continue
		}
		lastCorr = corr
		if rt, isRT := qov.Typ.(*values.RecordType); isRT {
			if len(rt.Legs) > 0 {
				// A clustered box run: its name is the rightmost LEAF's (the
				// sourceBinding convention), which the buried bounds already
				// carry at the leaf's own offset — a run-level entry under
				// that name would shadow it with the WHOLE concat (the
				// RIGHT-box collision: "D.ID" first-matched the box run and
				// read the null-supplying leg's slot). Subs only.
				for _, sub := range rt.Legs {
					legs = append(legs, values.RecordTypeLeg{Name: sub.Name, Start: i + sub.Start, Width: sub.Width})
				}
			} else {
				legs = append(legs, values.RecordTypeLeg{Name: corr, Start: i, Width: len(rt.Fields)})
			}
		}
	}
	return &values.RecordType{Fields: fields, Legs: legs}
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

// ordinalJoinBuild is the per-cursor ordinal-BUILD state, computed ONCE at
// cursor construction (detection is the structural
// ContainsBakedOrdinal probe on the plan's result value — emergent from the
// representation, never a per-plan flag). Enabled marks the cursor as an ordinal build site: its
// emitted rows carry the positional row evaluated from the RC with per-leg
// bindings — the row model's single authority.
type ordinalJoinBuild struct {
	// Enabled is true iff the result value contains a baked ordinal reference
	// (values.ContainsBakedOrdinal, deep). A nil/lazy result value yields a nil
	// *ordinalJoinBuild instead — use the nil-safe enabled().
	Enabled bool
	// RC is the result value as the RC the build evaluates per-field.
	RC *values.RecordConstructorValue
	// OutputType is the built positional row's single authoritative type
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
	// RawLegs are legs referenced by a BARE QuantifiedObjectValue over a
	// NON-record type — the RFC-142 lateral-unnest bare-scalar (or struct)
	// element leg, whose whole flowed value IS the column (Java's isPrimitive()
	// branch: the element is referenced directly, never ofOrdinal — ofOrdinal
	// over a scalar throws). Such a leg must bind its RAW value, never be adapted
	// to an OrdinalRow: adaptLegPositional would synthesize an EMPTY positional
	// row for a non-record value, so the element would build NULL — silently
	// wrong. Discriminated by leg SHAPE at construction, never a per-plan flag.
	// The record-leg OrdinalRow path (adaptLegPositional) is unchanged.
	RawLegs map[values.CorrelationIdentifier]struct{}
	// OrdinalityLegs are legs whose row is a WITH-ORDINALITY Explode row
	// (element at slot 0, 1-based ordinal at slot 1). Such a leg binds
	// STRICTLY POSITIONALLY (leg slot i = row slot i), never by the leg
	// type's AS/AT alias NAMES — a user may spell an alias `_0`/`_1`
	// (`FROM t, t.arr AS "_1" AT "_0"`), and a name lookup would route the
	// wrong internal key. Distinguished by PRODUCER CONTEXT (the FlatMap
	// knows its inner is an ordinality Explode — newFlatMapCursor sets
	// this), NOT the row SHAPE: a leg whose own columns are aliased
	// `_0`/`_1` is shape-identical but binds correctly by NAME
	// (adaptLegPositional).
	OrdinalityLegs map[values.CorrelationIdentifier]struct{}
}

// newOrdinalJoinBuild probes a join plan's result value at cursor
// construction. nil (disabled) when rv is nil or carries no baked ordinal —
// the non-build cursor path (identity pass-through / fold). A LOUD error when
// rv contains baked ordinals but is not a *RecordConstructorValue: every shape
// the planner can legitimately produce for an ordinal-build join is an RC
// (the seed, or the wrapper-merge-folded projection RC — the drift asserts
// pin that); anything else is a planner bug and must die at construction,
// never be silently demoted to a non-build cursor.
func newOrdinalJoinBuild(rv values.Value, preds []predicates.QueryPredicate) (*ordinalJoinBuild, error) {
	// Two build triggers:
	//   - a FrontierPinned baked reference anywhere in the RV (the flat
	//     seed, its folds, and the post-translation MIXED upper shape whose
	//     fields are ofOrdinal-over-innerMerge alongside bare leg QOVs);
	//   - the positional-merge RC (ALL fields bare `_i`-named QOVs —
	//     the lowest merge level carries no baked refs at all, but its rows
	//     must build positional: the level above reads them by ordinal).
	if rv == nil || (!values.ContainsBakedOrdinal(rv) && !values.IsPositionalMergeRC(rv)) {
		return nil, nil
	}
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, fmt.Errorf("ordinal join build: result value contains baked ordinal references but is a %T, want *RecordConstructorValue (seed or folded projection RC) — planner bug", rv)
	}
	spans, _, windowsOK := ordinalJoinSpans(rc)
	// LegTypes come from the RESULT VALUE *and* the join PREDICATES: a
	// folded projection RV can DROP a leg entirely while a
	// baked cross-leg ON predicate still references it — collecting from the
	// RV alone left the dropped leg typeless, and the leg
	// adapted to a ZERO-WIDTH binding that blew up the predicate
	// (loud OrdinalResolutionError, "row columns []") on a legitimate plan.
	legTypes := legTypesFromResultValue(rc)
	// Bare QOV fields carry their leg's type directly (the positional-merge
	// shape's `_i` columns and the mixed upper's untranslated leg): without
	// this a bare-QOV leg is typeless and its adapter degrades to an all-nil
	// synthesis even when the leg flows a typed row. Same
	// conflict-impossibility invariant as widenLegTypesFromPlan — every
	// source copies the one planner-constructed typed QOV — asserted the
	// same way (a silent first-wins would be an inconsistent assertion
	// of a load-bearing invariant).
	var rawLegs map[values.CorrelationIdentifier]struct{}
	for _, f := range rc.Fields {
		if qov, isQOV := f.Value.(*values.QuantifiedObjectValue); isQOV {
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := legTypes[qov.Correlation]; !seen {
					legTypes[qov.Correlation] = rt
				} else if len(prev.Fields) != len(rt.Fields) {
					panic(fmt.Sprintf("leg %s carries DIVERGENT types (%d vs %d fields) across the RV's bare-QOV and baked-reference sources — all references must copy the one planner-constructed typed QOV (planner bug)", qov.Correlation, len(prev.Fields), len(rt.Fields)))
				}
			} else {
				// A bare QOV over a NON-record type: the lateral-unnest
				// bare-scalar/struct element leg — bind its whole flowed value
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
	return &ordinalJoinBuild{
		Enabled:    true,
		RC:         rc,
		OutputType: rcOutputType(rc),
		Spans:      spans,
		WindowsOK:  windowsOK,
		LegTypes:   legTypes,
		RawLegs:    rawLegs,
	}, nil
}

// enabled is the nil-safe Enabled read — a non-build cursor stores a nil
// *ordinalJoinBuild.
func (b *ordinalJoinBuild) enabled() bool { return b != nil && b.Enabled }

// widenLegTypesFromPlan widens LegTypes with every BAKED leg reference found
// in a physical plan tree's predicate surfaces — PredicatesFilter/Filter
// predicates and scan/index comparison operands. The correlated implementation
// pushes the join's baked ON references INTO the inner plan as SARGs and
// residual filters, so a folded result value that DROPPED a leg leaves the
// build typeless for it even though the inner plan still references it — the
// untyped leg then adapts to a
// zero-width binding and dies loudly on a legitimate plan. Called by
// newFlatMapCursor with the inner plan; the NLJ path gets the same widening
// directly from its predicate list in newOrdinalJoinBuild.
//
// The walk exists only because a folded RV can drop a leg the plan still
// references. Its
// exact-set plan arms fail SAFE: a missed predicate surface leaves the leg
// typeless → loud zero-width death, never silent.
//
// Multiple sources (RV, join preds, pushed SARGs) can each carry a leg's
// type; this is CONFLICT-IMPOSSIBLE, not precedence — every baked reference
// is a copy of the ONE seed-constructed typed QOV, and every transformation
// preserves marker and type. The width-divergence panic below asserts that
// load-bearing invariant.
func (b *ordinalJoinBuild) widenLegTypesFromPlan(plan plans.RecordQueryPlan) {
	if !b.enabled() || plan == nil {
		return
	}
	walkBakedRefs(plan, func(v values.Value) values.Value {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return v
		}
		if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := b.LegTypes[qov.Correlation]; !seen {
					b.LegTypes[qov.Correlation] = rt
				} else if len(prev.Fields) != len(rt.Fields) {
					panic(fmt.Sprintf("leg %s carries DIVERGENT baked types (%d vs %d fields) across the RV/predicate/SARG sources — all baked references must copy the one seed-constructed typed QOV (planner bug)", qov.Correlation, len(prev.Fields), len(rt.Fields)))
				}
			}
		}
		return v
	})
}

// walkBakedRefs walks a physical plan tree's predicate surfaces —
// PredicatesFilter/Filter predicates and scan/index comparison operands —
// applying collect to every value found there. It is the ONE baked-reference
// plan walk (a single derivation path), shared by
// widenLegTypesFromPlan (the build-side type widening, width-divergence panic
// in its collector) and probeOuterBakedType (the
// disabled-build probe).
//
// RecordQueryNestedLoopJoinPlan also implements GetPredicates but is
// DELIBERATELY omitted: a baked-ref-bearing NLJ does not appear inside a
// gated FlatMap's inner plan (an existential inner is never itself consumed
// as a gated join leg).
// The exact-set plan arms fail SAFE: a missed predicate surface leaves a leg
// typeless / a probe negative — a loud zero-width death downstream, never
// silent misbinding.
func walkBakedRefs(plan plans.RecordQueryPlan, collect func(values.Value) values.Value) {
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, collect)
		}
	}
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

// probeOuterBakedType is the DISABLED-BUILD probe: a
// FlatMap whose own result value builds no ordinal state (the identity-RV
// existential FlatMap — WHERE-EXISTS over a gated join) can still carry BAKED
// FrontierPinned references to the OUTER alias inside its inner plan (the
// correlated implementation pushes the join's ON references down as SARGs and
// residual filters). Those references resolve by ordinal and die loudly on a
// name-keyed binding (BakedNameContextError), so the cursor must bind
// the outer positionally, adapted to the probed type's layout. The probe
// recovers the outer's typed RecordType
// from those references through the shared walker; the width-divergence panic
// asserts the same one-seed invariant widenLegTypesFromPlan pins.
func probeOuterBakedType(plan plans.RecordQueryPlan, outerAlias values.CorrelationIdentifier) *values.RecordType {
	var found *values.RecordType
	walkBakedRefs(plan, func(v values.Value) values.Value {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return v
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV || qov.Correlation != outerAlias {
			return v
		}
		if rt, isRT := qov.Type().(*values.RecordType); isRT {
			if found == nil {
				found = rt
			} else if len(found.Fields) != len(rt.Fields) {
				panic(fmt.Sprintf("outer %s carries DIVERGENT baked types (%d vs %d fields) across the inner plan's references — all baked references must copy the one seed-constructed typed QOV (planner bug)", outerAlias, len(found.Fields), len(rt.Fields)))
			}
		}
		return v
	})
	return found
}

// legType resolves the adapter's leg type for a leg alias: from the spans when
// the RV is the pristine seed (WindowsOK), else from the RV's baked references
// (LegTypes), else nil (no baked reference names this leg — the adapter then
// only passes a positional row through or yields a zero-width row).
func (b *ordinalJoinBuild) legType(id values.CorrelationIdentifier) *values.RecordType {
	if b.WindowsOK {
		for _, s := range b.Spans {
			if s.Alias == id {
				return s.LegType
			}
		}
	}
	return b.LegTypes[id]
}

// legRows adapts the two join legs into the BUILD-time binding map: alias →
// values.OrdinalRow via adaptLegPositional (layout-matching legs flow through;
// other layouts gather into the leg type). A NIL QueryResult pointer is the
// NULL leg (LEFT/FULL null padding): its alias maps to nil, PRESENT — the
// binder then returns (nil, true) and the leg's baked references evaluate to
// NULL (the null extension falls out of evaluation).
func (b *ordinalJoinBuild) legRows(outerAlias, innerAlias string, outer, inner *QueryResult) (map[values.CorrelationIdentifier]values.OrdinalRow, map[values.CorrelationIdentifier]any, error) {
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

func (b *ordinalJoinBuild) bindLeg(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, alias string, qr *QueryResult) error {
	id := values.NamedCorrelationIdentifier(alias)
	// A RAW leg (a bare-QOV non-record unnest element) binds its whole flowed
	// scalar — never adapted to a (non-record → empty) OrdinalRow. The element
	// flows as a 1-slot `_0` PositionalRow (scalarPositionalRow), so bind the
	// UNWRAPPED scalar, which QOV(inner).Evaluate flows as the value. A nil pointer
	// is the deliberately-NULL leg: raw nil (→ NULL).
	if _, isRaw := b.RawLegs[id]; isRaw {
		switch {
		case qr == nil || qr.Positional == nil:
			// A nil / positional-less inner is the deliberately-NULL leg (→ NULL);
			// bind an UNTYPED nil, never a typed-nil *PositionalRow.
			raw[id] = nil
		case isBareScalarRow(qr.Positional):
			raw[id] = qr.Positional.Slots[0]
		default:
			raw[id] = qr.Positional
		}
		return nil
	}
	// A WITH-ORDINALITY Explode leg binds STRICTLY POSITIONALLY: the producer
	// (explodeOrdinalityResult) emits the element at slot 0 and the 1-based
	// ordinal at slot 1 under the internal `[_0,_1]` schema, so slot i = row
	// slot i — the leg type's AS/AT alias NAMES never participate (a user may
	// spell an alias `_0`/`_1`). See OrdinalityLegs (producer context, set by
	// newFlatMapCursor).
	if _, isOrd := b.OrdinalityLegs[id]; isOrd {
		if qr == nil {
			legs[id] = nil
			return nil
		}
		lt := b.legType(id)
		row := NewPositionalRow(lt)
		if lt != nil && qr.Positional != nil {
			for i := range lt.Fields {
				if v, ok := qr.Positional.Get(i); ok {
					row.Slots[i] = v
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

// evaluateLegs builds the positional row from already-adapted leg bindings —
// the SINGLE eval path (evaluateOrdinalJoinRow) under a buildLegBinder. Split
// from evaluate so the NLJ cursor can share one legRows adaptation between
// the predicate context and the build.
func (b *ordinalJoinBuild) evaluateLegs(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, base values.CorrelationBinder) (*PositionalRow, error) {
	return evaluateOrdinalJoinRow(b.RC, b.OutputType, &buildLegBinder{legs: legs, raw: raw, base: base})
}

// evaluateBound builds the positional row from any pre-built leg binder — the
// zero-rebuild path the NLJ cursor's per-pair twoLegBinder uses (no per-pair
// map, no per-pair re-adaptation).
func (b *ordinalJoinBuild) evaluateBound(bindings values.CorrelationBinder) (*PositionalRow, error) {
	return evaluateOrdinalJoinRow(b.RC, b.OutputType, bindings)
}

// twoLegBinder is the NLJ cursor's per-pair leg binder: exactly the join's
// two legs, pre-adapted rows plugged in per candidate pair — no map, no
// re-adaptation (the inner rows are a FIXED slice adapted once at cursor
// construction; the outer is adapted once per outer-row advance — per-pair
// adapter work would be a structural perf regression). A nil row IS the
// deliberately-NULL leg (LEFT/FULL padding): (nil, true). Non-leg correlations
// delegate to base.
//
// No RAW-leg arm (unlike buildLegBinder): a raw bare-QOV-over-non-record leg is
// the lateral-unnest element, which is ALWAYS a FlatMap seed — the NLJ path
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

// evaluate is the one-shot build: adapt both legs (nil pointer = NULL leg),
// then evaluate the RC per-field into a PositionalRow under OutputType. base
// resolves outer correlations beyond the two legs (may be nil).
func (b *ordinalJoinBuild) evaluate(outerAlias, innerAlias string, outer, inner *QueryResult, base values.CorrelationBinder) (*PositionalRow, error) {
	legs, raw, err := b.legRows(outerAlias, innerAlias, outer, inner)
	if err != nil {
		return nil, err
	}
	return b.evaluateLegs(legs, raw, base)
}

// buildLegBinder is the BUILD-time correlation binder: DIRECT per-leg bindings
// (predicates and result-value evaluation need no windows at
// build — each leg binds to its OWN leg-local row, so both baked (leg ordinal)
// and lazy (leg-relative resolveOrdinal) references read the right slot, even
// for the second leg). A key PRESENT with a nil value is the deliberately-NULL
// leg: GetCorrelationBinding returns (nil, true) and the baked node's
// `return bound, nil` arm yields NULL. Anything else
// delegates to base (outer correlations; nil base = unbound).
//
// The map-based binder is used ONLY for flatMap's one-shot evaluate (one
// binder per EMITTED row — no per-candidate cost there); the NLJ's per-pair
// hot path uses the fixed twoLegBinder.
type buildLegBinder struct {
	legs map[values.CorrelationIdentifier]values.OrdinalRow
	// raw carries bare-QOV non-record legs (the unnest element) bound to
	// their WHOLE flowed value — QOV(inner).Evaluate returns the scalar/struct
	// itself, not a positional row. A key present with a nil value is that leg's
	// NULL binding.
	raw  map[values.CorrelationIdentifier]any
	base values.CorrelationBinder
}

// GetCorrelationBinding implements values.CorrelationBinder.
func (b *buildLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if v, present := b.raw[id]; present {
		return v, true // raw leg — the whole flowed value (nil = NULL leg)
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

// rowLegsBinder resolves a correlation to its LEG WINDOW derived from the
// row's OWN Type.Legs — the runtime half of the quantifier-addressed
// reference model: a SOURCE-RELATIVE baked reference
// (FieldValue{QOV(leg), col, Resolved: source ordinal}) evaluated over a
// merged/concatenated row binds its leg's window, where the source-relative
// ordinal reads the source's own slot (legWindowRow.Get = parent.Get(offset+i)
// — Java's quantifier binding, ordinal within the quantifier's row). The
// window authority is the row TYPE's leg boundaries (concatLegPositionals /
// ordinalLegType stamp them at row construction), NOT a plan-shape probe — so
// every merged row serves its legs uniformly, gated joins and leg-concat
// fallback merges alike. Aliases not in the row's legs delegate to base (an
// outer correlation); a miss with no base stays unbound (loud downstream).
type rowLegsBinder struct {
	base values.CorrelationBinder
	row  *PositionalRow
}

// GetCorrelationBinding implements values.CorrelationBinder. First-match on
// duplicate leg names mirrors RecordType.FieldIndex's first-match rule.
func (b *rowLegsBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if b.row != nil && b.row.Type != nil {
		name := id.Name()
		for _, leg := range b.row.Type.Legs {
			if !strings.EqualFold(leg.Name, name) {
				continue
			}
			end := leg.Start + leg.Width
			if leg.Start < 0 || end > len(b.row.Type.Fields) {
				// Malformed bounds: skip — a whole-row fallback would silently
				// misread another leg's slots; the unbound miss stays loud at
				// the evaluateCorrelated tail.
				break
			}
			sub := &values.RecordType{Nullable: b.row.Type.Nullable, Fields: b.row.Type.Fields[leg.Start:end]}
			return &legWindowRow{parent: b.row, legType: sub, offset: leg.Start, width: leg.Width}, true
		}
	}
	if b.base != nil {
		return b.base.GetCorrelationBinding(id)
	}
	return nil, false
}

// scalarSubqueriesFromBinder unwraps a build-time correlation binder to the
// pre-evaluated scalar-subquery map carried by the base *EvaluationContext, so a
// FOLDED build result value's ScalarSubqueryValue resolves at build (the B1
// existential wrap folds an uncorrelated scalar into the wrap RV). The build
// binders (buildLegBinder / twoLegBinder) chain their base down to the
// EvaluationContext; a bare EvaluationContext base returns its own map. Any other
// binder (a plan-time probe, no base) returns nil — the scalar then declines loudly
// at build, which is exactly the UnboundScalarSubquery contract for an
// unresolvable context.
func scalarSubqueriesFromBinder(b values.CorrelationBinder) map[values.CorrelationIdentifier]any {
	switch bb := b.(type) {
	case *EvaluationContext:
		return bb.scalarSubqueries
	case *buildLegBinder:
		return scalarSubqueriesFromBinder(bb.base)
	case *twoLegBinder:
		return scalarSubqueriesFromBinder(bb.base)
	}
	return nil
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
	spans, _, ok := downstreamLegWindowsTyped(input)
	return spans, ok
}

// downstreamLegWindowsTyped is downstreamLegWindows that ALSO returns the merged
// row's RecordType (the identity-FlatMap pass-through's
// loud adaptation guard). Same acceptance as downstreamLegWindows: a genuine
// gated ordinal join outer only (ordinalJoinSpansOf accepts the pristine
// FrontierPinned seed shape and declines everything else).
func downstreamLegWindowsTyped(input plans.RecordQueryPlan) ([]legSpan, *values.RecordType, bool) {
	join := unwrapToJoinPlan(input)
	if join == nil {
		return nil, nil, false
	}
	return joinPlanSpansTyped(join)
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
			// An identity-over-OUTER FlatMap (the WHERE-EXISTS pass-through,
			// RFC-141) is a row-preserving passthrough toward its OUTER plan:
			// the cursor re-emits the outer row verbatim (the identity
			// positional pass-through), so the window
			// authority is the outer's join — this FlatMap's own bare-QOV RV
			// yields no spans. Any other RV keeps the FlatMap as the terminal:
			// the seed RC is its own authority, everything else declines in
			// joinPlanSpans. An INNER-identity RV (FirstOrDefault-style
			// shapes) re-emits INNER rows and must NOT unwrap to the outer.
			if qov, ok := p.GetResultValue().(*values.QuantifiedObjectValue); ok && qov.Correlation == p.GetOuterAlias() {
				input = p.GetOuter()
				continue
			}
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
// WHERE-on-ordinal / LIMIT can add) — the RFC-142 unnest producer signal.
// Such an inner flows a per-row two-slot row keyed by the internal `_0`/`_1`
// positions, so its leg must bind POSITIONALLY (OrdinalityLegs). Only an
// ordinality Explode qualifies: a non-ordinality Explode (an IN-list) flows a
// bare scalar (a RawLeg, a different path), and any other inner binds through
// the normal leg adapter.
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
// but Correlations is the legWindowBinder — an upper's direct leg
// reference (FieldValue(QOV(leg), col), lazy or baked) resolves LEG-LOCALLY
// through its window, an outer correlation delegates to the base
// EvaluationContext, and only an unbound (join-quantifier) reference falls to
// the bare merged row. Applied UNCONDITIONALLY on windowed inputs — even with
// no param/subquery/outer binding in play — because the leg references NEED
// the Correlations bindings (the bare merged row misreads them leg-relative:
// a wrong-slot hazard).
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

// spanAwareRow is the merged positional row as the eval context's Positional:
// ordinal access passes through to the merged row untouched; MultiLeg exposes
// the leg-window structure to the values-side correlated fall-through guard
// (a source-relative baked ordinal must not read a multi-leg row bare — its
// ordinal addresses one leg's own window). Every column REFERENCE over the
// merged row resolves by plan-time-baked ordinal (a flat reference by its
// merged-row slot, a correlated leg reference through its leg window via the
// legWindowBinder Correlations) — there is no name-keyed read arm.
type spanAwareRow struct {
	parent values.OrdinalRow
	spans  []legSpan
}

func (r *spanAwareRow) Get(ord int) (any, bool) { return r.parent.Get(ord) }

// MultiLeg reports whether the merged row spans MORE than one leg window (or a
// single leg narrower than the row) — the values-side correlated fall-through
// guard's probe (see PositionalRow.MultiLeg).
func (r *spanAwareRow) MultiLeg() bool {
	if len(r.spans) == 0 {
		return false
	}
	if len(r.spans) == 1 && r.spans[0].Offset == 0 {
		if tn, ok := r.parent.(interface{ TypeNames() []string }); ok && r.spans[0].Width == len(tn.TypeNames()) {
			return false
		}
	}
	return true
}

// TypeNames feeds OrdinalResolutionError diagnostics (values.ordinalRowNames).
func (r *spanAwareRow) TypeNames() []string {
	if tn, ok := r.parent.(interface{ TypeNames() []string }); ok {
		return tn.TypeNames()
	}
	return nil
}
