package query

// RFC-173 Outcome-B B1 (U-1) — the existential wrap over a gathered ordinal
// cluster: a multi-way (arity >= 3) INNER join under a correlated WHERE EXISTS
// ORDINALIZES while preserving the index SARG.
//
// WHY A WRAP (the structural law, five falsified attempts deep): a flat
// [ForEach×N, Existential] select is unimplementable except MATERIALIZED —
// PartitionSelectRule refuses any select carrying an existential quantifier
// (rule_partition_select.go ForEach-only guard), so the only physical implementer
// is implementNWayJoinWithExistential, which builds a predicate-free left-deep
// cross-product and applies ALL join predicates as one post-filter (SARG lost by
// construction). And Go's single-pass Cascades cannot re-plan a sub-select minted
// at OnMatch, so the join can only be enumerated separately if it is a SEPARATE
// REFERENCE at translation. Hence: translate the join + the WHERE's non-EXISTS
// conjuncts as its OWN gathered ordinal cluster (the P1 shape — SARG-preserving,
// PartitionSelectRule-enumerable, zero name-model producers), and wrap it with a
// 2-quantifier [ForEach(box), Existential...] select (the P2/P7 shape
// implementExistentialSelect plans as FlatMap(box, FirstOrDefault(inner))).
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

// rebaseLegRefsToBox rewrites every lazy leg reference (a bare FieldValue over a
// leg QOV with a window in the box layout) to the positional read
// ofOrdinal(QOV(box), window.Offset+idx). Returns ok=false when any
// leg-correlated reference SURVIVES the rewrite (a whole-row QOV read, a dotted
// flat read, or a column the leg's window cannot resolve) — the caller declines
// to the name model rather than ship an unbound reference (silent NULL).
func rebaseLegRefsToBox(v values.Value, windows map[string]values.OrdinalSeedLegWindow, boxQOV values.Value) (values.Value, bool) {
	if v == nil {
		return nil, true
	}
	out := values.Replace(v, func(n values.Value) values.Value {
		key, isRef := legRef(n)
		if !isRef {
			return n
		}
		w, isLeg := windows[key]
		if !isLeg || w.Typ == nil {
			return n
		}
		fv := n.(*values.FieldValue) // legRef confirmed the cast
		idx, found := w.Typ.FieldIndex(fv.Field)
		if !found {
			return n // survives → the post-walk fails the rebase loudly
		}
		baked, err := values.NewFieldValueOfOrdinal(boxQOV, w.Offset+idx)
		if err != nil {
			return n // out-of-range cannot happen by construction; fail-open
		}
		return baked
	})
	// Post-walk: no leg-correlated QOV may survive. Anything left (whole-row
	// reads, dotted reads legRef skips, unresolved columns) means the value
	// would evaluate against an unbound quantifier above the wrap.
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

// rebaseLegRefsToBoxPred is rebaseLegRefsToBox over a predicate's value trees.
func rebaseLegRefsToBoxPred(p predicates.QueryPredicate, windows map[string]values.OrdinalSeedLegWindow, boxQOV values.Value) (predicates.QueryPredicate, bool) {
	ok := true
	out := predicates.ReplaceValues(p, func(v values.Value) values.Value {
		rebased, vOK := rebaseLegRefsToBox(v, windows, boxQOV)
		if !vOK {
			ok = false
		}
		return rebased
	})
	return out, ok
}

// translateExistsOverGatheredCluster is the B1 arm: the folded projection
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
		// certs and are NOT re-verified over the wrap yet. B1's first slice is
		// WHERE-EXISTS only; the projected extension is a booked follow-on.
		return nil
	}
	if t.existsFoldHasChain {
		// An intervening Sort/Limit chain re-applies ABOVE the fold with
		// unrebased leg-qualified emissions (sortKeySourceValue + the hidden-field
		// cleanup projection) — resolvable over a name-model output, unbound
		// (silent NULL) over the wrap's positional output. Decline; the chained
		// shape keeps the name-model plan (ordering-over-wrap is a booked
		// extension).
		return nil
	}
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
	boxSel, isSel := box.(interface{ GetResultValue() values.Value })
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

	// The wrap's result value: the folded projection with every leg reference
	// rebased onto the box output (the projected ExistsValue, referencing the
	// existential QOV, passes through untouched).
	rv, rvOK := rebaseLegRefsToBox(resultOverride, windows, boxQOV)
	if !rvOK {
		return nil
	}

	quantifiers := []expressions.Quantifier{outerQ}
	sourceAliases := []string{boxBinding}
	var preds []predicates.QueryPredicate
	// The EXISTS polarity markers (ExistentialValuePredicate / its negation)
	// reference the existential QOVs only; rebase defensively anyway.
	for _, mp := range extractExistsPredicates(f.Predicate) {
		rebased, mpOK := rebaseLegRefsToBoxPred(mp, windows, boxQOV)
		if !mpOK {
			return nil
		}
		preds = append(preds, rebased)
	}
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		quantifiers = append(quantifiers, expressions.NamedExistentialQuantifier(esq.Alias, subRef))
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			rebased, jpOK := rebaseLegRefsToBoxPred(joinPred, windows, boxQOV)
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

	return expressions.NewSelectExpressionWithAliases(rv, quantifiers, preds, sourceAliases)
}
