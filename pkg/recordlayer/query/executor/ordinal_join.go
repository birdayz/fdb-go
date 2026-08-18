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
	Alias values.CorrelationIdentifier
	// Kind is the planner twin's values.LegKind, carried so the two walks can be
	// compared on it. A span whose kind disagrees with its window is a layout
	// disagreement exactly as an offset disagreement is — it says the two sides
	// read the same slot with different arithmetic.
	Kind    values.LegKind
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
// values.AssertOrdinalJoinSeed, called by the translator at the seed (and by its
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
		if carriesNestedLeg(spans) {
			return nil, nil, false
		}
		return spans, mergedType, true
	}
	if spans, mergedType, ok = unnestMixedSeedSpans(v); ok {
		if carriesNestedLeg(spans) {
			return nil, nil, false
		}
		return spans, mergedType, true
	}
	return nil, nil, false
}

// carriesNestedLeg is the NARROW entry's fail-closed decline, and the executor
// twin of values.OrdinalSeedLegWindows' own.
//
// The planner's narrow entry refuses a seed carrying a nested leg outright
// rather than returning it top-level-only, because a caller given top-level-only
// windows would silently be missing sub-windows it WOULD have had for a flat box
// leg and has no way to tell the two apart. The executor's narrow entry owes the
// identical refusal: the two walks must agree on the ACCEPT BOUNDARY, not merely
// on the layout they produce once both accept.
//
// It was missing, and the divergence was invisible until a §1(b) fixture drove
// it — measured: the planner's narrow walk DECLINED a seed whose leg run carried
// a nested boundary while the executor's narrow walk ACCEPTED it. Before the
// nested kind existed no producer could build such a seed, so the gap was
// unreachable rather than benign; activation is what armed it.
func carriesNestedLeg(spans []legSpan) bool {
	for _, s := range spans {
		if s.Kind == values.LegKindNested {
			return true
		}
		if s.LegType == nil {
			continue
		}
		for _, sub := range s.LegType.Legs {
			if sub.Kind == values.LegKindNested {
				return true
			}
		}
	}
	return false
}

// ordinalJoinSpansAcceptingNested is ordinalJoinSpans plus the NESTED leg kind —
// the executor twin of values.OrdinalSeedLegWindowsAcceptingNested, and the
// second half of the ONE-PREDICATE rule.
//
// The two walks are independent and a disagreement about which field is a leg
// and which is an element shifts the offset of every field after it, so the
// element test is one shared function (values.IsMixedSeedElementType) under a
// structural ban on discarded *RecordType assertions in these two files. The
// nested kind extends that SAME shared predicate: the recognizer is
// values.IsPositionalMergeRC — already this file's ordinal-build trigger — plus
// the per-slot BOUND record test, which the ban permits precisely because it
// binds. Neither side gets a private copy.
//
// It is a SEPARATE entry for the same reason the planner's is. ordinalJoinSpans
// drives windowsOK at the cursor, which is a nil/non-nil decision about whether
// leg windows apply to a join's output row at all; widening it in place would
// flip that decision for every positional-merge row in the corpus.
func ordinalJoinSpansAcceptingNested(v values.Value) (spans []legSpan, mergedType *values.RecordType, ok bool) {
	if rc, isRC := v.(*values.RecordConstructorValue); isRC && values.IsPositionalMergeRC(rc) {
		return positionalMergeSpans(rc)
	}
	if spans, mergedType, ok = ordinalJoinSpansOf(v, nil); ok {
		if !nestedSubLegsAreExpressible(spans) {
			return nil, nil, false
		}
		return spans, mergedType, true
	}
	// The MIXED single-source lateral-unnest seed (baked outer run
	// + a bare-scalar element) is not a pristine all-baked seed, so
	// ordinalJoinSpansOf declines it; derive its windows (outer leg + a
	// synthesized 1-field element leg) so the shadowing element gets its own
	// namespace. Decline-only for every other shape.
	if spans, mergedType, ok = unnestMixedSeedSpans(v); ok {
		if !nestedSubLegsAreExpressible(spans) {
			return nil, nil, false
		}
		return spans, mergedType, true
	}
	return nil, nil, false
}

// nestedSubLegsAreExpressible is the executor twin of the planner's refusal at
// finalizeSeedWindows' nested sub-window insertion.
//
// A span's leg table may declare a NESTED sub-leg — RFC-200 §1(b), the shape the
// three opt-in sites actually receive, where the merge appears as a leg's own
// ROW TYPE rather than as the whole result value. Two things must hold for
// either walk to address such a sub-leg, and the planner already refuses both:
//
//   - the slot it names must actually hold a RECORD. The leg table says a whole
//     row lives there; if the type disagrees, two producers disagree about a
//     slot's shape and neither side may guess which is right.
//   - that record must carry NO leg table of its own, because those boundaries
//     would be TWO steps from the merged row and neither walk has a two-step
//     window to express them with.
//
// WITHOUT THIS THE TWO WALKS DIVERGED, measured rather than anticipated: on a
// seed whose nested sub-leg carried its own leg table the planner declined and
// the executor ACCEPTED — the cross-agreement invariant broken, and invisible
// until a §1(b) fixture existed to drive it. The matrix had only covered §1(a),
// the whole-RC head, which no opt-in site ever passes.
func nestedSubLegsAreExpressible(spans []legSpan) bool {
	for _, s := range spans {
		if s.LegType == nil {
			continue
		}
		for _, sub := range s.LegType.Legs {
			if sub.Kind != values.LegKindNested {
				continue
			}
			if sub.Start < 0 || sub.Start >= len(s.LegType.Fields) {
				return false
			}
			subType, isRT := s.LegType.Fields[sub.Start].FieldType.(*values.RecordType)
			if !isRT || subType == nil || len(subType.Legs) > 0 {
				return false
			}
		}
	}
	return true
}

// positionalMergeSpans derives one span per slot of a POSITIONAL MERGE row —
// the executor twin of values.positionalMergeWindows, and it must agree with it
// bit for bit on (Alias, Kind, Offset, LegType).
//
// The shape is one bare QOV per collapsed lower quantifier, each holding that
// quantifier's WHOLE row, and the executor has spoken it at runtime since before
// this window existed: newOrdinalJoinBuild ENABLES on values.IsPositionalMergeRC
// precisely because the lowest merge level carries no baked refs at all while
// the level above reads its rows by ordinal, and at evaluation each `_i` field
// binds the leg's adapted OrdinalRow, so slot i of the built PositionalRow holds
// the leg's whole row object. What was missing was a layout that could SAY so.
func positionalMergeSpans(rc *values.RecordConstructorValue) (spans []legSpan, mergedType *values.RecordType, ok bool) {
	n := len(rc.Fields)
	spansList := make([]legSpan, 0, n)
	mergedFields := make([]values.Field, n)
	for i, f := range rc.Fields {
		// Guaranteed by IsPositionalMergeRC: a bare QOV of a DISTINCT quantifier,
		// named OrdinalFieldName(i) in position order.
		qov, admitted := values.AsQuantifiedObjectValue(f.Value)
		if !admitted {
			return nil, nil, false
		}
		mergedFields[i] = values.Field{Name: f.Name, FieldType: qov.Type(), Ordinal: i}
		// The per-slot record test BINDS the type and requires non-nil — the same
		// test the planner twin makes, for the same reason: routing it through
		// IsMixedSeedElementType would classify a nil-typed slot as nested and hand
		// a nil LegType to every reader of this span.
		legType, isRT := qov.FlowedType().(*values.RecordType)
		if !isRT || legType == nil {
			elemName := strings.ToUpper(f.Name)
			spansList = append(spansList, legSpan{
				Alias: qov.Correlation(), Kind: values.LegKindFlatRun,
				LegType: &values.RecordType{Fields: []values.Field{{Name: elemName, FieldType: qov.Type(), Ordinal: 0}}},
				Offset:  i, Width: 1,
			})
			continue
		}
		// A nested slot's own row carrying LEG BOUNDARIES puts those boundaries TWO
		// steps from the merged row, and neither this walk nor its planner twin has
		// a two-step span to express them with. DECLINE, exactly as the planner
		// declines: a layout missing a sub-window is a layout whose qualified reads
		// resolve to the wrong slots, so the honest answer is no layout at all.
		if len(legType.Legs) > 0 {
			return nil, nil, false
		}
		spansList = append(spansList, legSpan{
			Alias: qov.Correlation(), Kind: values.LegKindNested,
			LegType: legType, Offset: i, Width: 1,
		})
	}
	return spansList, &values.RecordType{Fields: mergedFields, Legs: mergedLegsOfSpans(spansList)}, true
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
// discriminator never matches a folded projection or a baked+constant fold.
//
// It also does not match the POSITIONAL-MERGE RC, whose bare QOVs are over
// RECORD leg types — and that sentence used to be the end of the story. It is
// not any more: the merge row IS derivable, by positionalMergeSpans, and this
// walk declining it is now a property of the NARROW entry rather than a
// statement about the shape. ordinalJoinSpansAcceptingNested is where it is
// admitted, as one NESTED span per slot. The narrow entry keeps declining
// because its answer drives windowsOK at the cursor, and that is a nil/non-nil
// decision no caller of this entry asked to have changed.
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
		if elemQOV, isQOV := values.AsQuantifiedObjectValue(f.Value); isQOV {
			// The element test is values.IsMixedSeedElementType on BOTH sides, not a
			// second copy of it here: this walk and the planner's must agree bit for
			// bit about which field is the element, because disagreeing about that
			// shifts the offset of every field after it. Two copies of a rule agree
			// until one is edited.
			if values.IsMixedSeedElementType(elemQOV.Type()) {
				if !coverageOK() {
					return nil, nil, false // an unfinished leg run before the element
				}
				if _, dup := seen[elemQOV.Correlation().String()]; dup {
					return nil, nil, false
				}
				seen[elemQOV.Correlation().String()] = struct{}{}
				elemName := strings.ToUpper(f.Name)
				elemLegType := &values.RecordType{Fields: []values.Field{{Name: elemName, FieldType: elemQOV.Type(), Ordinal: 0}}}
				mergedFields[i] = values.Field{Name: elemName, FieldType: elemQOV.Type(), Ordinal: i}
				spansList = append(spansList, legSpan{Alias: elemQOV.Correlation(), Kind: values.LegKindFlatRun, LegType: elemLegType, Offset: i, Width: 1})
				curIdx = -1
				hasElement = true
				continue
			}
		}
		fv, isFV := values.AsFieldValue(f.Value)
		if !isFV || fv.Path() == nil || !fv.Path().IsFrontierPinned() {
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
			spansList = append(spansList, legSpan{Alias: alias, Kind: values.LegKindFlatRun, LegType: legType, Offset: i, Width: 1})
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
		fv, isFV := values.AsFieldValue(f.Value)
		if !isFV || fv.Path() == nil || !fv.Path().IsFrontierPinned() {
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
			spans = append(spans, legSpan{Alias: alias, Kind: values.LegKindFlatRun, LegType: legType, Offset: i, Width: 1})
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
		// Only a FLAT run's leg table describes slots of the merged row. A nested
		// span's LegType is a row one level down, so its boundaries are not
		// s.Offset-relative and splicing them would place them over its
		// neighbours. Both walks refuse such a span upstream; this is the
		// belt-and-braces reading, so the arithmetic below cannot be reached with
		// an operand it does not describe.
		if s.Kind == values.LegKindFlatRun && len(s.LegType.Legs) > 0 {
			for _, sub := range s.LegType.Legs {
				// A REBASE, not a re-mint: the sub-leg's identity is carried
				// verbatim and only its offset moves.
				mergedLegs = append(mergedLegs, values.NewRecordTypeLeg(
					// A REBASE carries the KIND as it carries the Alias. A rebase that
					// re-mints a kind is the same defect class as one that re-mints an
					// identity: the sub-leg did not change shape, only offset.
					sub.Kind, sub.Alias, sub.Name, s.Offset+sub.Start, sub.Width))
			}
			continue
		}
		// The span's OWN kind, carried. A top-level span is a flat run for every
		// shape the pristine and mixed walks produce; the positional-merge walk
		// produces nested ones, and a stamp here would flatten them.
		mergedLegs = append(mergedLegs, values.NewRecordTypeLeg(
			s.Kind, s.Alias, s.Alias.Name(), s.Offset, s.Width))
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
func resolveSpanLeaf(fv values.FieldValue, fieldName string, legRVs map[values.CorrelationIdentifier]values.Value) (alias values.CorrelationIdentifier, legType *values.RecordType, legOrd int, ok bool) {
	if fv == nil || fv.Path() == nil {
		return alias, nil, 0, false
	}
	qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !isQOV {
		return alias, nil, 0, false
	}
	alias = qov.Correlation()
	legType, isRT := qov.Type().(*values.RecordType)
	if !isRT {
		return alias, nil, 0, false
	}
	accs := fv.Path().Ordinals()
	for depth := 0; len(accs) > 1; depth++ {
		if depth > 32 {
			return alias, nil, 0, false
		}
		rc, isRC := legRVs[alias].(*values.RecordConstructorValue)
		if !isRC {
			return alias, nil, 0, false
		}
		i := accs[0]
		if i < 0 || i >= len(rc.Fields) {
			return alias, nil, 0, false
		}
		slot := rc.Fields[i].Value
		if slotQOV, isQOV := values.AsQuantifiedObjectValue(slot); isQOV {
			alias = slotQOV.Correlation()
			if legType, isRT = slotQOV.Type().(*values.RecordType); !isRT {
				return alias, nil, 0, false
			}
			accs = accs[1:]
			continue
		}
		if slotFV, isFV := values.AsFieldValue(slot); isFV {
			if slotFV.Path() == nil || !slotFV.Path().IsFrontierPinned() {
				return alias, nil, 0, false
			}
			slotQOV, isQ := values.AsQuantifiedObjectValue(slotFV.ChildValue())
			if !isQ {
				return alias, nil, 0, false
			}
			alias = slotQOV.Correlation()
			if legType, isRT = slotQOV.Type().(*values.RecordType); !isRT {
				return alias, nil, 0, false
			}
			slotOrdinals := slotFV.Path().Ordinals()
			composed := make([]int, 0, len(slotOrdinals)+len(accs)-1)
			composed = append(composed, slotOrdinals...)
			accs = append(composed, accs[1:]...)
			continue
		}
		return alias, nil, 0, false
	}
	// A TERMINAL slot that is a bare NON-RECORD QOV of a
	// quantifier we hold a legRV for is the gathered unnest's MIXED element
	// carried through a partition collapse — unnestMixedSeedSpans' trailing-
	// element synthesis lifted one level (the merge translated the seed's
	// direct-QOV element into this single-accessor pinned ref). Synthesize the
	// element's 1-field leg: alias = the SLOT QOV's correlation (the AS alias,
	// never the merge alias), the sole column named from the enclosing RC
	// field (fieldName — the same naming authority as the top-level
	// synthesis). The discriminator is the SLOT SHAPE, and the test for it is
	// values.IsMixedSeedElementType — the SAME authority the seed walk above and
	// the planner's window derivation ask, because all three decide which field is
	// the element and disagreeing about that shifts the offset of every field after
	// it. This site carried a third hand-rolled copy of the assertion; it was
	// bit-identical, which is how a copy stays until one of them is edited. The
	// non-record test is load-bearing against whole-leg record slots (the pristine
	// positional-merge RC), which must keep resolving as merge-leg runs.
	term := accs[0]
	if rc, isRC := legRVs[alias].(*values.RecordConstructorValue); isRC && term >= 0 && term < len(rc.Fields) {
		if slotQOV, isQ := values.AsQuantifiedObjectValue(rc.Fields[term].Value); isQ {
			if values.IsMixedSeedElementType(slotQOV.Type()) {
				elemName := strings.ToUpper(fieldName)
				return slotQOV.Correlation(), &values.RecordType{Fields: []values.Field{
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
		// Both join plans now hand over their leg identities TYPED, so the two arms
		// are the same code. This arm used to mint an identifier from the NLJ's alias
		// text, which put the leg-identity decision for the whole merge path in a
		// consumer's hands: the mint chose the spelling, and an exact comparison
		// against a spelling the comparer chose proves nothing.
		addJoinLegRV(out, t.GetOuterAlias(), t.GetOuter())
		addJoinLegRV(out, t.GetInnerAlias(), t.GetInner())
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
			// A SPLICE is a rebase: the sub-span's kind rides with its alias, as at
			// every other rebase site. Re-minting a kind here would describe a
			// nested sub-leg as flat the moment one existed.
			out = append(out, legSpan{Alias: ss.Alias, Kind: ss.Kind, LegType: ss.LegType, Offset: s.Offset + ss.Offset, Width: ss.Width})
		}
	}
	return out
}

// joinPlanSpansTyped derives the leg windows for a join plan's output row and
// ALSO returns the merged row's RecordType (which ordinalJoinSpansOf already
// computes). The identity-FlatMap positional pass-through
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
	// parentType is the LAZY alternative to legType, for producers that already
	// hold the MERGED row's type and would otherwise have to slice a fresh
	// per-leg *RecordType out of it just to fill legType.
	//
	// legType is read by exactly one thing — TypeNames, which feeds the column
	// names into an OrdinalResolutionError. That is an ERROR path. Building a
	// []values.Field copy and a *RecordType per leg per OUTER ROW to service it
	// is the whole cost of a diagnostic that almost never renders, so a producer
	// may hand over the merged type and the window slices the names on demand
	// instead. legType still wins when set: producers that genuinely have a
	// per-leg type (the leg spans derived from an ordinal join RC) pass it, and
	// its fields are the leg's own re-ordinalized ones.
	parentType *values.RecordType
	offset     int
	width      int
	// fromMergedBinder marks a window produced by bindMergedOuterLegs, so the
	// merged-leg binding census can count LOOKUPS that resolve to one. Windows
	// built by legWindowBinder (which serves an already-established span table)
	// are a different producer and are not counted here.
	fromMergedBinder bool
	// siblingLegs marks a window whose merged row bound TWO OR MORE legs — the
	// box-gather shape, as opposed to a merged row carrying a single leg.
	//
	// Stamped by the producer, which is the only place the row's leg COUNT is in
	// hand: a reader holds one window and cannot tell a lone leg from one of
	// several, and the aliases it could ask about collide across queries. It is
	// set only while the leg-identity census gate is on, because its sole consumer
	// is that census's activation criterion.
	siblingLegs bool
	// shadowsExisting marks a window that DISPLACED a binding the incoming context
	// already carried for the same alias, and shadowed holds what it displaced.
	//
	// This is the structural marker for "a second resolution route exists": when a
	// window shadows, the alias was already resolvable WITHOUT the binder. The
	// binder's shadowing semantics are documented in DIVERGENCES.md.
	//
	// It is recorded to be MEASURED, not to be reasoned from, because the reasoning
	// it invites is wrong. The tempting inference — a window that shadows nothing is
	// the binder's only binding, so a read of it is a genuine consumer — was the
	// premise this marker was added under, and the census refuted it: over a full
	// sqldriver run ZERO of the binder's reads shadow, all of them are unshadowed,
	// and declining them entirely still changes no row
	// (measured by running the shape down both resolution routes). "The only
	// binding" and "load-bearing" are therefore different properties: those reads
	// were the first without being the second, because the value they resolved
	// never reached an answer. The ordinal model has since removed the reads
	// entirely — TestFDB_MergedLegBinding_NothingReadsTheBinder pins that the shape
	// still binds and is still not read — so the flag now describes a channel with
	// no consumer at all. Load-bearing is settled per reader shape by running both
	// routes, never by reading this flag.
	//
	// Recorded only while the leg-identity census gate is on; the map probe is not
	// something the per-outer-row path should pay for in production.
	shadowsExisting bool
	shadowed        any
	// misaimed marks a window whose span was deliberately moved off its own leg by
	// misaimMergedLegWindows, so a read that resolves to it is reading the WRONG
	// slots. Set only under EvaluationContext.WithMergedLegWrongWindows, which is
	// itself honoured only while the leg-identity census gate is on.
	//
	// It travels on the window rather than being inferred at the read site because
	// the reader holds one window and no leg table: it cannot tell a window aimed at
	// its own leg from one aimed at its sibling's, and that distinction is the whole
	// content of the wrong-window instrument's engagement floor.
	misaimed bool
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
	if w.legType != nil {
		return typeFieldNames(w.legType)
	}
	// The lazy form: slice this window's names out of the MERGED type. Bounds
	// are re-checked rather than assumed — this runs while building an error
	// message, and a diagnostic that panics loses the error it was describing.
	if w.parentType == nil {
		return nil
	}
	end := w.offset + w.width
	if w.offset < 0 || w.width < 0 || end > len(w.parentType.Fields) {
		return nil
	}
	out := make([]string, w.width)
	for i := range out {
		out[i] = w.parentType.Fields[w.offset+i].Name
	}
	return out
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

// exactLocalBinding resolves one declared leg window and returns the exact
// record type which owns that window. The boolean means the correlation is
// locally claimed; a claimed malformed window is an error and must not fall
// through to an enclosing binding with the same alias.
func (b *legWindowBinder) exactLocalBinding(
	id values.CorrelationIdentifier,
) (values.OrdinalRow, *values.RecordType, bool, error) {
	for _, s := range b.spans {
		if !values.SameLeg(s.Alias, id) {
			continue
		}
		if s.LegType != nil {
			if w, ok := buriedLegWindow(b.row, s, id); ok {
				return w, w.legType, true, nil
			}
		}
		if s.LegType == nil || s.Offset < 0 || s.Width < 0 || s.Width != len(s.LegType.Fields) {
			return nil, nil, true, layoutBindingError(values.LayoutRuntimeShape,
				"local leg window has no exact record type or invalid bounds")
		}
		return &legWindowRow{parent: b.row, legType: s.LegType, offset: s.Offset, width: s.Width}, s.LegType, true, nil
	}
	for _, s := range b.spans {
		if s.LegType == nil {
			continue
		}
		for _, buried := range s.LegType.Legs {
			if !values.SameLeg(buried.Alias, id) {
				continue
			}
			w, ok := buriedLegWindow(b.row, s, id)
			if !ok {
				return nil, nil, true, layoutBindingError(values.LayoutRuntimeShape,
					"buried local leg window has invalid bounds")
			}
			return w, w.legType, true, nil
		}
	}
	return nil, nil, false, nil
}

// GetQuantifiedBinding gives exact QOV reads the same local-leg precedence as
// GetCorrelationBinding, while rejecting a same-alias foreign exact type
// before it can borrow either this row or an enclosing binding.
func (b *legWindowBinder) GetQuantifiedBinding(view values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "leg-window lookup QOV is not exact")
	}
	window, declared, claimed, err := b.exactLocalBinding(exact.Correlation())
	if err != nil {
		return nil, false, err
	}
	if claimed {
		if declared == nil || !values.FlowedRowShapeEquals(exact, declared) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				"leg-window lookup type disagrees with the local exact leg")
		}
		return window, true, nil
	}
	return (&evaluationObjectBinder{base: b.base}).GetQuantifiedBinding(view)
}

func (b *legWindowBinder) IsExplicitNullQuantifiedBinding(view values.QuantifiedObjectValue) (bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return false, layoutBindingError(values.CorrelationForeignValue, "leg-window absence QOV is not exact")
	}
	_, declared, claimed, err := b.exactLocalBinding(exact.Correlation())
	if err != nil {
		return false, err
	}
	if claimed {
		if declared == nil || !values.FlowedRowShapeEquals(exact, declared) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				"leg-window absence type disagrees with the local exact leg")
		}
		return false, nil
	}
	return (&evaluationObjectBinder{base: b.base}).IsExplicitNullQuantifiedBinding(view)
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
	for _, s := range b.spans {
		// Exact, via the one identity comparison: both sides are correlation
		// IDENTIFIERS (canonical UPPER for user aliases, verbatim lowercase q$N for
		// machine mints). A fold here would let a quoted "q$5" user alias cross into
		// the machine namespace.
		if !values.SameLeg(s.Alias, id) {
			continue
		}
		if s.LegType != nil {
			if w, ok := buriedLegWindow(b.row, s, id); ok {
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
		if w, ok := buriedLegWindow(b.row, s, id); ok {
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
// Malformed bounds are a MISS, never the run-wide window: a whole-concat read
// would silently serve another leg's slots.
//
// A miss is NOT uniformly loud downstream — the disposition depends on the
// reference kind that failed to bind (values.FieldValue.evaluate, the
// RowEvalContext arm):
//   - an UNPINNED leg-relative baked root over a multi-leg row: loud
//     (UnboundEvalContextError), because its ordinal addresses its source's own
//     window and a whole-row read would be a wrong-slot answer;
//   - a lazy (unbaked) reference with no positional row: loud, same error;
//   - a FRONTIER-PINNED baked root: NOT loud. It falls through to a positional
//     read against the whole merged row, which is correct precisely when the
//     ordinal already indexes that row (the plan-time-baked box case) and wrong
//     when it does not.
//
// So the miss path is correct-or-loud for everything except a frontier-pinned
// ref, whose correctness rests on the baker having pinned it against the row it
// is now read from. That is why the typed identity matters here: an unstated
// leg alias turns a bind into a miss, and for the pinned kind the miss is
// silent.
func buriedLegWindow(row values.OrdinalRow, s legSpan, alias values.CorrelationIdentifier) (*legWindowRow, bool) {
	for _, bl := range s.LegType.Legs {
		if values.LegIdentityCensusEnabled() {
			// RETIRED PREDICATE: `bl.Name == alias` — this function took the alias as a
			// STRING and compared it exactly against the buried leg's text.
			values.RecordLegIdentityConversion(values.LegSiteBuriedLegWindow,
				bl.Alias, alias, bl.Name == alias.Name())
			values.RecordLegIdentityLeg(bl)
		}
		if !values.SameLeg(bl.Alias, alias) {
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
func adaptLegPositional(qr QueryResult, legType *values.RecordType, owner values.CorrelationIdentifier) (values.OrdinalRow, error) {
	if qr.Positional != nil {
		// The passthrough requires ORDERED per-slot name agreement with the
		// leg type — a width-only check is not enough: two layouts of the
		// same columns are the same width, and a baked leg ordinal over the
		// wrong layout silently reads the wrong slot. A non-aligned
		// positional row is a LEGITIMATE plan shape (not a gate breach):
		// fall through to the per-layout gather below (rowSlotForLegColumn
		// binds each leg column against the row's own plan-produced type),
		// with the zero-match tripwire as the final guard.
		if legType == nil || positionalMatchesLegType(qr.Positional, legType) {
			return qr.Positional, nil
		}
	}
	row := NewPositionalRow(legType)
	if legType == nil {
		return row, nil
	}
	// Layout PERMUTATION at the leg boundary: gather the row's slots into
	// legType order via the row's own plan-produced RecordType
	// (rowSlotForLegColumn, which resolves only an UNAMBIGUOUS name and hands
	// back the ambiguity otherwise). A documented
	// residual of Go's two-layout seed: gated-join legs are seeded with the
	// LOGICAL (table-shaped) leg type while a physical leg may emit a row
	// typed by its own plan output (covering-index rows are LOGICAL-shaped
	// today — buildCoveringLogicalRow — but the leg contract does not pin
	// every producer's layout), so the two layouts can be permutations of
	// each other. Java has no such adapter —
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
				idx, bind := rowSlotForLegColumn(qr.Positional.Type, f.Name, owner)
				switch bind {
				case legColumnBound:
					v, _ := qr.Positional.Get(idx)
					row.Slots[i] = v
					matched++
				case legColumnAmbiguous:
					// LOUD. The source row carries this leg column at more than
					// one slot and nothing here can say which one the leg type
					// meant. Leaving the slot nil would answer NULL for a
					// column that has a value; taking either duplicate would
					// read the other leg's column. Both return plausible rows
					// that no downstream check rejects, which is why this is a
					// refusal and not a degradation.
					return nil, fmt.Errorf("leg adapter: leg column %q is declared %d times by the source row %v — "+
						"a merged row puts every leg's columns in one flat namespace, so this name addresses two "+
						"different columns and the leg type does not say which; the producer must qualify the leg "+
						"type's column names (or carry a baked ordinal) rather than have this reader guess",
						f.Name, qr.Positional.Type.FieldNameHits(f.Name), typeFieldNames(qr.Positional.Type))
				case legColumnUnbound:
					// The source row does not carry this leg column at all; the
					// slot stays nil and the zero-match tripwire below is the
					// guard for the case where NONE of them bound.
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

// legColumnBind is what one leg-type column name did against a source row's
// layout. THREE outcomes, not two, because the two ways of failing to produce
// an ordinal call for opposite handling by the caller.
type legColumnBind int

const (
	// legColumnBound: exactly one slot answers, and the returned ordinal is it.
	legColumnBound legColumnBind = iota
	// legColumnUnbound: no slot answers. Benign — the source row simply does
	// not carry this leg column, and the gather leaves it nil.
	legColumnUnbound
	// legColumnAmbiguous: the row declares the name MORE THAN ONCE and nothing
	// at this reader can choose between them. Never benign: the value is there,
	// twice, so leaving the slot nil is a WRONG value rather than a missing one
	// — and picking either duplicate is a wrong-leg read of the kind
	// leg_column_dotted_containment_test.go pins (`ID` in both the O and I legs
	// of one merged row addresses two different columns). The caller must
	// refuse, not degrade.
	legColumnAmbiguous
)

// rowSlotForLegColumn binds one leg-type column name to a slot of the source
// row's plan-produced RecordType: the flat lookup first, then — for a DOTTED
// name over a row whose type carries leg boundaries (RecordType.Legs, the
// merged concat / clustered box layout) — the per-leg window: qualifier → the
// leg's window, column → the column WITHIN it. The dotted arm serves the
// correlated-scalar seed legs, whose seed leg types name columns literally
// `LEG.COL` while the physical leg emits the merged row those names address.
//
// THE FLAT LOOKUP DECLINES ON A DUPLICATE NAME, and that decline is reported as
// legColumnAmbiguous rather than folded into the miss. A merged row puts every
// leg's columns in one flat namespace, so two legs that both declare `ID` put
// `ID` at two slots; answering either would be a wrong-leg read that returns a
// plausible value, and answering "not present" would hand the caller a nil for
// a column the row does carry. Both are silent wrong rows, so the ambiguity
// travels to the caller intact and fails there.
//
// A DOTTED name whose flat lookup was ambiguous still tries the per-leg arm:
// the qualifier is strictly more information than the flat namespace has, so if
// a leg window resolves it, that answer is not a guess. Only when nothing
// resolves does the ambiguity stand.
//
// owner is the IDENTITY of the leg whose type supplied `name` — the correlation
// adaptLegPositional is adapting a row FOR. It does not steer the lookup; it is
// recorded so the census can answer the question the conversion turns on: would
// selecting the source window by identity have chosen the leg the text chose?
func rowSlotForLegColumn(rt *values.RecordType, name string, owner values.CorrelationIdentifier) (int, legColumnBind) {
	census := values.LegIdentityCensusEnabled()
	flatHits := rt.FieldNameHits(name)
	if flatHits == 1 {
		i, _ := rt.FieldIndexUnique(name)
		if census {
			recordLegColumnProvenance(rt, name, true, false, nil, "", owner)
		}
		return i, legColumnBound
	}
	flatAmbiguous := flatHits > 1
	unresolved := legColumnUnbound
	if flatAmbiguous {
		unresolved = legColumnAmbiguous
	}
	di := strings.IndexByte(name, '.')
	if di <= 0 || len(rt.Legs) == 0 {
		if census {
			recordLegColumnProvenance(rt, name, false, flatAmbiguous, nil, "", owner)
		}
		return 0, unresolved
	}
	qual, col := name[:di], name[di+1:]
	for i := range rt.Legs {
		leg := rt.Legs[i]
		if !strings.EqualFold(leg.Name, qual) {
			continue
		}
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(rt.Fields) {
			break
		}
		for k := leg.Start; k < end; k++ {
			if strings.EqualFold(rt.Fields[k].Name, col) {
				// The census is told WHICH leg answered, so it can ask whether
				// that leg also states an identity — the fact that decides
				// whether this reader can be re-keyed off the name.
				if census {
					recordLegColumnProvenance(rt, name, false, flatAmbiguous, &leg, qual, owner)
				}
				return k, legColumnBound
			}
		}
		break
	}
	if census {
		recordLegColumnProvenance(rt, name, false, flatAmbiguous, nil, qual, owner)
	}
	return 0, unresolved
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
		if isDottedQualifiedName(f.Name) {
			return true
		}
	}
	return false
}

// isDottedQualifiedName reports whether a column name is a DOTTED QUALIFIER
// (`alias.column`) — the merge-row signal — as opposed to a name that merely
// CONTAINS a dot.
//
// The distinction is load-bearing because a composite column's name is its
// RENDERED TYPE, and a rendered record type contains the dots of every
// qualified column inside it: a one-field record over `C.ID` renders as
// `{_0: C.ID#0}`. Testing for a bare dot classified such a row as merge-shaped,
// which sent a perfectly ordinary one-column leg into the leg adapter's
// zero-match tripwire ("a gated join must not consume a merge-shaped leg")
// instead of the slot-for-slot arm that is correct for it. A rendered composite
// is delimited — a qualifier never is — so the delimiter, not the dot, is what
// separates the two.
func isDottedQualifiedName(name string) bool {
	if strings.HasPrefix(name, "{") || strings.HasPrefix(name, "[") {
		return false
	}
	return strings.IndexByte(name, '.') >= 0
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
func evaluateOrdinalJoinRow(rc *values.RecordConstructorValue, mergedType *values.RecordType, bindings values.CorrelationBinder, clock values.StatementClock) (*PositionalRow, error) {
	if len(rc.Fields) != len(mergedType.Fields) {
		// The merged type is derived from this RC by ordinalJoinSpans — a
		// mismatch means the plan is malformed (a planner bug), which must
		// fail the query, not the process.
		return nil, fmt.Errorf("ordinal join row: RC has %d fields but merged type has %d — the merged type must derive from this RC (ordinalJoinSpans); malformed plan", len(rc.Fields), len(mergedType.Fields))
	}
	row := NewPositionalRow(mergedType)
	// A FOLDED result value (the B1 existential wrap) can carry an uncorrelated
	// ScalarSubqueryValue — a per-query constant the executor pre-evaluates and
	// binds by alias. Thread that map onto the build eval context so the scalar
	// resolves here (else ScalarSubqueryValue.Evaluate is loud: UnboundScalarSubquery).
	// The pristine ordinal-join SEED (baked leg refs only) carries none, so this is
	// nil for the NLJ path — harmless.
	evalCtx := &values.RowEvalContext{Correlations: bindings, ScalarSubqueries: scalarSubqueriesFromBinder(bindings), Clock: clock}
	for i, f := range rc.Fields {
		// Join-leg QOVs are whole record objects. The cursor adapters guarantee
		// an OrdinalRow (or present nil for an unmatched leg); reject a hostile
		// direct binder before the exact FieldValue's Java-compatible
		// non-record=>NULL behavior could turn a malformed executor boundary into
		// a plausible row of NULLs.
		if field, isField := values.AsFieldValue(f.Value); isField {
			if qov, isQOV := values.AsQuantifiedObjectValue(field.ChildValue()); isQOV && bindings != nil && qov.FlowedType().Code() == values.TypeCodeRecord {
				if bound, present := bindings.GetCorrelationBinding(qov.Correlation()); present && bound != nil {
					if _, isOrdinal := bound.(values.OrdinalRow); !isOrdinal {
						ordinal := -1
						if path := field.Path(); path != nil {
							ordinals := path.Ordinals()
							if len(ordinals) > 0 {
								ordinal = ordinals[0]
							}
						}
						return nil, &values.BakedNameContextError{Field: field.DisplayName(), Ordinal: ordinal}
					}
				}
			}
		}
		v, err := f.Value.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		row.Slots[i] = v
	}
	return row, nil
}

// evaluateOrdinalJoinBareRow builds the emitted row for a BARE (non-RC) baked
// result value — see ordinalJoinBuild.Bare. The value is evaluated ONCE against
// the leg bindings and the OUTPUT SHAPE follows from what it produced, not from
// a plan flag:
//
//   - a ROW (the leg row the reference names — every leg adapter produces a
//     *PositionalRow) flows through AS ITSELF, keeping its own type and any leg
//     windows that type carries. Re-wrapping it into a 1-slot row would
//     double-nest it: a downstream ordinal read of column i would land on the
//     whole record instead of the column, which is precisely how the non-build
//     path returns zero rows for this shape;
//   - anything else is a scalar column and wraps into the 1-slot `_0` row every
//     other scalar-output path uses (scalarPositionalRow);
//   - NULL flows as a nil row (the deliberately-NULL leg's extension).
//
// A wrongly-shaped binding still fails LOUD inside Evaluate: a baked ordinal
// read against a row that cannot serve it is an OrdinalResolutionError, so
// dropping the RC-only construction check trades no silence for reach.
func evaluateOrdinalJoinBareRow(v values.Value, bindings values.CorrelationBinder, clock values.StatementClock) (*PositionalRow, error) {
	evalCtx := &values.RowEvalContext{Correlations: bindings, ScalarSubqueries: scalarSubqueriesFromBinder(bindings), Clock: clock}
	out, err := v.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	switch row := out.(type) {
	case nil:
		return nil, nil
	case *PositionalRow:
		return row, nil
	}
	return scalarPositionalRow(out), nil
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
		fv, isFV := values.AsFieldValue(f.Value)
		if !isFV {
			lastCorr = ""
			continue
		}
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV {
			lastCorr = ""
			continue
		}
		corr := qov.Correlation().Name()
		if corr == lastCorr {
			continue
		}
		lastCorr = corr
		if rt, isRT := qov.FlowedType().(*values.RecordType); isRT {
			if len(rt.Legs) > 0 {
				// A clustered box run: its name is the rightmost LEAF's (the
				// sourceBinding convention), which the buried bounds already
				// carry at the leaf's own offset — a run-level entry under
				// that name would shadow it with the WHOLE concat (the
				// RIGHT-box collision: "D.ID" first-matched the box run and
				// read the null-supplying leg's slot). Subs only.
				for _, sub := range rt.Legs {
					legs = append(legs, values.NewRecordTypeLeg(
						// A REBASE carries the KIND as it carries the Alias. A rebase that
						// re-mints a kind is the same defect class as one that re-mints an
						// identity: the sub-leg did not change shape, only offset.
						sub.Kind, sub.Alias, sub.Name, i+sub.Start, sub.Width))
				}
			} else {
				legs = append(legs, values.NewRecordTypeLeg(
					// rcOutputType's top-level box RUN.
					values.LegKindFlatRun, qov.Correlation(), corr, i, len(rt.Fields)))
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
//
// It ASSERTS the width-agreement invariant itself rather than leaving it to the
// caller: every reference to one leg is a copy of the one planner-constructed
// typed QOV, so two references disagreeing on that leg's width is a malformed
// plan. This walk used to overwrite silently (last-wins), and the only explicit
// assert lived in the caller's RC-specific bare-QOV loop — so the BARE arm, which
// calls this and nothing else, had no assert at all, and even on the RC path a
// leg referenced twice by two BAKED refs of different widths passed. Asserting
// here covers both arms with one check, which is also why
// widenLegTypesFromPredicates can keep its first-wins widening: it is the
// widening source, and this is the assertion its doc points at.
func legTypesFromResultValue(rv values.Value) (map[values.CorrelationIdentifier]*values.RecordType, error) {
	legs := make(map[values.CorrelationIdentifier]*values.RecordType)
	var err error
	values.WalkValue(rv, func(n values.Value) bool {
		fv, isFV := values.AsFieldValue(n)
		if !isFV || fv.Path() == nil {
			return true
		}
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV {
			return true
		}
		rt, isRT := qov.Type().(*values.RecordType)
		if !isRT {
			return true
		}
		prev, seen := legs[qov.Correlation()]
		if !seen {
			legs[qov.Correlation()] = rt
			return true
		}
		if len(prev.Fields) != len(rt.Fields) && err == nil {
			err = fmt.Errorf("leg %s carries DIVERGENT types (%d vs %d fields) across the "+
				"result value's baked references — all references must copy the one "+
				"planner-constructed typed QOV (planner bug; malformed plan)",
				qov.Correlation(), len(prev.Fields), len(rt.Fields))
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return legs, nil
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
	// RC is the result value as the RC the build evaluates per-field. Nil
	// exactly when Bare is set.
	RC *values.RecordConstructorValue
	// Bare is the result value when it is NOT an RC — a single baked value the
	// select flows as its whole output (PartitionSelectRule's single-live-lower
	// row translated onto a merge quantifier: `ofOrdinal(QOV(merge), i)`). The
	// built row is that ONE value's evaluation, not an assembled slot list, so
	// there is no OutputType and no leg window to publish: a row-shaped result
	// flows through as itself (the merged leg row it names, windows and all) and
	// a scalar wraps into a 1-slot row. Mutually exclusive with RC.
	Bare values.Value
	// OutputType is the built positional row's single authoritative type
	// (rcOutputType; == ordinalJoinSpans' mergedType for the pristine seed).
	OutputType *values.RecordType
	// OutputLayout is the selected immutable physical address space attached to
	// every row this build emits. It replaces RecordType.Legs as runtime QOV
	// binding authority; folded/partial sources are simply absent windows.
	OutputLayout values.OrdinalLayout
	// OutputLayoutFromPlan records that OutputLayout is the selected plan's
	// admitted physical contract rather than the executor's legacy derivation
	// from RC. configureNullSupplying must preserve that exact contract: it may
	// contain proof-backed scalar windows which are not syntactically visible in
	// RC and therefore cannot be reconstructed here.
	OutputLayoutFromPlan bool
	// OutputSourceOrigins maps a proof-backed output-window source to the exact
	// NLJ leg and exact selected-child source which supplied it. It is used only
	// to derive null-extension presence: a present child row proves an ordinary
	// source matched, while a child source which was itself null-supplying must
	// propagate that child's exact per-row match state.
	OutputSourceOrigins map[values.CorrelationIdentifier]outputSourceOrigin
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
	// knows its inner is an ordinality Explode — newFlatMapCursorWithOuterProperties sets
	// this), NOT the row SHAPE: a leg whose own columns are aliased
	// `_0`/`_1` is shape-identical but binds correctly by NAME
	// (adaptLegPositional).
	OrdinalityLegs map[values.CorrelationIdentifier]struct{}
	// Clock supplies the statement-stable CURRENT_TIMESTAMP-family instant
	// to the build's row evaluation, set from the constructing cursor's
	// EvaluationContext. Without it a CURRENT_* value folded into the baked
	// result value would evaluate against RowEvalContext's wall-clock
	// fallback and drift across rows.
	Clock values.StatementClock
}

type outputSourceOrigin struct {
	topLegAlias        values.CorrelationIdentifier
	childSource        values.QuantifiedObjectValue
	childNullSupplying bool
}

// newOrdinalJoinBuild probes a join plan's result value at cursor
// construction. nil (disabled) when rv is nil or carries no baked ordinal —
// the non-build cursor path (identity pass-through / fold).
//
// Two ENABLED shapes, discriminated by the result value's own structure:
//
//   - an RC — the leg-concat seed or a folded projection: the built row is one
//     slot per RC field, with leg windows when the seed is pristine;
//   - a BARE (non-RC) baked value — the select flows ONE value as its whole
//     output: the built row is that value's evaluation (see Bare and
//     evaluateOrdinalJoinBareRow). Java imposes no RC requirement at any point;
//     ImplementNestedLoopJoinRule.java:187,201,214 hand
//     selectExpression.getResultValue() to RecordQueryFlatMapPlan verbatim in
//     all three arms, and PartitionSelectRule.java:281,319 mints exactly this
//     shape — a single-live-lower select flows one leg's whole row, and a later
//     positional-merge round translates that bare QOV into
//     `ofOrdinal(QOV(merge), i)`.
//
// The bare arm must stay ENABLED and must not be turned back into either an
// error or a decline. Both were MEASURED on the 3-way comma join with equijoin
// predicates and a projected EXISTS
// (sqldriver.TestFDB_CommaJoin3ProjectedExistsWithEquijoins, which carries the
// three correct rows): erroring fails the query outright, and declining to the
// non-build path executes and returns ZERO rows, because that path binds the
// outer by name rather than adapting it to the merge layout the inner's
// pushed-down SARGs read by ordinal, then wraps the flowed leg row in a 1-slot
// scalar row.
func newOrdinalJoinBuild(rv values.Value, preds []predicates.QueryPredicate) (*ordinalJoinBuild, error) {
	return newOrdinalJoinBuildWithOutputLayout(rv, preds, nil, nil)
}

// newOrdinalJoinBuildWithOutputLayout is the selected-plan counterpart of
// newOrdinalJoinBuild. The plan-owned layout is authoritative because it can
// carry exact retained-source proofs which are no longer present as QOV nodes
// in rv. sourceOrigins is an equally exact construction-time proof used only
// for null-supplying window presence; both inputs are defensively admitted and
// copied before the cursor retains them.
func newOrdinalJoinBuildWithOutputLayout(
	rv values.Value,
	preds []predicates.QueryPredicate,
	outputLayout values.OrdinalLayout,
	sourceOrigins map[values.CorrelationIdentifier]outputSourceOrigin,
) (*ordinalJoinBuild, error) {
	// Two build triggers:
	//   - a FrontierPinned baked reference anywhere in the RV (the flat
	//     seed, its folds, and the post-translation MIXED upper shape whose
	//     fields are ofOrdinal-over-innerMerge alongside bare leg QOVs);
	//   - the positional-merge RC (ALL fields bare `_i`-named QOVs —
	//     the lowest merge level carries no baked refs at all, but its rows
	//     must build positional: the level above reads them by ordinal).
	if rv == nil {
		return nil, nil
	}
	rc, isRC := rv.(*values.RecordConstructorValue)
	// The legacy no-layout constructor uses the structural bake/merge probe to
	// distinguish an ordinal build from a name-model join whose emitted row is
	// the direct concatenation of its two children. Every selected NLJ has an
	// output layout, so its mere presence is not a build trigger: forcing an
	// ordinary one-level RC through evaluation discards mergeRows' leg windows
	// and shadows the exact child bindings used by filters above the join.
	//
	// A selected layout forces RC evaluation only when it proves mergeRows cannot
	// realize the selected program: either the layout carries proof-only retained
	// sources, the RC descends through a nested object inside a leg while the
	// physical children arrive boxed, or one output slot retains a whole record
	// QOV. Plain child concatenation cannot realize the last shape: it would
	// append the record's fields instead of placing the whole object in that one
	// exact selected-carrier slot. In all three cases the RC must be evaluated to
	// produce the selected flat carrier. Non-RC values retain the existing
	// baked-only Bare admission, and the legacy constructor retains its ordinary
	// semantic-RC decline.
	planBackedRC := outputLayout != nil && isRC &&
		(len(sourceOrigins) > 0 || recordConstructorReadsNestedLegPath(rc) ||
			recordConstructorRetainsWholeRecordSlot(rc))
	if !values.ContainsBakedOrdinal(rv) && !values.IsPositionalMergeRC(rv) && !planBackedRC {
		return nil, nil
	}
	if !isRC {
		// A BARE (non-RC) baked result value: the select's output is not an
		// assembled row but a SINGLE flowed value read out of a leg. Java's
		// RecordQueryFlatMapPlan has no RC requirement at all — it evaluates
		// selectExpression.getResultValue() against the bound legs whatever its
		// shape (ImplementNestedLoopJoinRule.java:187,201,214 pass the select's
		// result value verbatim in all three arms) — and PartitionSelectRule
		// legitimately MINTS this shape: its single-live-lower arm flows one
		// leg's whole row (`quantifier.getFlowedObjectValue()`,
		// PartitionSelectRule.java:281), and a later positional-merge round
		// translates that bare QOV onto the merge quantifier
		// (`resultValue.translateCorrelations(translationMap)`, :319), leaving
		// `ofOrdinal(QOV(merge), i)` — a bare baked reference — as the upper
		// select's whole result value.
		//
		// The build stays ENABLED for it, and that is the load-bearing part:
		// DECLINING (returning a nil build) routes the cursor down the non-build
		// path, which binds the outer by name instead of adapting it to the merge
		// layout the inner's pushed-down SARGs read by ordinal, and wraps the
		// flowed row in a 1-slot scalar row. Measured on the 3-way comma join
		// with equijoin predicates and a projected EXISTS: declining executes and
		// returns ZERO rows where three are correct. Enabled, the legs bind
		// exactly as for an RC and the one value is evaluated against them.
		// The width-agreement assert runs on THIS path too — it lives inside
		// legTypesFromResultValue, so the Bare arm gets it without a copy of the RC
		// arm's loop. It used to be RC-only, which left the arm that has exactly one
		// leg-type source with no check on that source at all.
		bareLegTypes, err := legTypesFromResultValue(rv)
		if err != nil {
			return nil, err
		}
		bare := &ordinalJoinBuild{Enabled: true, Bare: rv, LegTypes: bareLegTypes}
		widenLegTypesFromPredicates(bare.LegTypes, preds)
		return bare, nil
	}
	spans, _, windowsOK := ordinalJoinSpans(rc)
	// LegTypes come from the RESULT VALUE *and* the join PREDICATES: a
	// folded projection RV can DROP a leg entirely while a
	// baked cross-leg ON predicate still references it — collecting from the
	// RV alone left the dropped leg typeless, and the leg
	// adapted to a ZERO-WIDTH binding that blew up the predicate
	// (loud OrdinalResolutionError, "row columns []") on a legitimate plan.
	legTypes, err := legTypesFromResultValue(rc)
	if err != nil {
		return nil, err
	}
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
		if qov, isQOV := values.AsQuantifiedObjectValue(f.Value); isQOV {
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := legTypes[qov.Correlation()]; !seen {
					legTypes[qov.Correlation()] = rt
				} else if len(prev.Fields) != len(rt.Fields) {
					return nil, fmt.Errorf("leg %s carries DIVERGENT types (%d vs %d fields) across the RV's bare-QOV and baked-reference sources — all references must copy the one planner-constructed typed QOV (planner bug; malformed plan)", qov.Correlation(), len(prev.Fields), len(rt.Fields))
				}
			} else {
				// A bare QOV over a NON-record type: the lateral-unnest
				// bare-scalar/struct element leg — bind its whole flowed value
				// raw (see RawLegs).
				if rawLegs == nil {
					rawLegs = map[values.CorrelationIdentifier]struct{}{}
				}
				rawLegs[qov.Correlation()] = struct{}{}
			}
		}
	}
	widenLegTypesFromPredicates(legTypes, preds)
	outputType := rcOutputType(rc)
	derivedLayout, err := values.NewFlatOrdinalLayoutForRetainedResult(rc, nil)
	if err != nil {
		return nil, fmt.Errorf("ordinal join output layout: %w", err)
	}
	fromPlan := outputLayout != nil
	if !fromPlan && len(sourceOrigins) > 0 {
		return nil, layoutBindingError(values.LayoutSourceNotProvided,
			"nested-loop join source origins require a selected output layout")
	}
	if fromPlan {
		if err := values.ValidateOrdinalLayoutAdmission(outputLayout); err != nil {
			return nil, fmt.Errorf("ordinal join selected output layout: %w", err)
		}
		if outputLayout.CarrierKind() != values.OrdinalCarrierRecord ||
			outputLayout.Carrier() == nil ||
			!values.PhysicalCarrierType(outputLayout).Equals(outputType) {
			return nil, layoutBindingError(values.LayoutCarrierMismatch,
				"selected nested-loop join layout disagrees with the result program")
		}
		declared := make(map[values.CorrelationIdentifier]struct{})
		for _, source := range outputLayout.WindowSources() {
			declared[source.Correlation()] = struct{}{}
		}
		for source, origin := range sourceOrigins {
			if source.IsZero() || origin.topLegAlias.IsZero() || origin.childSource == nil {
				return nil, layoutBindingError(values.CorrelationZero,
					"nested-loop join output-source origin contains a zero correlation")
			}
			if origin.childSource.Correlation() != source {
				return nil, layoutBindingError(values.CorrelationForeignValue,
					"nested-loop join output-source origin names a different child source")
			}
			if _, ok := declared[source]; !ok {
				return nil, layoutBindingError(values.LayoutSourceNotProvided,
					"nested-loop join source origin is absent from the selected output layout")
			}
		}
		derivedLayout = outputLayout
	}
	retainedOrigins := make(map[values.CorrelationIdentifier]outputSourceOrigin, len(sourceOrigins))
	for source, origin := range sourceOrigins {
		retainedOrigins[source] = origin
	}
	build := &ordinalJoinBuild{
		Enabled:              true,
		RC:                   rc,
		OutputType:           outputType,
		OutputLayout:         derivedLayout,
		OutputLayoutFromPlan: fromPlan,
		OutputSourceOrigins:  retainedOrigins,
		Spans:                spans,
		WindowsOK:            windowsOK,
		LegTypes:             legTypes,
		RawLegs:              rawLegs,
	}
	// Both leg-type sources are now populated (legTypesFromResultValue, the bare
	// QOV pass and widenLegTypesFromPredicates have all run), so this is the
	// first point at which they can be compared. widenLegTypesFromPlan runs the
	// same assertion again after it adds plan-discovered legs.
	if err := build.assertLegTypeSourcesAgree(); err != nil {
		return nil, err
	}
	return build, nil
}

// recordConstructorReadsNestedLegPath reports the one selected-layout RC shape
// which a plain concat of child rows cannot realize: a field descends through a
// nested record owned by a quantified leg. A one-accessor field is already a
// natural child slot and must keep the ordinary mergeRows path (and its exact
// leg windows). Childless/computed values do not prove a nested physical leg.
func recordConstructorReadsNestedLegPath(rc *values.RecordConstructorValue) bool {
	if rc == nil {
		return false
	}
	nested := false
	values.WalkValue(rc, func(value values.Value) bool {
		field, ok := values.AsFieldValue(value)
		if !ok || field.Path() == nil || field.Path().Len() <= 1 {
			return true
		}
		if _, ok := values.AsQuantifiedObjectValue(field.ChildValue()); !ok {
			return true
		}
		nested = true
		return false
	})
	return nested
}

// recordConstructorRetainsWholeRecordSlot reports a direct exact record QOV in
// one RC output slot. NewFlatOrdinalLayoutForRetainedResult publishes that QOV
// as an ObjectPath source, and the selected-layout carrier check below requires
// the same exact record type at the same ordinal before a build is returned.
// A scalar QOV remains a natural one-slot child value and is not authority to
// leave mergeRows; ordinary flat one-level RCs likewise retain their leg-window
// preserving concatenation path.
func recordConstructorRetainsWholeRecordSlot(rc *values.RecordConstructorValue) bool {
	if rc == nil {
		return false
	}
	for _, field := range rc.Fields {
		qov, ok := values.AsQuantifiedObjectValue(field.Value)
		if !ok {
			continue
		}
		if !values.IsMixedSeedElementType(qov.FlowedType()) {
			return true
		}
	}
	return false
}

// configureNullSupplying rebuilds the output layout with explicit per-row
// presence for the named join legs. The exact QOV roots are discovered from
// the admitted result program; a missing/differently-typed root is a malformed
// plan, never a reason to infer presence from all-NULL field values.
func (b *ordinalJoinBuild) configureNullSupplying(correlations ...values.CorrelationIdentifier) error {
	if b == nil || b.RC == nil || len(correlations) == 0 {
		return nil
	}
	if b.OutputLayoutFromPlan {
		// RecordQueryNestedLoopJoinPlan constructed this exact layout with the
		// join type in hand, including every admitted source's null-supplying
		// bit. Re-deriving from RC would silently erase proof-only scalar
		// sources. Per-row presence for such sources is derived from their exact
		// child origins in evaluateBound.
		return nil
	}
	wanted := make(map[values.CorrelationIdentifier]struct{}, len(correlations))
	for _, correlation := range correlations {
		wanted[correlation] = struct{}{}
	}
	found := make(map[values.CorrelationIdentifier]values.QuantifiedObjectValue, len(correlations))
	var conflict error
	values.WalkValue(b.RC, func(value values.Value) bool {
		qov, ok := values.AsQuantifiedObjectValue(value)
		if !ok {
			return true
		}
		if _, needed := wanted[qov.Correlation()]; !needed {
			return true
		}
		if previous, exists := found[qov.Correlation()]; exists && !values.FlowedTypesEqual(previous, qov) {
			conflict = fmt.Errorf("null-supplying leg %s has conflicting exact types", qov.Correlation())
			return false
		}
		found[qov.Correlation()] = qov
		return true
	})
	if conflict != nil {
		return conflict
	}
	nullSources := make([]values.QuantifiedObjectValue, 0, len(correlations))
	for _, correlation := range correlations {
		qov, exists := found[correlation]
		if !exists {
			// A completely projected-away leg needs no output window or presence.
			continue
		}
		nullSources = append(nullSources, qov)
	}
	layout, err := values.NewFlatOrdinalLayoutForRetainedResult(b.RC, nullSources)
	if err != nil {
		return fmt.Errorf("ordinal join null-supplying layout: %w", err)
	}
	b.OutputLayout = layout
	return nil
}

// widenLegTypesFromPredicates adds the leg types the join PREDICATES carry to
// an already-collected map. A folded projection RV can DROP a leg entirely
// while a baked cross-leg ON predicate still references it; collecting from the
// RV alone leaves the dropped leg typeless, and it then adapts to a ZERO-WIDTH
// binding that blows up the predicate. FIRST-WINS rather than a divergence
// assert: this is the widening SOURCE, and the RV-side collection the caller
// already ran is the one that asserts width agreement.
func widenLegTypesFromPredicates(legTypes map[values.CorrelationIdentifier]*values.RecordType, preds []predicates.QueryPredicate) {
	for _, p := range preds {
		predicates.ReplaceValues(p, func(v values.Value) values.Value {
			fv, isFV := values.AsFieldValue(v)
			if !isFV || fv.Path() == nil {
				return v
			}
			if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
				if rt, isRT := qov.Type().(*values.RecordType); isRT {
					if _, seen := legTypes[qov.Correlation()]; !seen {
						legTypes[qov.Correlation()] = rt
					}
				}
			}
			return v
		})
	}
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
// newFlatMapCursorWithOuterProperties with the inner plan; the NLJ path gets the same widening
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
// preserves marker and type. The width-divergence error below asserts that
// load-bearing invariant: a violation is a malformed plan (planner bug) and
// fails the query, not the process. The walk callback has no error channel,
// so the divergence is captured and returned after the walk.
func (b *ordinalJoinBuild) widenLegTypesFromPlan(plan plans.RecordQueryPlan) error {
	if !b.enabled() || plan == nil {
		return nil
	}
	var divergence error
	// The walk continues widening LegTypes after a capture; harmless — the
	// caller (newFlatMapCursorWithOuterProperties) discards the whole build on error.
	walkBakedRefs(plan, func(v values.Value) values.Value {
		fv, isFV := values.AsFieldValue(v)
		if !isFV || fv.Path() == nil {
			return v
		}
		if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
			// Reserved current names the local physical row phase of whichever
			// operator owns this predicate/SARG. A nested inner plan can contain
			// several such phases with legitimately different widths; none is a
			// FlatMap leg identity. Folding them into LegTypes under the one
			// `_current` key creates a false divergence and can never help the leg
			// adapter, whose lookups are by the FlatMap's named outer/inner aliases.
			if qov.Correlation() == values.CurrentCorrelation() {
				return v
			}
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if prev, seen := b.LegTypes[qov.Correlation()]; !seen {
					b.LegTypes[qov.Correlation()] = rt
				} else if len(prev.Fields) != len(rt.Fields) && divergence == nil {
					divergence = fmt.Errorf("leg %s carries DIVERGENT baked types (%d vs %d fields) across the RV/predicate/SARG sources — all baked references must copy the one seed-constructed typed QOV (planner bug; malformed plan)", qov.Correlation(), len(prev.Fields), len(rt.Fields))
				}
			}
		}
		return v
	})
	if divergence != nil {
		return divergence
	}
	// A leg this walk ADDED can newly collide with the seed leg window, so the
	// two-source agreement is re-asserted here rather than only at construction.
	// The walk never overwrites an existing entry (the arm above reports instead),
	// so a leg that agreed before still agrees.
	return b.assertLegTypeSourcesAgree()
}

// walkBakedRefs walks a physical plan tree's predicate surfaces —
// PredicatesFilter/Filter predicates and scan/index comparison operands —
// applying collect to every value found there. It is the ONE baked-reference
// plan walk (a single derivation path), shared by
// widenLegTypesFromPlan (the build-side type widening, width-divergence error
// captured in its collector) and probeOuterBakedType (the
// disabled-build probe).
//
// RecordQueryNestedLoopJoinPlan also implements GetPredicates but is
// DELIBERATELY omitted: a baked-ref-bearing NLJ does not appear inside a
// gated FlatMap's inner plan (an existential inner is never itself consumed
// as a gated join leg).
// A missed predicate surface fails SAFE for the BUILD consumer only: a
// typeless leg dies loudly at zero width downstream. It does NOT fail safe for
// the PROBE consumer — a probe negative is indistinguishable from "this inner
// plan has no baked references", which licenses name-keyed binding. So the arm
// set has to cover every surface a SARG can be pushed onto, which is why the
// covering scan is enumerated below rather than left to child descent.
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
		// The three SARGable leaves, read through the one accessor they share.
		// A COVERING scan belongs in this set because the access path emits
		// Fetch(Covering(IndexScan)) for EVERY index-backed access (RFC-220), so
		// it is the ordinary shape here, not an exotic one — and it holds its
		// index scan as a FIELD, so the generic child descent below never
		// reaches the operands. Its omission would NOT fail safe the way a
		// missed predicate surface does: for the probe, a nil outer type reads
		// as "no baked references at all" and licenses the identity
		// pass-through to bind by NAME, which is the exact state the probe
		// exists to prevent. The covering plan's GetScanComparisons delegates to
		// the inner scan, whose physical range it shares.
		case *plans.RecordQueryScanPlan, *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan:
			sargable, ok := t.(interface {
				GetScanComparisons() []*predicates.ComparisonRange
			})
			if !ok {
				break
			}
			for _, cr := range sargable.GetScanComparisons() {
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
// from those references through the shared walker; the width-divergence error
// asserts the same one-seed invariant widenLegTypesFromPlan pins.
func probeOuterBakedType(plan plans.RecordQueryPlan, outerAlias values.CorrelationIdentifier) (*values.RecordType, error) {
	var found *values.RecordType
	// The walk callback has no error channel; a width divergence — a
	// malformed plan (planner bug) that must fail the query, not the
	// process — is captured and returned after the walk.
	var divergence error
	walkBakedRefs(plan, func(v values.Value) values.Value {
		fv, isFV := values.AsFieldValue(v)
		if !isFV || fv.Path() == nil || !fv.Path().IsFrontierPinned() {
			return v
		}
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV || qov.Correlation() != outerAlias {
			return v
		}
		if rt, isRT := qov.Type().(*values.RecordType); isRT {
			if found == nil {
				found = rt
			} else if len(found.Fields) != len(rt.Fields) && divergence == nil {
				divergence = fmt.Errorf("outer %s carries DIVERGENT baked types (%d vs %d fields) across the inner plan's references — all baked references must copy the one seed-constructed typed QOV (planner bug; malformed plan)", outerAlias, len(found.Fields), len(rt.Fields))
			}
		}
		return v
	})
	return found, divergence
}

// legType resolves the adapter's leg type for a leg alias.
//
// THE BAKED REFERENCES ARE THE AUTHORITY. LegTypes carries the types recovered
// from the RV's own baked leg references, widened from the join predicates and
// from the plan walk, so it is the type the expressions that will actually be
// EVALUATED were constructed against. Spans are the pristine-seed leg-window
// walk — a positional description of the seed RC, available only when
// WindowsOK, and invisible for a folded RV whose output is a plain projection
// row. So a span types a leg only when no baked reference names it.
//
// WHEN BOTH NAME A LEG THEY DESCRIBE ONE FLOWED ROW, so agreement is an
// invariant rather than a coincidence — which means the order below decides
// nothing on a well-formed plan. A disagreement is a malformed plan, not a
// preference to resolve, and it is rejected by assertLegTypeSourcesAgree at
// BUILD time rather than here: the two sources are fixed for the cursor's
// lifetime, so re-deciding per row would pay for the check on every row of
// every join and still have nowhere to report it from (pairBinder has no error
// return). Checked once at the boundary, this stays a plain accessor.
func (b *ordinalJoinBuild) legType(id values.CorrelationIdentifier) *values.RecordType {
	if exact := b.LegTypes[id]; exact != nil {
		return exact
	}
	if b.WindowsOK {
		for _, s := range b.Spans {
			if s.Alias == id {
				return s.LegType
			}
		}
	}
	return nil
}

// assertLegTypeSourcesAgree rejects a build whose two leg-type sources describe
// one leg with different widths. It runs after every widening has been applied,
// so LegTypes is final.
//
// Without it, legType's precedence silently picks a winner and the leg is
// adapted to a width the other half of the plan does not expect — the failure
// then surfaces far downstream as an ordinal that resolves into the wrong
// column, or not at all. This is the same class as the two DIVERGENT reports
// legTypesFromResultValue and widenLegTypesFromPlan already make, and it is
// worded the same way so all three read as one rule.
func (b *ordinalJoinBuild) assertLegTypeSourcesAgree() error {
	if !b.WindowsOK {
		// Spans describe the pristine seed only; a folded RV has no second
		// source to disagree with.
		return nil
	}
	for _, s := range b.Spans {
		exact := b.LegTypes[s.Alias]
		if exact == nil || s.LegType == nil {
			continue
		}
		if len(exact.Fields) != len(s.LegType.Fields) {
			return fmt.Errorf("leg %s carries DIVERGENT types (%d vs %d fields) between the RV's baked references and the seed leg window — both describe the same flowed row (planner bug; malformed plan)",
				s.Alias, len(exact.Fields), len(s.LegType.Fields))
		}
	}
	return nil
}

// legRows adapts the two join legs into the BUILD-time binding map: alias →
// values.OrdinalRow via adaptLegPositional (layout-matching legs flow through;
// other layouts gather into the leg type). A NIL QueryResult pointer is the
// NULL leg (LEFT/FULL null padding): its alias maps to nil, PRESENT — the
// binder then returns (nil, true) and the leg's baked references evaluate to
// NULL (the null extension falls out of evaluation).
func (b *ordinalJoinBuild) legRows(outerAlias, innerAlias values.CorrelationIdentifier, outer, inner *QueryResult) (map[values.CorrelationIdentifier]values.OrdinalRow, map[values.CorrelationIdentifier]any, error) {
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

func (b *ordinalJoinBuild) bindLeg(legs map[values.CorrelationIdentifier]values.OrdinalRow, raw map[values.CorrelationIdentifier]any, id values.CorrelationIdentifier, qr *QueryResult) error {
	// FirstOrDefault's empty record arm carries an exact-width shell so layout
	// validation remains possible, with LayoutPresence stating that the WHOLE
	// quantified object is absent. Bind that object as NULL before any scalar
	// unwrap or record adaptation. Inferring presence from the non-nil shell
	// makes EXISTS over an empty record subplan unconditionally true; inferring
	// absence from nil slots would misclassify a matched all-NULL record.
	if qr != nil && qr.Positional != nil {
		_, explicitAbsent, err := qr.Positional.wholeObjectBinding()
		if err != nil {
			return err
		}
		if explicitAbsent {
			if _, isRaw := b.RawLegs[id]; isRaw {
				raw[id] = nil
			} else {
				legs[id] = nil
			}
			return nil
		}
	}
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
	// newFlatMapCursorWithOuterProperties).
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
	// A leg whose flowed value is NOT A RECORD binds its DATUM, not the one-slot
	// carrier around it — on EITHER side of the FlatMap.
	//
	// Java decides this from the VALUE's own static result type, never from
	// which side of the join the binding came from:
	// QuantifiedObjectValue.eval (QuantifiedObjectValue.java:82-95) is
	// `isRelation() -> obj`, `isRecord() -> getMessage()`, else
	// `binding.getDatum()`. The two FlatMap bindings are the IDENTICAL call
	// (RecordQueryFlatMapPlan.java:135 and :140 — `withBinding(CORRELATION,
	// quantifier.getAlias(), result)`), so there is no side for a rule to key on.
	//
	// isBareScalarRow is exactly Go's "not a record" test here: it matches the
	// 1-slot `_0` carrier scalarPositionalRow builds around a computed scalar,
	// and a genuine one-column leg carries the COLUMN's name, not `_0`. So a
	// row-shaped leg — one column or many, outer or inner — cannot reach this.
	//
	// The consequence when the carrier is bound instead: the quantifier object
	// is non-null whatever it holds, so ExistsValue.eval
	// (ExistsValue.java:98-100, `getChild().eval() != null`) can no longer see
	// an empty subplan. Measured before the unwrap existed: `SELECT a.id, EXISTS
	// (SELECT 1 FROM e WHERE e.c = b.c) FROM a JOIN b ON … JOIN q ON …` over an
	// EMPTY `e` answered TRUE for every row, while the same query correlated to
	// the FIRST leg (which lands on the sibling non-build path, which already
	// unwrapped) answered FALSE.
	if isBareScalarRow(qr.Positional) {
		raw[id] = qr.Positional.Slots[0]
		return nil
	}
	row, err := adaptLegPositional(*qr, b.legType(id), id)
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
	return b.evaluateBound(&buildLegBinder{legs: legs, raw: raw, base: base})
}

// evaluateBound builds the positional row from any pre-built leg binder — the
// zero-rebuild path the NLJ cursor's per-pair twoLegBinder uses (no per-pair
// map, no per-pair re-adaptation).
func (b *ordinalJoinBuild) evaluateBound(bindings values.CorrelationBinder) (*PositionalRow, error) {
	if b.Bare != nil {
		return evaluateOrdinalJoinBareRow(b.Bare, bindings, b.Clock)
	}
	row, err := evaluateOrdinalJoinRow(b.RC, b.OutputType, bindings, b.Clock)
	if err != nil {
		return nil, err
	}
	if b.OutputLayout != nil {
		presenceBindings := bindings
		var sourcePresence *outputSourcePresenceBinder
		if len(b.OutputSourceOrigins) > 0 {
			sourcePresence = &outputSourcePresenceBinder{
				base: bindings, origins: b.OutputSourceOrigins,
			}
			presenceBindings = sourcePresence
		}
		presence, presenceErr := values.NewWindowMatchPresenceFromCorrelations(
			b.OutputLayout, presenceBindings)
		if sourcePresence != nil && sourcePresence.err != nil {
			return nil, sourcePresence.err
		}
		if presenceErr != nil {
			return nil, presenceErr
		}
		row.Layout = b.OutputLayout
		row.LayoutPresence = presence
	}
	return row, nil
}

// outputSourcePresenceBinder translates only the physical presence question
// for a proof-backed retained source to the selected child leg which supplied
// it. It is deliberately not an evaluation binder: the value is read from its
// exact output window after the row has been built. Presence instead follows
// the leg and, for a nested null-supplying source, the child row's exact match
// state. A matched all-NULL object remains distinct from an absent object.
type outputSourcePresenceBinder struct {
	base    values.CorrelationBinder
	origins map[values.CorrelationIdentifier]outputSourceOrigin
	err     error
}

func (b *outputSourcePresenceBinder) GetCorrelationBinding(
	id values.CorrelationIdentifier,
) (any, bool) {
	if b == nil || b.base == nil {
		return nil, false
	}
	if origin, ok := b.origins[id]; ok {
		value, present := b.base.GetCorrelationBinding(origin.topLegAlias)
		if !present {
			b.err = layoutBindingError(values.LayoutPresenceMissing,
				"retained output source origin leg is absent")
			return nil, false
		}
		if value == nil {
			return nil, true
		}
		row, ok := value.(*PositionalRow)
		if !ok || row == nil || row.Layout == nil {
			b.err = layoutBindingError(values.LayoutRuntimeShape,
				"retained output source origin is not an exact positional row")
			return nil, false
		}
		childNullSupplying, childErr := values.LayoutWindowNullSupplying(
			row.Layout, origin.childSource)
		if childErr != nil {
			b.err = fmt.Errorf("retained output source child layout: %w", childErr)
			return nil, false
		}
		if childNullSupplying != origin.childNullSupplying {
			b.err = layoutBindingError(values.LayoutInvalidWindow,
				"retained output source null-supplying proof disagrees with the child layout")
			return nil, false
		}
		if !origin.childNullSupplying {
			return value, true
		}
		matched, matchErr := values.OrdinalWindowMatchState(
			row.Layout, row.LayoutPresence, origin.childSource)
		if matchErr != nil {
			b.err = fmt.Errorf("retained output source match state: %w", matchErr)
			return nil, false
		}
		if !matched {
			return nil, true
		}
		return value, true
	}
	return b.base.GetCorrelationBinding(id)
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
	outerType        *values.RecordType
	innerType        *values.RecordType
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

// GetQuantifiedBinding preserves the pair-local leg precedence of the legacy
// correlation binder in the exact namespace. A same-alias QOV with another
// exact type is a foreign owner and cannot fall through to the ambient context.
func (b *twoLegBinder) GetQuantifiedBinding(view values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "join-pair lookup QOV is not exact")
	}
	switch exact.Correlation() {
	case b.outerID:
		if b.outerType == nil || !values.FlowedRowShapeEquals(exact, b.outerType) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				fmt.Sprintf("outer join-pair lookup %s: read as %s, local leg %s",
					exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
					values.DescribeType(b.outerType)))
		}
		return b.outer, true, nil
	case b.innerID:
		if b.innerType == nil || !values.FlowedRowShapeEquals(exact, b.innerType) {
			return nil, false, layoutBindingError(values.CorrelationTypeConflict,
				fmt.Sprintf("inner join-pair lookup %s: read as %s, local leg %s",
					exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
					values.DescribeType(b.innerType)))
		}
		return b.inner, true, nil
	}
	return (&evaluationObjectBinder{base: b.base}).GetQuantifiedBinding(view)
}

func (b *twoLegBinder) IsExplicitNullQuantifiedBinding(view values.QuantifiedObjectValue) (bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return false, layoutBindingError(values.CorrelationForeignValue, "join-pair absence QOV is not exact")
	}
	switch exact.Correlation() {
	case b.outerID:
		if b.outerType == nil || !values.FlowedRowShapeEquals(exact, b.outerType) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				"outer join-pair absence type disagrees with the local exact leg")
		}
		return b.outer == nil, nil
	case b.innerID:
		if b.innerType == nil || !values.FlowedRowShapeEquals(exact, b.innerType) {
			return false, layoutBindingError(values.CorrelationTypeConflict,
				"inner join-pair absence type disagrees with the local exact leg")
		}
		return b.inner == nil, nil
	}
	return (&evaluationObjectBinder{base: b.base}).IsExplicitNullQuantifiedBinding(view)
}

// evaluate is the one-shot build: adapt both legs (nil pointer = NULL leg),
// then evaluate the RC per-field into a PositionalRow under OutputType. base
// resolves outer correlations beyond the two legs (may be nil).
func (b *ordinalJoinBuild) evaluate(outerAlias, innerAlias values.CorrelationIdentifier, outer, inner *QueryResult, base values.CorrelationBinder) (*PositionalRow, error) {
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

// GetCorrelationBinding implements values.CorrelationBinder. Two legs of one row
// can carry the SAME identity only if a producer minted it twice, which SameLeg
// cannot happen across the deliberately case-disjoint alias namespaces; the loop
// therefore takes the first identity match and stops. This is NOT the retired
// name lookup's first-match rule wearing a different field — it selects on the
// leg's IDENTITY, which is why a duplicate here would be a producer bug rather
// than an ordinary ambiguity to resolve.
func (b *rowLegsBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if b.row != nil && b.row.Type != nil {
		for _, leg := range b.row.Type.Legs {
			if values.LegIdentityCensusEnabled() {
				// RETIRED PREDICATE: `leg.Name == id.Name()` — exact on the leg's text
				// against the correlation's own spelling. Recording it lets the census
				// measure whether the conversion changed this binder's ANSWER, rather
				// than only whether the pair it now compares folds equal.
				values.RecordLegIdentityConversion(values.LegSiteRowLegsBinder,
					leg.Alias, id, leg.Name == id.Name())
				values.RecordLegIdentityLeg(leg)
			}
			// The leg's IDENTITY decides, through the one comparison every identity
			// proof routes through. Not its Name: matching text here would be an
			// identity claim made by string equality, and the alias namespaces are
			// deliberately case-DISJOINT precisely so that claim cannot be forged.
			if !values.SameLeg(leg.Alias, id) {
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
// The passthrough set is deliberately EXACT, not permissive. InJoin and InUnion
// ARE passthroughs (each re-emits its single inner's rows verbatim under a
// per-in-value binding — the binding changes row count/order/content-selection,
// never the row LAYOUT, and leg windows are a purely positional property; see
// the cases in unwrapToJoinPlan). Left out on purpose: FirstOrDefault/
// DefaultOnEmpty (fabricate a default row), FetchFromPartialRecord (row
// transform), the general multi-child unions/intersections (RecordQueryUnionPlan
// etc.), projection/map/aggregation (row rewrites — projection/map are
// themselves dispatch sites). Missing a genuine passthrough UNDER-provides
// windows, which fails LOUD downstream
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
			// joinPlanSpansTyped. An INNER-identity RV (FirstOrDefault-style
			// shapes) re-emits INNER rows and must NOT unwrap to the outer.
			if qov, ok := values.AsQuantifiedObjectValue(p.GetResultValue()); ok && qov.Correlation() == p.GetOuterAlias() {
				input = p.GetOuter()
				continue
			}
			return p
		case *plans.RecordQueryInMemorySortPlan:
			input = p.GetInner()
		case *plans.RecordQueryLimitPlan:
			input = p.GetInner()
		case *plans.RecordQueryDistinctPlan:
			input = p.GetInner()
		case *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
			input = p.GetInner()
		case *plans.RecordQueryTypeFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryPredicatesFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryInJoinPlan:
			// An IN-join re-executes its inner ONCE PER in-list value under a
			// parameter binding (executeInJoin: evalCtx.WithBinding(...) then
			// ExecutePlan(GetInner()) flat-mapped). The binding changes which
			// rows are produced and the row COUNT — never the row LAYOUT: the
			// cursor re-emits the inner join's merged QueryResults VERBATIM (no
			// projection/transform). Leg windows are a purely POSITIONAL property
			// of that layout, so the join below the IN-join is still the window
			// authority. Without this, a source-relative ORDER-BY / filter key
			// over a join wrapped by an `... IN (…)` filter (which lowers to an
			// InJoin) reaches a bare multi-leg row and fails LOUD in the
			// correlated fall-through guard, even though the leg structure is
			// intact and resolvable. By the time rows reach an ABOVE-InJoin
			// consumer (the in-memory sort) they are already materialized, so no
			// in-value binding is in scope there — the sort key resolves purely
			// through the leg windows.
			input = p.GetInner()
		case *plans.RecordQueryInUnionPlan:
			// The other lowering of `... IN (…)`: like InJoin it runs its single
			// inner ONCE PER in-value under a binding and MERGE-UNIONs (or
			// concats) the results (executeInUnion — no projection/transform of
			// the inner rows). Row count/order change; the row LAYOUT does not,
			// so the join below it is still the leg-window authority.
			//
			// DEFENSIVE SYMMETRY, not an e2e-reachable path today: the cost model
			// currently prefers InJoin for the join+IN+ORDER-BY shapes that
			// trigger the multi-leg hazard, so no live query is known to route an
			// InUnion over a join into a leg-window consumer. This arm is unit-
			// proven in TestDownstreamLegWindows (not by a driver test) and kept
			// because InUnion is structurally the identical layout-preserving
			// single-inner passthrough (ImplementInUnionRule's result value is a
			// bare QOV over the inner quantifier, whose inner may be a join): the
			// day the cost model routes it here, this pre-closes a latent loud
			// failure. Correct-or-loud regardless — an InUnion whose inner isn't a
			// join still bottoms out at `default → nil` (windowsOK=false).
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
	legs := &legWindowBinder{base: correlationBase(ec), spans: spans, row: pos}
	rc := &values.RowEvalContext{
		Positional:   &spanAwareRow{parent: pos, spans: spans},
		Correlations: legs,
	}
	if ec != nil {
		rc.Binder = ec
		rc.ScalarSubqueries = ec.scalarSubqueries
		rc.Clock = ec
		// Installing an exact object binder changes QOV evaluation from
		// correlation-then-positional to exact-only. Do so only when there is an
		// ambient exact object to preserve; a current-only predicate deliberately
		// retains the positional fallback until its producer supplies an exact
		// current carrier handle.
		if len(ec.quantifiedBindings) > 0 {
			rc.Objects = legs
		}
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
