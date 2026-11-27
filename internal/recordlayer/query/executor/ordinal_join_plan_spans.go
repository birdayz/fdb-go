// Portions derived from FoundationDB Record Layer (RecordQueryFlatMapPlan.java,
// ImplementNestedLoopJoinRule.java, FieldValue.java, PartitionSelectRule.java,
// and others),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/internal/unicode15"
)

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
				elemName := unicode15.ToUpperString(fieldName)
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
