package cascades

import (
	"errors"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SelectMergeRule merges nested Select/Filter expressions into a
// single, flatter SelectExpression. When a SelectExpression has a
// ForEach quantifier whose child Reference holds a
// LogicalFilterExpression or another SelectExpression, this rule pulls
// up the child's quantifiers and predicates into the parent.
//
// Pattern (LogicalFilter child):
//
//	SelectExpression(
//	  ForEach(alias=A) → Ref[LogicalFilter(preds, ForEach(alias=B) → Ref[scan])],
//	  ...other quantifiers...,
//	  outerPreds
//	)
//
// Rewrite:
//
//	SelectExpression(
//	  ForEach(alias=B) → Ref[scan],
//	  ...other quantifiers...,
//	  rebase(outerPreds, A→B) + preds
//	)
//
// Pattern (SelectExpression child):
//
//	SelectExpression(
//	  ForEach(alias=A) → Ref[SelectExpression([q1,q2,...], childPreds, childResult)],
//	  ...other quantifiers...,
//	  outerPreds
//	)
//
// Rewrite:
//
//	SelectExpression(
//	  q1, q2, ...,
//	  ...other quantifiers...,
//	  rebase(outerPreds, A→childResult) + rebase(childPreds) + outerPreds
//	)
//
// Ports Java's SelectMergeRule: a REWRITING implementation rule over a final
// Select whose inputs are already pruned (OnPrunedInputsRule), yielding a
// final. Merging only finals keeps its output away from the exploration rules;
// exploring merges re-fed DecorrelateValuesRule, which pushed a values box
// down that the next merge pulled back up, one range(1) more per round.
type SelectMergeRule struct {
	matcher matching.BindingMatcher
}

func NewSelectMergeRule() *SelectMergeRule {
	return &SelectMergeRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("select_merge"),
	}
}

func (r *SelectMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnlyOnPrunedInputs: Java's SelectMergeRule implements OnPrunedInputsRule.
func (r *SelectMergeRule) OnlyOnPrunedInputs() bool { return true }

func (r *SelectMergeRule) OnMatch(call *ImplementationRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	if !isFinalMember(call.Reference, sel) {
		return
	}

	// An OUTER-join SelectExpression is a binary box: exactly two quantifiers,
	// the preserved (left) leg and the null-supplying (right) leg, with a fixed
	// JoinType. Merging ANY child into a leg would dissolve that binary
	// structure into a flat N-quantifier select that re-enumerates as a
	// reorderable cross-cluster — dropping the NULL-padded rows the outer join
	// must produce (e.g. `(a JOIN b) LEFT JOIN c` would lose c's unmatched
	// padding). Java only flattens inner-join-equivalent boxes; an outer-join
	// box is a hard optimization barrier on BOTH sides. Leave it intact.
	// Key on ChildrenAsSet() (the inner-equivalence property — INNER or CROSS,
	// the commutative/mergeable boxes) rather than `== JoinInner`, so a CROSS
	// box stays mergeable; this matches the same axis ChildrenAsSet/EqualsWithoutChildren guard.
	if !sel.ChildrenAsSet() {
		return
	}

	quantifiers := sel.GetQuantifiers()
	// Merging splices a target child's quantifiers into the parent and removes
	// the target edge. Without a proof that the strict carrier remains oriented
	// to its scalar outer, that can dissolve the only cardinality boundary.
	if hasStrictSingleQuantifier(quantifiers) {
		return
	}

	// An EXISTENTIAL WRAP select — exactly ONE ForEach (the box) plus
	// existential quantifier(s) — must NOT have a POSITIONAL ordinal-seed
	// box dissolved into it. The wrap is the semi-join shape
	// implementExistentialSelect plans as FlatMap(box, FirstOrDefault(inner))
	// with the box SEPARATELY enumerated so its index SARGs and ordinal contract
	// stay intact. RFC-190 partitions deliberately emitted name-model N-way
	// existential selects, but that does not make a positional gathered-cluster
	// seed safe to dissolve into its parent. A parent that already has multiple
	// ForEach quantifiers is not this wrapper shape and remains mergeable.
	forEachCount, hasExistential := 0, false
	for _, q := range quantifiers {
		switch q.Kind() {
		case expressions.QuantifierForEach:
			forEachCount++
		case expressions.QuantifierExistential:
			hasExistential = true
		}
	}
	existentialWrap := hasExistential && forEachCount == 1

	// Identify which ForEach quantifiers have a mergeable child.
	// A child is mergeable if the Reference contains a
	// RelationalExpressionWithPredicates member (LogicalFilter or Select).
	type mergeTarget struct {
		idx       int
		child     expressions.RelationalExpressionWithPredicates
		childExpr expressions.RelationalExpression
	}
	var targets []mergeTarget
	for i, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach || q.IsNullOnEmpty() {
			continue
		}
		childRef := q.GetRangesOver()
		if childRef == nil {
			continue
		}
		// Java binds a child whose pruned finals hold exactly one mergeable
		// expression (SelectMergeableProperty, only()).
		finals := childRef.FinalMembers()
		mergeable := 0
		for _, member := range finals {
			if _, ok := member.(expressions.RelationalExpressionWithPredicates); ok {
				mergeable++
			}
		}
		if mergeable != 1 {
			continue
		}
		for _, member := range finals {
			// A strict edge inside a child Select/Filter is equally opaque: the
			// merge would splice it into a wider parent where no implementation
			// owns its per-outer-row contract.
			if hasStrictSingleQuantifier(member.GetQuantifiers()) {
				continue
			}
			childSel, isChildSel := member.(*expressions.SelectExpression)
			// An OUTER-join child SelectExpression is OPAQUE to merging: pulling
			// its legs up into the parent would discard the child's outer-join
			// edge (only the PARENT's JoinType is preserved on the merged
			// SelectExpression), collapsing e.g. `(a LEFT JOIN b) LEFT JOIN c`
			// into a flat reorderable cross-cluster — which drops the
			// NULL-padded rows the inner LEFT JOIN must produce. Java's
			// associativity rules only flatten inner-join-equivalent boxes; an
			// outer-join box is a hard optimization barrier. Leave it nested.
			// ChildrenAsSet() = inner-equivalent (INNER or CROSS); a non-set
			// (outer-join) child box is opaque.
			if isChildSel && !childSel.ChildrenAsSet() {
				continue
			}
			// A lateral chain keeps the FlatMap boundary that binds each collection.
			if (childRefResultIsNonSeed(childRef) || childRefIsPositionalUnnestSelect(childRef)) &&
				siblingFreeCorrelatedTo(quantifiers, i, q.GetAlias()) {
				break
			}
			// The existential-wrap guard (see the header above the target loop):
			// every lateral-UNNEST child stays nested, independent of its result
			// representation or window count. Flattening a positional AS+AT seed
			// exposes its Explode as a direct leg of the three-quantifier
			// existential Select. Flattening a non-positional record-element seed is
			// worse: substituting its whole output alias through the child RC can
			// reinterpret X.EK as the same ordinal of an earlier outer source (for
			// example T.ID), yielding a cheaper but semantically false correlated
			// probe. In both cases the nested child is the sole FlatMap boundary that
			// binds the Explode collection and preserves the authored element owner.
			// An uncorrelated Explode (such as inline VALUES) is not lateral and stays
			// mergeable.
			//
			// The existing >2-window barrier remains for positional NON-UNNEST
			// boxes. A two-window scan/join box still merges exactly as before,
			// so this is not a blanket arity-two optimization barrier. `continue`
			// skips this member, not the target; all semantically-equal members
			// share the result contract that the positional checks recognize.
			if existentialWrap {
				if childRefIsLateralUnnestSelect(childRef) {
					continue
				}
				if childSel, isSel := member.(*expressions.SelectExpression); isSel {
					if rc, isRC := childSel.GetResultValue().(*values.RecordConstructorValue); isRC {
						if w, _ := values.OrdinalSeedLegWindows(rc); len(w) > 2 {
							continue
						}
					}
				}
			}
			if wp, ok := member.(expressions.RelationalExpressionWithPredicates); ok {
				// An ORDINAL child select merging into a
				// multi-quantifier parent is LEGITIMATE composition — the
				// spliced references compose via translateValueCorrelations
				// over ReplaceLeavesOnceMaybe, and baked-over-baked rebuilds
				// FUSE into multi-accessor FieldPaths (the fuse arm);
				// the positive compose pin covers it.
				targets = append(targets, mergeTarget{
					idx:       i,
					child:     wp,
					childExpr: member,
				})
				break
			}
		}
	}

	if len(targets) == 0 {
		return
	}

	byIndex := make(map[int]mergeTarget, len(targets))
	for _, target := range targets {
		byIndex[target.idx] = target
	}
	indices := make([]int, len(quantifiers))
	byAlias := make(map[values.CorrelationIdentifier]int, len(quantifiers))
	used := make(map[values.CorrelationIdentifier]bool)
	for i, q := range quantifiers {
		indices[i] = i
		byAlias[q.GetAlias()] = i
		if _, merged := byIndex[i]; !merged {
			used[q.GetAlias()] = true
		}
	}
	// Go's quantifiers carry SQL-visible names, so a pulled-up binding can
	// share its name with a correlation the select reads from an enclosing
	// scope (a CTE body over LB inside a subquery correlated to the outer LB);
	// it is renamed rather than capturing that read. Java's ids are unique. A
	// read of a leg a merged box flows whole names that leg (the box declares
	// it as a window), so it keeps its binding.
	exposed := make(map[values.CorrelationIdentifier]struct{})
	for _, target := range targets {
		for alias := range selectMergeExposedLegs(target.childExpr) {
			exposed[alias] = struct{}{}
		}
	}
	for alias := range expressions.GetCorrelatedToOfExpression(sel) {
		if _, leg := exposed[alias]; !leg {
			used[alias] = true
		}
	}
	dependencies := make([]map[int]struct{}, len(quantifiers))
	for i, q := range quantifiers {
		dependencies[i] = make(map[int]struct{})
		for alias := range q.GetCorrelatedTo() {
			if dependency, local := byAlias[alias]; local {
				dependencies[i][dependency] = struct{}{}
			}
		}
	}
	order, ok := stableTopologicalOrder(indices, dependencies)
	if !ok {
		return
	}

	tr := newSelectMergeTranslation(call.memo)
	var newQuantifiers []expressions.Quantifier
	var newPredicates []predicates.QueryPredicate
	var newAliases []string
	fail := func(err error) {
		var declined *selectMergeDeclinedError
		if !errors.As(err, &declined) {
			call.Fail(err)
		}
	}
	for _, i := range order {
		q := quantifiers[i]
		parentAlias := ""
		if i < len(sel.GetSourceAliases()) {
			parentAlias = sel.GetSourceAliases()[i]
		}
		target, merged := byIndex[i]
		if !merged {
			translated, err := tr.quantifier(q)
			if err != nil {
				fail(err)
				return
			}
			newQuantifiers = append(newQuantifiers, translated)
			newAliases = append(newAliases, parentAlias)
			continue
		}

		// Translate dependencies before dissolving this box, as Java's
		// correlation-order traversal does. Pulled-up bindings must be unique.
		childQs := target.childExpr.GetQuantifiers()
		renamed := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier)
		for _, childQ := range childQs {
			alias := childQ.GetAlias()
			if used[alias] {
				renamed[alias] = values.UniqueCorrelationIdentifier()
				alias = renamed[alias]
			}
			used[alias] = true
		}
		childTranslation := tr.withoutBindings(childQs).withAliases(renamed)
		for j, childQ := range childQs {
			translated, err := childTranslation.quantifier(childQ)
			if err != nil {
				fail(err)
				return
			}
			if alias, changed := renamed[childQ.GetAlias()]; changed {
				translated = translated.WithAlias(alias)
			}
			newQuantifiers = append(newQuantifiers, translated)
			// A filter passes its row through, so the parent's name is its
			// source's; a select's name is the row it projects, never a source.
			alias := parentAlias
			if childSel, isSelect := target.childExpr.(*expressions.SelectExpression); isSelect {
				alias = ""
				if j < len(childSel.GetSourceAliases()) {
					alias = childSel.GetSourceAliases()[j]
				}
			}
			newAliases = append(newAliases, alias)
		}
		childPreds, err := childTranslation.predicates(target.child.GetPredicates())
		if err != nil {
			fail(err)
			return
		}
		newPredicates = append(newPredicates, childPreds...)
		childResult, err := childTranslation.value(target.childExpr.GetResultValue())
		if err != nil {
			fail(err)
			return
		}
		positionalSeed := false
		if rc, isRC := childResult.(*values.RecordConstructorValue); isRC && len(childQs) > 1 {
			windows, _ := values.OrdinalSeedLegWindows(rc)
			positionalSeed = windows != nil
		}
		tr.add(q.GetAlias(), childResult, positionalSeed)
	}
	result, err := tr.value(sel.GetResultValue())
	if err != nil {
		fail(err)
		return
	}
	if expressions.LegTableConflictsWith(call.Reference, result) {
		return
	}
	parentPreds, err := tr.predicates(sel.GetPredicates())
	if err != nil {
		fail(err)
		return
	}
	newPredicates = append(newPredicates, parentPreds...)
	if len(sel.GetSourceAliases()) == 0 {
		newAliases = nil
	}
	merged, err := expressions.NewSelectExpressionWithJoinType(result, newQuantifiers, newPredicates, newAliases, sel.GetJoinType())
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(merged)
}

var _ ImplementationRule = (*SelectMergeRule)(nil)

// bakedBoxRefCallback returns a values.Replace callback (pre-order) that
// collapses BAKED references over a merged-away box alias through the box's
// result-value RC — the exact leg reference the ordinal named (a
// multi-accessor path collapses its root and fuses the suffix). LAZY
// references over the same alias are skipped whole (their QOV child is
// pointer-marked before the walk descends): a lazy read re-binds to
// the pulled-up leg quantifier of the same name. The alias collision is a
// property of the PLANNING-time dissolved-LEFT regime specifically —
// RewriteOuterJoinRule's box select is quantified by its rightmost leaf's
// name and its dissolution REUSES the null-supplying leg's alias;
// translation-time gated boxes mint <leaf>$BOX bindings, where same-name
// lazy refs cannot arise and this callback's lazy-keep arm is inert. A bare
// box QOV substitutes the whole RC (the merged child's row), with the RC's
// own leg QOVs marked so a leg alias that equals the box alias is never
// re-substituted — the self-reference loop ReplaceLeavesOnceMaybe documents.
//
// Residual hazard (review note, no known trigger): the collapse output (leg
// QOV references) is not protected against capture by a binder BETWEEN the
// merge level and the reference site — Java's unique mints preclude the
// class structurally; in Go, SQL name shadowing makes the shape
// unreachable from user syntax and the $BOX minting closes the gated
// regime. If a new rewrite ever interposes a binder that reuses a leg
// alias, revisit this callback's scoping.
func bakedBoxRefCallback(rcByAlias map[values.CorrelationIdentifier]values.Value) func(values.Value) values.Value {
	// skip holds every node of a subtree the callback RETURNED (the RC
	// itself, a collapsed leg reference) plus the children of kept-intact
	// lazy references. The box is NAMED by its rightmost LEG, so the RC's
	// own internal leg references carry the very alias being substituted —
	// without the pointer-marks the walk's re-descent would self-apply the
	// collapse to them (a leg-relative ordinal read through the box-level
	// window: the wrong column, proven by the shared-pointer RC artifact).
	skip := map[values.Value]struct{}{}
	mark := func(v values.Value) {
		values.Replace(v, func(n values.Value) values.Value {
			skip[n] = struct{}{}
			return n
		})
	}
	// boxLevel discriminates a reference ADDRESSED AT THE BOX (its QOV
	// carries the box's concat type — arity equal to the RC's) from a
	// LEG-level reference that merely shares the box's name (Go names the
	// box by its rightmost leaf, and the dissolution reuses leg aliases, so
	// BOTH leg quantifiers can collide with the box alias: the rightmost
	// leaf on LEFT, the preserved leg on RIGHT). A leg's arity is STRICTLY
	// smaller than the concat's (the other leg has ≥1 column), so the count
	// is a sound key. An untyped reference is box-level: the only binder at
	// the pre-merge scope was the box quantifier (the WHERE-EXISTS wrapper's
	// bare untyped RV). A concrete non-record type (a scalar unnest element)
	// cannot be the box's row, so it is the leg.
	boxLevel := func(t values.Type, rcv *values.RecordConstructorValue) bool {
		rt, isRT := t.(*values.RecordType)
		if !isRT || rt == nil {
			return t == nil || t.Code() == values.TypeCodeUnknown || t.Code() == values.TypeCodeRecord
		}
		return len(rt.Fields) == len(rcv.Fields)
	}
	return func(node values.Value) values.Value {
		if _, s := skip[node]; s {
			return node
		}
		if field, isFV := values.AsFieldValue(node); isFV {
			child := field.ChildValue()
			qov, isQOV := values.AsQuantifiedObjectValue(child)
			if !isQOV {
				return node
			}
			rc, hit := rcByAlias[qov.Correlation()]
			if !hit {
				return node
			}
			path := field.Path()
			if rcv, isRC := rc.(*values.RecordConstructorValue); isRC && path != nil && boxLevel(qov.FlowedType(), rcv) {
				ordinals := path.Ordinals()
				rootOrd := -1
				if len(ordinals) >= 1 {
					rootOrd = ordinals[0]
				}
				// A reused leg alias needs a leg-relative lookup. A distinct box
				// alias addresses its own output row, even without a seed marker.
				_, reusedByLeg := values.GetCorrelatedToOfValue(rcv)[qov.Correlation()]
				if !path.IsFrontierPinned() && len(ordinals) >= 1 && reusedByLeg {
					rootOrd = values.LegAwareRootOrdinal(field, ordinals[0], rcv, rootOrd)
				}
				if len(ordinals) >= 1 && rootOrd >= 0 && rootOrd < len(rcv.Fields) && rcv.Fields[rootOrd].Value != nil {
					slot := rcv.Fields[rootOrd].Value
					if len(ordinals) == 1 {
						mark(slot)
						return slot
					}
					// A MULTI-accessor path over the box (a path fused by an
					// earlier merge round): collapse the ROOT accessor
					// through the RC and preserve the suffix — fused onto
					// the slot's own baked leg reference, the identical
					// construction the rebuild's fuse arm produces. Leaving
					// it whole would strand the reference on the
					// merged-away alias (dangling, or re-bound to the
					// same-named pulled-up leg with the wrong window).
					out, err := values.ResolveFieldOrdinals(slot, ordinals[1:])
					if err == nil && out != nil && sameExactType(out.Type(), field.ResultType()) {
						mark(out)
						return out
					}
				}
			}
			// Lazy, leg-level, or non-collapsible reference: keep it INTACT,
			// including its QOV child — it re-binds by name to the pulled-up
			// leg quantifier of the same name.
			mark(child)
			return node
		}
		if qov, isQOV := values.AsQuantifiedObjectValue(node); isQOV {
			if rc, hit := rcByAlias[qov.Correlation()]; hit {
				if rcv, isRC := rc.(*values.RecordConstructorValue); isRC && !boxLevel(qov.FlowedType(), rcv) {
					return node // a leg-level whole-row read: the pulled-up leg re-binds it
				}
				mark(rc)
				return rc
			}
			return node
		}
		return node
	}
}

// childRefResultIsNonSeed identifies the non-positional lateral-chain barrier.
func childRefResultIsNonSeed(childRef *expressions.Reference) bool {
	for _, m := range childRef.AllMembers() {
		sel, ok := m.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		if rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue); isRC {
			if w, _ := values.OrdinalSeedLegWindows(rc); w != nil {
				return false // positional ordinal seed
			}
		}
		return true
	}
	return false
}

// childRefIsPositionalUnnestSelect reports whether a merge candidate's child is
// a POSITIONAL ordinal-seed SelectExpression that is ITSELF a lateral unnest —
// it has an Explode among its OWN quantifiers (the ORDINAL
// chained first link). This is the positional twin of childRefResultIsNonSeed
// for the chained-unnest barrier: like the non-seed chain, an ordinal chained
// first link must stay NESTED (Java never flattens a lateral chain), because the
// positional-seed rebase cannot compose the retained Explode sibling's fused
// ofOrdinal+name-suffix collection through the merge.
//
// A GATED BOX child (a positional seed whose quantifiers are all ForEach over
// scans/joins — NO Explode of its own) is EXCLUDED: its retained-Explode-sibling
// merge IS handled by the positional-seed rebase (the gathered-unnest owner-window
// bake), so it stays mergeable. Only a child that is itself an unnest FlatMap
// trips the barrier.
func childRefIsPositionalUnnestSelect(childRef *expressions.Reference) bool {
	for _, m := range childRef.AllMembers() {
		sel, ok := m.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue)
		if !isRC {
			continue
		}
		if w, _ := values.OrdinalSeedLegWindows(rc); w == nil {
			continue // not a positional ordinal seed
		}
		if selectIsLateralUnnest(sel) {
			return true
		}
	}
	return false
}

// childRefIsLateralUnnestSelect reports whether one candidate child Select
// owns an Explode whose collection is correlated to another quantifier in that
// same Select. That structural dependency is the lateral-UNNEST boundary; an
// uncorrelated record-valued Explode used as a relation source is deliberately
// excluded.
func childRefIsLateralUnnestSelect(childRef *expressions.Reference) bool {
	if childRef == nil {
		return false
	}
	for _, member := range childRef.AllMembers() {
		if sel, ok := member.(*expressions.SelectExpression); ok && selectIsLateralUnnest(sel) {
			return true
		}
	}
	return false
}

func selectIsLateralUnnest(sel *expressions.SelectExpression) bool {
	if sel == nil {
		return false
	}
	aliases := make(map[values.CorrelationIdentifier]struct{}, len(sel.GetQuantifiers()))
	for _, quantifier := range sel.GetQuantifiers() {
		aliases[quantifier.GetAlias()] = struct{}{}
	}
	for _, quantifier := range sel.GetQuantifiers() {
		ref := quantifier.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, member := range ref.AllMembers() {
			if _, isExplode := member.(*expressions.ExplodeExpression); !isExplode {
				continue
			}
			for correlation := range member.GetCorrelatedToWithoutChildren() {
				if _, isChildAlias := aliases[correlation]; isChildAlias {
					return true
				}
			}
		}
	}
	return false
}

// siblingFreeCorrelatedTo reports whether some quantifier OTHER than index
// `self` ranges over an EXPLODE that is free-correlated to `alias` — the
// chained-unnest signature (the second unnest's Explode collection references
// the first unnest's output alias). Scoped to Explode siblings so the barrier
// never fires for a legitimate correlated SelectExpression-sibling merge
// (RFC-040), which the merge's substitution handles.
func siblingFreeCorrelatedTo(quantifiers []expressions.Quantifier, self int, alias values.CorrelationIdentifier) bool {
	for j, q := range quantifiers {
		if j == self {
			continue
		}
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if _, corr := ref.GetCorrelatedTo()[alias]; !corr {
			continue
		}
		for _, m := range ref.AllMembers() {
			if _, isExplode := m.(*expressions.ExplodeExpression); isExplode {
				return true
			}
		}
	}
	return false
}

// selectMergeExposedLegs returns the quantifiers whose whole row child's result
// flows: a filter's row, or a box's positional legs.
func selectMergeExposedLegs(child expressions.RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	legs := make(map[values.CorrelationIdentifier]struct{})
	add := func(v values.Value) {
		if qov, ok := values.AsQuantifiedObjectValue(v); ok {
			legs[qov.Correlation()] = struct{}{}
		}
	}
	rv := child.GetResultValue()
	add(rv)
	if rc, ok := rv.(*values.RecordConstructorValue); ok {
		for _, field := range rc.Fields {
			add(field.Value)
		}
	}
	return legs
}
