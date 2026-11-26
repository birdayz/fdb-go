// Portions derived from FoundationDB Record Layer (RecordQueryFlatMapPlan.java,
// ImplementNestedLoopJoinRule.java, FieldValue.java, PartitionSelectRule.java,
// and others),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fmt"

	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
			elemName := unicode15.ToUpperString(f.Name)
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
				elemName := unicode15.ToUpperString(f.Name)
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
					sub.Kind, sub.Alias, sub.Name, s.Offset+sub.Start, sub.Width,
				))
			}
			continue
		}
		// The span's OWN kind, carried. A top-level span is a flat run for every
		// shape the pristine and mixed walks produce; the positional-merge walk
		// produces nested ones, and a stamp here would flatten them.
		mergedLegs = append(mergedLegs, values.NewRecordTypeLeg(
			s.Kind, s.Alias, s.Alias.Name(), s.Offset, s.Width,
		))
	}
	return mergedLegs
}
