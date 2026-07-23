package query

// The existential wrap over a gathered ordinal cluster: a multi-way
// (arity >= 3) INNER join under a correlated WHERE EXISTS ORDINALIZES while
// preserving the index SARG.
//
// WHY A WRAP: a flat [ForEach×N, Existential] select carrying a CORRELATED
// existential is unimplementable except MATERIALIZED. RFC-190 190.1 retired
// the ordinal N-way arm this comment used to cite as the only physical
// implementer (implementNWayJoinWithExistential built a predicate-free
// left-deep cross-product and applied ALL join predicates as one post-filter,
// losing the SARG) — the general >=3-way flat existential now decomposes via
// PartitionSelectRule into ordinary 2-quantifier pairs instead. That
// decomposition covers the UNCORRELATED case (buildExistentialJoinSelect's
// direct-emit translation); a CORRELATED existential still has no such route
// (PartitionSelectRule cannot represent a live existential positionally), so
// this wrap remains the mechanism for it. And Go's single-pass Cascades
// cannot re-plan a sub-select minted at OnMatch, so the join can only be
// enumerated separately if it is a SEPARATE REFERENCE at translation. Hence:
// translate the join + the WHERE's non-EXISTS conjuncts
// as its OWN gathered ordinal cluster (SARG-preserving,
// PartitionSelectRule-enumerable, zero name-model producers), and wrap it
// with a 2-quantifier [ForEach(box), Existential...] select
// (implementExistentialSelect plans it as FlatMap(box, FirstOrDefault(inner))).
//
// THE REBASE (the one new mechanism): predicates and the folded projection
// reference join LEGS (FieldValue over QOV(leg)). Above the wrap those
// quantifiers are gone — the box flows ONE positional row under its own
// quantifier — so every leg reference is rewritten AT TRANSLATION to
// FieldValue.ofOrdinal(QOV(box), window.Offset + columnIndex) using the box
// seed's leg-window layout (values.OrdinalSeedLegWindows — the same authority
// the executor's spans agree with). By ordinal, not name: the merged concat's
// field names may collide (duplicates survive); the window is unambiguous.
// The box's internal positionalMergeCase collapse preserves the flat output
// layout (same field count and order), so the translation-time windows and the
// runtime row agree by construction.
//
// FAIL-OPEN: every unhandled shape declines (nil) to the existing name-model
// paths — dup-alias FROM (minted bindings), an un-gated cluster, an unrebasable
// reference (verified by a post-walk: NO leg-correlated QOV may survive), a
// non-RC seed. The wrap only replaces the name-model plan when the whole
// composition succeeds.

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// existsFoldableGatheredCluster reports whether f is a WHERE-EXISTS filter over
// a gated arity>=3 non-dup INNER cluster — the shape whose projection fold must
// route through translateExistsOverGatheredCluster (the projection's leg
// references only resolve over the wrap when folded + rebased; an ordinary Map
// above the wrap could not see the legs). Used to WIDEN the projected-EXISTS
// fold gating: for this shape the fold fires even when the SELECT list carries
// no projected EXISTS.
func (t *cascadesTranslator) existsFoldableGatheredCluster(f *logical.LogicalFilter) bool {
	if len(f.ExistsSubqueries) == 0 {
		return false
	}
	j, ok := f.Input.(*logical.LogicalJoin)
	if !ok || j.Kind != logical.JoinInner {
		return false
	}
	legs := t.gatherInnerClusterLegs(j)
	if len(legs) <= 2 {
		return false
	}
	ops := make([]logical.LogicalOperator, len(legs))
	for i, l := range legs {
		ops[i] = l.op
	}
	if mintedBindingLeg(ops...) != "" {
		return false
	}
	return t.gatesAsFreshCluster(j)
}

// rebaseLegRefsToBox rewrites every lazy leg reference to the positional read
// ofOrdinal(QOV(box), slot). THREE reference shapes are rewritten (the TOTAL
// rebase — a QOV-only rewrite leaves dotted/bare frontier reads lazy, and a
// MIXED result value then silently NULLs them: the baked fields flip the wrap
// cursor to build-ordinal evaluation, whose context has no name channel for
// the lazy remainder):
//   - QOV-shaped: FieldValue{COL, Child: QOV(leg)} → the leg window + COL index
//   - DOTTED frontier: FieldValue{"LEG.COL", no Child} → split at the first dot
//   - BARE frontier: FieldValue{"COL", no Child} → a UNIQUE match across the
//     merged concat's field names (ambiguous/absent stays lazy for the caller's
//     verification to decline)
//
// Returns ok=false when any leg-correlated QOV survives the rewrite (whole-row
// reads, unresolvable columns) — the caller declines to the name model rather
// than ship an unbound reference (silent NULL). Result-value callers must apply
// the STRICTER wrapRVFullyBaked check on top (no lazy read of ANY shape may
// survive in the RV; predicates legitimately keep existential-QOV refs).
func rebaseLegRefsToBox(v values.Value, windows map[string]values.OrdinalSeedLegWindow, mergedType *values.RecordType, boxQOV values.Value) (values.Value, bool) {
	if v == nil {
		return nil, true
	}
	out := values.Replace(v, func(n values.Value) values.Value {
		fv, isFV := n.(*values.FieldValue)
		// A source-relative baked ref (the resolver's construction-time bind)
		// still addresses its LEG's row — rebase it exactly like its lazy
		// twin; only machinery-owned baked nodes (pinned / multi-accessor)
		// are final.
		if !isFV || (fv.Resolved != nil && !fv.SourceRelativeBaked()) {
			return n
		}
		// QOV-shaped leg reference.
		if key, isRef := legRef(n); isRef {
			w, isLeg := windows[key]
			if !isLeg || w.Typ == nil {
				return n
			}
			idx, found := w.Typ.FieldIndex(fv.Field)
			if !found {
				return n // survives → the caller's verification declines
			}
			baked, err := values.NewFieldValueOfOrdinal(boxQOV, w.Offset+idx)
			if err != nil {
				return n // out-of-range cannot happen by construction; fail-open
			}
			return baked
		}
		if fv.Child != nil {
			return n // a non-QOV composed read — leave; verification decides
		}
		// DOTTED frontier read ("LEG.COL").
		if dot := strings.IndexByte(fv.Field, '.'); dot > 0 {
			w, isLeg := windows[strings.ToUpper(fv.Field[:dot])]
			if !isLeg || w.Typ == nil {
				return n
			}
			idx, found := w.Typ.FieldIndex(fv.Field[dot+1:])
			if !found {
				return n
			}
			baked, err := values.NewFieldValueOfOrdinal(boxQOV, w.Offset+idx)
			if err != nil {
				return n
			}
			return baked
		}
		// BARE frontier read ("COL") — bake only on a UNIQUE merged-name match.
		if mergedType == nil {
			return n
		}
		slot, matches := -1, 0
		want := strings.ToUpper(fv.Field)
		for i, mf := range mergedType.Fields {
			if strings.ToUpper(mf.Name) == want {
				slot, matches = i, matches+1
			}
		}
		if matches != 1 {
			return n // absent or ambiguous — verification declines
		}
		baked, err := values.NewFieldValueOfOrdinal(boxQOV, slot)
		if err != nil {
			return n
		}
		return baked
	})
	// Post-walk: no leg-correlated QOV may survive.
	ok := true
	values.WalkValue(out, func(n values.Value) bool {
		if qov, isQOV := n.(*values.QuantifiedObjectValue); isQOV {
			if _, isLeg := windows[strings.ToUpper(qov.Correlation.Name())]; isLeg {
				ok = false
				return false
			}
		}
		return true
	})
	return out, ok
}

// wrapRVFullyBaked reports whether the wrap's rebased RESULT VALUE is uniformly
// build-evaluable. The wrap cursor builds positionally when ANY field is baked
// and then evaluates EVERY field over the build context. That context carries the
// per-leg Correlations plus the query's pre-evaluated ScalarSubqueries map
// (evaluateOrdinalJoinRow threads it from the base EvaluationContext) — but NO
// name channel and NO parameter Binder — so every node must be something that
// context can evaluate. This is a WHITELIST with default DENY: a blacklist
// missed two wrong-rows cases before (a lazy bare read, then a
// ScalarSubqueryValue silently NULLing), so an unknown node kind now declines
// the wrap (fail-open to the name model) instead of being presumed safe. Allowed:
// baked (Resolved) FieldValues, the box's own QOV, literal/constant kinds, the
// pure computation kinds over those that the certs exercise under build
// (arithmetic, boolean ops, CASE, casts, scalar functions, IN-list/LIKE), and a
// ScalarSubqueryValue whose result is REGISTERED for pre-evaluation
// (scalarAliases) — an uncorrelated scalar is a per-query constant the executor
// binds by alias, so it builds evaluable exactly like a ConstantValue. A scalar
// NOT in the registered set (a mis-orchestration) declines rather than risk an
// UnboundScalarSubqueryError at build. Everything else — parameters, aggregates,
// windowed values, foreign QOVs, lazy reads — declines.
func wrapRVFullyBaked(v values.Value, boxBinding string, scalarAliases map[values.CorrelationIdentifier]struct{}) bool {
	ok := true
	values.WalkValue(v, func(n values.Value) bool {
		switch nv := n.(type) {
		case *values.RecordConstructorValue:
			return true
		case *values.FieldValue:
			// A source-relative baked ref (resolver construction bind) is NOT
			// build-evaluable: its ordinal addresses the reference's own
			// source row, not the box row the build context serves — only the
			// rebase's machinery-owned box ofOrdinals qualify. Decline like a
			// lazy read (fail-open to the name model).
			if nv.Resolved == nil || nv.SourceRelativeBaked() {
				ok = false
				return false
			}
			return true
		case *values.QuantifiedObjectValue:
			if !strings.EqualFold(nv.Correlation.Name(), boxBinding) {
				ok = false
				return false
			}
			return true
		case *values.ScalarSubqueryValue:
			if _, registered := scalarAliases[nv.Alias]; !registered {
				ok = false
				return false
			}
			return true
		case *values.ConstantValue, *values.NullValue, *values.BooleanValue:
			return true
		case *values.ArithmeticValue, *values.AndOrValue, *values.NotValue,
			*values.CastValue, *values.PromoteValue, *values.EvaluatesToValue,
			*values.ConditionSelectorValue, *values.PickValue,
			*values.ScalarFunctionValue, *values.InOpValue,
			*values.LikeOperatorValue, *values.PatternForLikeValue:
			return true
		default:
			ok = false
			return false
		}
	})
	return ok
}

// registeredScalarAliases is the set of scalar-subquery aliases already registered
// for executor pre-evaluation (t.scalarSubqueries) — the aliases wrapRVFullyBaked
// admits into a build-evaluable wrap result value. Every projection/filter scalar
// subquery on the path to the wrap is appended before the wrap is built
// (translateProject, translateProjectOverExistsFilter), so a ScalarSubqueryValue
// surviving into the folded RV is registered iff its alias is in this set.
func (t *cascadesTranslator) registeredScalarAliases() map[values.CorrelationIdentifier]struct{} {
	aliases := make(map[values.CorrelationIdentifier]struct{}, len(t.scalarSubqueries))
	for _, ss := range t.scalarSubqueries {
		aliases[ss.Alias] = struct{}{}
	}
	return aliases
}

// rebaseBuriedExistsPlanLegRefs bakes leg references buried in an EXISTS
// subquery's retained plan onto the box's positional output. The builder keeps an
// outer-only conjunct (`EXISTS(... WHERE p.id = 2)`) INSIDE esq.Plan rather than
// extracting it to the JoinPredicate; over the wrap only QOV(box) survives, so the
// buried leg reference must be baked to an ofOrdinal over the box QOV (the same
// leg-window authority the projection and JoinPredicate channels use) so the
// translated inner correlates to the box, not to a renamed-away leg. Only the
// top-level LogicalFilter's predicate is rebased (the reachable retained shape) —
// a leg reference deeper than that survives and the caller's post-translation
// surviving-leg-ref check declines it (correct-or-decline). Non-leg references
// (the inner sources, a nested scalar subquery) pass through untouched. Returns
// (plan, false) only when a leg-correlated QOV survives the top-level rebase.
func rebaseBuriedExistsPlanLegRefs(plan logical.LogicalOperator, windows map[string]values.OrdinalSeedLegWindow, mergedType *values.RecordType, boxQOV values.Value) (logical.LogicalOperator, bool) {
	lf, isLF := plan.(*logical.LogicalFilter)
	if !isLF || lf.Predicate == nil {
		return plan, true
	}
	rebased, ok := rebaseLegRefsToBoxPred(lf.Predicate, windows, mergedType, boxQOV)
	if !ok {
		return plan, false
	}
	lfCopy := *lf
	lfCopy.Predicate = rebased
	return &lfCopy, true
}

// rebaseLegRefsToBoxPred is rebaseLegRefsToBox over a predicate's value trees.
func rebaseLegRefsToBoxPred(p predicates.QueryPredicate, windows map[string]values.OrdinalSeedLegWindow, mergedType *values.RecordType, boxQOV values.Value) (predicates.QueryPredicate, bool) {
	ok := true
	out := predicates.ReplaceValues(p, func(v values.Value) values.Value {
		rebased, vOK := rebaseLegRefsToBox(v, windows, mergedType, boxQOV)
		if !vOK {
			ok = false
		}
		return rebased
	})
	return out, ok
}

// translateExistsOverGatheredCluster handles the folded projection
// (resultOverride) over a WHERE-EXISTS whose input is a gated arity>=3 non-dup
// INNER cluster. Builds the join + non-EXISTS WHERE conjuncts as its own
// gathered ordinal cluster (SARG-preserving), wraps it with the existential
// quantifiers, and rebases the projection + each EXISTS correlation onto the
// box's positional output. Returns nil (fail-open to the name-model paths) on
// any unhandled shape.
func (t *cascadesTranslator) translateExistsOverGatheredCluster(
	join *logical.LogicalJoin,
	f *logical.LogicalFilter,
	resultOverride values.Value,
) expressions.RelationalExpression {
	if join.Kind != logical.JoinInner || len(f.ExistsSubqueries) == 0 || resultOverride == nil {
		return nil
	}
	if projectionReferencesExistsSubquery([]values.Value{resultOverride}) {
		// PROJECTED EXISTS (the boolean in the SELECT list) keeps today's
		// buildExistentialJoinSelect path byte-identical — its FOD/row-preserving
		// semantics and the EXISTS-inner-join composition are pinned by their own
		// tests and are NOT re-verified over the wrap. This handles WHERE-EXISTS
		// only; a projected-EXISTS extension over the wrap is not implemented.
		return nil
	}
	// A CHAINED fold (ORDER BY / LIMIT between the project and the filter) IS
	// supported: translateProjectOverExistsFilter re-applies the chain ABOVE the
	// wrap, and every emission it carries is baked onto the wrap's positional
	// output before it reaches here — the result value (including any hidden
	// remainingOrderBy columns collectExtraSortColumns appended) is uniformly
	// rebased + wrapRVFullyBaked-verified below, and each re-applied sort key
	// pull-up (pullUpSortKeyValue) resolves to an OUTPUT column name that reads the
	// wrap's positional output, never a renamed-away leg. A key that cannot pull up
	// to a bakeable output column declines through the RV verification
	// (correct-or-decline), so t.existsFoldHasChain no longer gates the wrap.
	// Multi-esq (>1 EXISTS) is ADMITTED here: the folded-projection gathers the
	// cluster into ONE ordinal seed, so the wrap `[ForEach(seed), ∃, ∃]` peels via
	// PartitionSelectRule Case 2 (one live lower) — NOT the ≥2-leg merge arm — and
	// physicalizes. Declining instead (name-model) routes a ≥3-way-join multi-esq
	// through the merge arm's anchored re-enumeration, which PANICS; gathering it into
	// a single seed is the safe path. Pinned by the multi-EXISTS n-way join FDB test.
	legs := t.gatherInnerClusterLegs(join)
	if len(legs) <= 2 {
		return nil
	}
	ops := make([]logical.LogicalOperator, len(legs))
	for i, l := range legs {
		ops[i] = l.op
	}
	if mintedBindingLeg(ops...) != "" {
		return nil // dup-alias FROM keeps the loud name-model decline
	}
	if !t.gatesAsFreshCluster(join) {
		return nil
	}
	if t.declineNegatedOuterOnlyEsqValue(resultOverride, f.ExistsSubqueries) {
		return nil
	}
	if t.declineNegatedOuterOnlyEsq(f.Predicate, f.ExistsSubqueries) {
		return nil
	}

	// From here on, translation has a SIDE EFFECT the fallback path would repeat:
	// translateSubqueryRef registers any uncorrelated scalar subquery nested in
	// an EXISTS plan on t.scalarSubqueries, and the executor pre-evaluates EVERY
	// entry — a decline after that registration would run the scalar TWICE (the
	// abandoned entry plus the fallback's own). Roll the slice back on every
	// decline; only a successful wrap keeps its registrations.
	scalarMark := len(t.scalarSubqueries)
	committed := false
	defer func() {
		if !committed {
			t.scalarSubqueries = t.scalarSubqueries[:scalarMark]
		}
	}()

	// The box: join legs + ON preds + the WHERE's non-EXISTS conjuncts, as a
	// FRESH ordinal cluster (the P1 shape). The conjuncts ride INSIDE the box so
	// cross-leg equijoins bake per-leg / single-leg predicates stay lazy — the
	// SARG-preserving positions.
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = false
	box := t.translateGatheredInnerCluster(join, legs, splitNonExistsPredicates(f.Predicate))
	t.inInnerCluster = prevEnclosure
	if box == nil {
		return nil
	}
	boxSel, isSel := box.(*expressions.SelectExpression)
	if !isSel {
		return nil
	}
	boxRC, isRC := boxSel.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		return nil
	}
	windows, mergedType := values.OrdinalSeedLegWindows(boxRC)
	if windows == nil || mergedType == nil {
		return nil
	}

	boxBinding := strings.ToUpper(legBinding(join))
	if boxBinding == "" {
		return nil
	}
	boxCorr := values.NamedCorrelationIdentifier(boxBinding)
	outerQ := expressions.NamedForEachQuantifier(boxCorr, expressions.InitialOf(box))
	boxQOV := values.NewQuantifiedObjectValueOfType(boxCorr, mergedType)

	// The wrap's result value: the folded projection with EVERY leg reference —
	// QOV-shaped, dotted-frontier, and unique-bare — rebased onto the box output
	// (the TOTAL rebase), then verified uniformly baked: the wrap cursor builds
	// positionally when any field is baked and evaluates every field over the
	// build context, so a single surviving lazy read would silently NULL (a
	// mixed-projection can produce exactly that). Correct-or-decline.
	rv, rvOK := rebaseLegRefsToBox(resultOverride, windows, mergedType, boxQOV)
	if !rvOK || !wrapRVFullyBaked(rv, boxBinding, t.registeredScalarAliases()) {
		return nil
	}

	quantifiers := []expressions.Quantifier{outerQ}
	sourceAliases := []string{boxBinding}
	var preds []predicates.QueryPredicate
	// The EXISTS polarity markers (ExistentialValuePredicate / its negation)
	// reference the existential QOVs only; rebase defensively anyway.
	for _, mp := range extractExistsPredicates(f.Predicate) {
		rebased, mpOK := rebaseLegRefsToBoxPred(mp, windows, mergedType, boxQOV)
		if !mpOK {
			return nil
		}
		preds = append(preds, rebased)
	}
	for _, esq := range f.ExistsSubqueries {
		// The builder RETAINS an outer-only conjunct (`EXISTS(... WHERE p.id=2)`)
		// INSIDE esq.Plan rather than extracting it to the JoinPredicate. Left
		// alone, the translated inner free-correlates to a LEG, which the wrap
		// renames away (only QOV(box) survives above it). BAKE it onto the box's
		// positional output first — the same leg-window rebase the folded
		// projection and the JoinPredicate channel use — so the retained conjunct
		// reads QOV(box) by ordinal (the supported correlation channel), exactly
		// as the unnest path bakes a buried outer-only conjunct
		// (rebaseUnnestOuterLegPredicateOrdinal). A leg reference the rebase cannot
		// resolve (a shape deeper than the top-level filter) still survives and the
		// post-translation check below declines it — correct-or-decline.
		if baked, ok := rebaseBuriedExistsPlanLegRefs(esq.Plan, windows, mergedType, boxQOV); ok {
			esq.Plan = baked
		} else {
			return nil
		}
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		// Correct-or-decline safety net: any leg-correlated QOV that survived the
		// rebase (a retained reference the top-level bake did not reach) would fail
		// ordinal resolution over the wrap's positional output, so decline to the
		// name model rather than ship an unbound reference.
		for corr := range subRef.GetCorrelatedTo() {
			if _, isLeg := windows[strings.ToUpper(corr.Name())]; isLeg {
				return nil
			}
		}
		quantifiers = append(quantifiers, expressions.NamedExistentialQuantifier(esq.Alias, subRef))
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			rebased, jpOK := rebaseLegRefsToBoxPred(joinPred, windows, mergedType, boxQOV)
			if !jpOK {
				return nil
			}
			preds = append(preds, rebased)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	// Record the gate decision (mirroring translateJoin's gather dispatch) so
	// downstream wedge-gate consumers see this join as Gated.
	if t.wedgeGate == nil {
		t.wedgeGate = make(map[*logical.LogicalJoin]wedgeGateDecision)
	}
	t.wedgeGate[join] = wedgeGateDecision{
		Gated:  true,
		Arity:  len(legs),
		Reason: "existential wrap over a gathered ordinal cluster (B1)",
	}

	committed = true
	return expressions.NewSelectExpressionWithAliases(rv, quantifiers, preds, sourceAliases)
}
