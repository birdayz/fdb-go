package cascades

import (
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
// Ports Java's SelectMergeRule (ImplementationCascadesRule). Placed in
// EXPLORE as an ExpressionRule because Go's PLANNING phase does not
// support multi-round implementation. Functionally equivalent: the
// merged Select is explored + matched + implemented normally.
//
// Convergence: each firing strictly reduces nesting depth. A flat
// Select with no mergeable children causes zero yields.
type SelectMergeRule struct {
	matcher matching.BindingMatcher
}

func NewSelectMergeRule() *SelectMergeRule {
	return &SelectMergeRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("select_merge"),
	}
}

func (r *SelectMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *SelectMergeRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

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
		for _, member := range childRef.AllMembers() {
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
			if childSel, ok := member.(*expressions.SelectExpression); ok && !childSel.ChildrenAsSet() {
				continue
			}
			if wp, ok := member.(expressions.RelationalExpressionWithPredicates); ok {
				// RFC-173 Slice 2 drift assert (contract ruling #1): the
				// translation-time cluster-arity gate SHADOWS this rule's
				// mergeability, so an ORDINAL child (baked result value) may
				// only ever merge into a PURE WRAPPER parent — a select whose
				// single quantifier is this child (the derived-table /
				// WHERE-fold flattening that leaves the post-merge select at
				// exactly the child's 2 ForEach legs). A parent with any
				// OTHER quantifier would flatten the ordinal child into a
				// ≥3-quantifier select — the name-model partition machinery
				// the gate exists to keep baked values out of. That means the
				// gate mis-scoped: a loud planner error, never a silent
				// wrong-model merge (a decline is equally forbidden — it
				// changes plan shapes).
				//
				// S3 fulcrum: an ORDINAL child select merging into a
				// multi-quantifier parent is LEGITIMATE composition -- the
				// spliced references compose via translateValueCorrelations
				// over ReplaceLeavesOnceMaybe, and baked-over-baked rebuilds
				// FUSE into multi-accessor FieldPaths (the W2 commit-1 arm).
				// The S2 drift assert that forbade this shape died with the
				// exactly-2 wedge; the positive compose pin covers it.
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

	// Build alias rebase map (single-quantifier children) and
	// TranslationMap (multi-quantifier children). rcByAlias mirrors the
	// TranslationMap's entries as plain data so the retained-quantifier
	// subtree translation below can rebuild SCOPED maps per nesting level
	// (a locally-rebound alias shadows the merge's substitution).
	aliasMap := values.AliasMap{}
	rcByAlias := map[values.CorrelationIdentifier]values.Value{}
	tmBuilder := NewTranslationMapBuilder()
	mergedIdxSet := map[int]bool{}
	for _, t := range targets {
		mergedIdxSet[t.idx] = true
	}

	// Build the new quantifier list and collect pulled-up predicates.
	newQuantifiers := make([]expressions.Quantifier, 0, len(quantifiers))
	var pulledPredicates []predicates.QueryPredicate

	for i, q := range quantifiers {
		if !mergedIdxSet[i] {
			newQuantifiers = append(newQuantifiers, q)
			continue
		}

		// Find the merge target for this index.
		var target mergeTarget
		for _, t := range targets {
			if t.idx == i {
				target = t
				break
			}
		}

		childQs := target.childExpr.GetQuantifiers()

		if len(childQs) == 1 {
			aliasMap[q.GetAlias()] = childQs[0].GetAlias()
		} else if len(childQs) > 1 {
			// Multi-quantifier child (e.g., Select with 2+ sources):
			// the parent's alias must be replaced with the child's
			// result value. TWO regimes by the child RV's row model
			// (RFC-173 item 3):
			//   - POSITIONAL ordinal-seed RC (a dissolved gated box): the
			//     SURGICAL substitution (rcByAlias → bakedBoxRefCallback) —
			//     baked refs collapse through the RC to the exact leg
			//     reference; LAZY refs stay intact and re-bind by name to
			//     the pulled-up leg quantifier (the box is NAMED by its
			//     rightmost leaf, so the name stays bound). A blanket
			//     TranslationMap substitution would put the dup-bare-named
			//     concat under a lazy read — FieldIndex first-match,
			//     silently the wrong column.
			//   - NAME-MODEL child (anchored record): the TranslationMap
			//     substitution, as always — the anchored RC is name-keyed
			//     and the child alias disappears without a re-binder.
			childResultValue := target.childExpr.GetResultValue()
			capturedResult := childResultValue
			parentAlias := q.GetAlias()
			positionalSeed := false
			if rcv, isRC := capturedResult.(*values.RecordConstructorValue); isRC {
				if w, _ := values.OrdinalSeedLegWindows(rcv); w != nil {
					positionalSeed = true
				}
			}
			if positionalSeed {
				rcByAlias[parentAlias] = capturedResult
			} else {
				tmBuilder.When(parentAlias).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
					return capturedResult
				})
			}
		}

		newQuantifiers = append(newQuantifiers, childQs...)
		pulledPredicates = append(pulledPredicates, target.child.GetPredicates()...)
	}

	// Rebase the parent's result value and predicates.
	newResultValue := sel.GetResultValue()
	if len(aliasMap) > 0 {
		newResultValue = values.RebaseValue(newResultValue, aliasMap)
	}
	// Apply TranslationMap for name-model multi-quantifier children, and the
	// surgical baked-collapse callback for positional-seed children (lazy
	// refs re-bind by name to the pulled-up leg — see the regime comment at
	// the multi-quantifier arm above).
	tm := tmBuilder.Build()
	if !tm.DefinesOnlyIdentities() {
		newResultValue = translateValueCorrelations(newResultValue, tm)
	}
	cb := bakedBoxRefCallback(rcByAlias)
	if len(rcByAlias) > 0 {
		newResultValue = values.Replace(newResultValue, cb)
	}

	// RFC-173 item 3: RETAINED quantifiers whose subtrees hold BAKED
	// references over a MERGED-AWAY alias get those references translated
	// through the merged child's result value — Java's
	// Quantifier.translateCorrelations, which Go's merge never needed until
	// the dissolved-LEFT fold produced the first stranded case: a
	// box-level-baked reference (an enclosing scope's ON conjunct, an
	// amendment-C buried bake) addresses the box quantifier POSITIONALLY;
	// when the box's child select merges up, that positional read would
	// re-bind by name to the same-named pulled-up LEG with the wrong type —
	// the divergent-baked-types class. Collapsing through the RC resolves
	// it to the exact leg reference the ordinal named, so the retained
	// quantifier's correlation set names the spliced legs — what the
	// partition/orientation machinery needs for the correlated probe. LAZY
	// references are deliberately LEFT ALONE: outside references address
	// legs by their own aliases (Go's box quantifier is NAMED by its
	// rightmost leaf), so after the merge pulls the legs up a name-model
	// read re-binds to the correct leg quantifier by construction —
	// substituting the RC under a lazy read would instead first-match a
	// duplicate bare name across the whole concat (silently the wrong
	// column). A subtree the helper cannot rebuild keeps its original
	// reference (dangling stays LOUD, never silently rebound).
	if len(rcByAlias) > 0 {
		mergedAliases := make(map[values.CorrelationIdentifier]struct{}, len(targets))
		for _, tgt := range targets {
			mergedAliases[quantifiers[tgt.idx].GetAlias()] = struct{}{}
		}
		for i, q := range newQuantifiers {
			if nq, ok := translateQuantifierCorrelations(q, mergedAliases, aliasMap, rcByAlias, call); ok {
				newQuantifiers[i] = nq
			}
		}
	}

	newPredicates := make([]predicates.QueryPredicate, 0, len(sel.GetPredicates())+len(pulledPredicates))
	for _, p := range sel.GetPredicates() {
		rp := p
		if len(aliasMap) > 0 {
			rp = predicates.RebasePredicate(rp, aliasMap)
		}
		if !tm.DefinesOnlyIdentities() {
			rp = translatePredicateCorrelations(rp, tm)
		}
		if len(rcByAlias) > 0 {
			rp = predicates.ReplaceValues(rp, cb)
		}
		newPredicates = append(newPredicates, rp)
	}
	newPredicates = append(newPredicates, pulledPredicates...)

	// Rebuild source aliases: drop merged aliases, splice in child aliases.
	var newAliases []string
	if srcAliases := sel.GetSourceAliases(); len(srcAliases) > 0 {
		for i := range quantifiers {
			if !mergedIdxSet[i] {
				if i < len(srcAliases) {
					newAliases = append(newAliases, srcAliases[i])
				}
				continue
			}
			var target mergeTarget
			for _, t := range targets {
				if t.idx == i {
					target = t
					break
				}
			}
			if childSel, ok := target.childExpr.(*expressions.SelectExpression); ok {
				newAliases = append(newAliases, childSel.GetSourceAliases()...)
			} else {
				// Non-SelectExpression child (e.g., LogicalFilter): preserve
				// the parent's alias for this position. The merged quantifier
				// replaces the parent's ForEach, so the alias that qualified
				// the parent's quantifier should carry over to the child's
				// quantifier(s).
				parentAlias := ""
				if i < len(srcAliases) {
					parentAlias = srcAliases[i]
				}
				for range target.childExpr.GetQuantifiers() {
					newAliases = append(newAliases, parentAlias)
				}
			}
		}
	}

	var merged *expressions.SelectExpression
	if len(newAliases) > 0 {
		merged = expressions.NewSelectExpressionWithJoinType(
			newResultValue, newQuantifiers, newPredicates, newAliases, sel.GetJoinType(),
		)
	} else {
		merged = expressions.NewSelectExpressionWithJoinType(
			newResultValue, newQuantifiers, newPredicates, nil, sel.GetJoinType(),
		)
	}
	call.Yield(merged)
}

var _ ExpressionRule = (*SelectMergeRule)(nil)

// translateQuantifierCorrelations rebuilds a RETAINED quantifier whose
// subtree references a merged-away alias FREELY (see the call site in
// OnMatch): the referenced SelectExpression's predicates and result value are
// translated through the merge's aliasMap/RC substitutions (recursively — a
// nested select may bury the reference another level down), re-memoized, and
// wrapped in a quantifier preserving the original's alias and flags
// (nullOnEmpty carries the dissolved-LEFT pad; strictSingle the scalar
// contract). ok=false when nothing references a merged alias or the subtree
// shape is not a SelectExpression — the caller keeps the original quantifier
// (a dangling reference stays LOUD, never silently rebound). The hit test is
// on the reference's FREE correlations (Java-aligned GetCorrelatedTo): a
// subtree whose OWN quantifier rebinds a merged name — the dissolved box's
// null-on-empty leg reuses the null-supplying leg's alias, which also names
// the box — is NOT correlated to the merge and must not be rewritten
// (rewriting it would capture the inner binding).
//
// The rebuild takes the FIRST SelectExpression member of the reference and
// re-memoizes only its translation — deliberate, not lossy: OnMatch YIELDS
// the merged select alongside the ORIGINAL, whose quantifier still ranges
// over the full multi-member group, so alternative members stay reachable
// through the pre-merge expression; the translated quantifier only needs
// one sound member to plan through.
func translateQuantifierCorrelations(
	q expressions.Quantifier,
	mergedAliases map[values.CorrelationIdentifier]struct{},
	aliasMap values.AliasMap,
	rcByAlias map[values.CorrelationIdentifier]values.Value,
	call *ExpressionRuleCall,
) (expressions.Quantifier, bool) {
	ref := q.GetRangesOver()
	if ref == nil {
		return q, false
	}
	hit := false
	for a := range ref.GetCorrelatedTo() {
		if _, merged := mergedAliases[a]; merged {
			hit = true
			break
		}
	}
	if !hit {
		return q, false
	}
	var newSel *expressions.SelectExpression
	for _, m := range ref.AllMembers() {
		sel, isSel := m.(*expressions.SelectExpression)
		if !isSel {
			continue
		}
		newSel = translateSelectCorrelations(sel, mergedAliases, aliasMap, rcByAlias, call)
		break
	}
	if newSel == nil {
		return q, false
	}
	newRef := call.MemoizeExpression(newSel)
	switch {
	case q.IsNullOnEmpty():
		return expressions.NamedForEachNullOnEmptyQuantifier(q.GetAlias(), newRef), true
	case q.IsStrictSingle():
		return expressions.NamedForEachStrictSingleQuantifier(q.GetAlias(), newRef), true
	case q.Kind() == expressions.QuantifierExistential:
		return expressions.NamedExistentialQuantifier(q.GetAlias(), newRef), true
	default:
		return expressions.NamedForEachQuantifier(q.GetAlias(), newRef), true
	}
}

// translateSelectCorrelations rebuilds a SelectExpression with every FREE
// merged-alias reference translated: predicates and result value through the
// merge's maps, and nested quantifiers recursively. Structure, source aliases
// and join type are preserved verbatim. SCOPING: an alias bound by THIS
// select's own quantifiers shadows the merge's substitution — its references
// bind locally, so it is stripped from the effective maps at this level and
// below (Go reuses human-readable aliases; Java's unique mints make this
// impossible there).
func translateSelectCorrelations(
	sel *expressions.SelectExpression,
	mergedAliases map[values.CorrelationIdentifier]struct{},
	aliasMap values.AliasMap,
	rcByAlias map[values.CorrelationIdentifier]values.Value,
	call *ExpressionRuleCall,
) *expressions.SelectExpression {
	shadowed := false
	for _, q := range sel.GetQuantifiers() {
		a := q.GetAlias()
		_, inAlias := aliasMap[a]
		_, inRC := rcByAlias[a]
		if inAlias || inRC {
			shadowed = true
			break
		}
	}
	effAliasMap, effRCByAlias, effMerged := aliasMap, rcByAlias, mergedAliases
	if shadowed {
		bound := make(map[values.CorrelationIdentifier]struct{}, len(sel.GetQuantifiers()))
		for _, q := range sel.GetQuantifiers() {
			bound[q.GetAlias()] = struct{}{}
		}
		effAliasMap = values.AliasMap{}
		for k, v := range aliasMap {
			if _, s := bound[k]; !s {
				effAliasMap[k] = v
			}
		}
		effRCByAlias = map[values.CorrelationIdentifier]values.Value{}
		for k, v := range rcByAlias {
			if _, s := bound[k]; !s {
				effRCByAlias[k] = v
			}
		}
		effMerged = map[values.CorrelationIdentifier]struct{}{}
		for k := range mergedAliases {
			if _, s := bound[k]; !s {
				effMerged[k] = struct{}{}
			}
		}
	}
	cb := bakedBoxRefCallback(effRCByAlias)
	changed := false
	newPreds := make([]predicates.QueryPredicate, len(sel.GetPredicates()))
	for i, p := range sel.GetPredicates() {
		np := p
		if len(effAliasMap) > 0 {
			np = predicates.RebasePredicate(np, effAliasMap)
		}
		np = predicates.ReplaceValues(np, cb)
		if np != p {
			changed = true
		}
		newPreds[i] = np
	}
	rv := sel.GetResultValue()
	if len(effAliasMap) > 0 {
		rv = values.RebaseValue(rv, effAliasMap)
	}
	rv = values.Replace(rv, cb)
	if rv != sel.GetResultValue() {
		changed = true
	}
	qs := sel.GetQuantifiers()
	newQs := make([]expressions.Quantifier, len(qs))
	for i, q := range qs {
		if nq, ok := translateQuantifierCorrelations(q, effMerged, effAliasMap, effRCByAlias, call); ok {
			newQs[i] = nq
			changed = true
		} else {
			newQs[i] = q
		}
	}
	if !changed {
		return nil
	}
	return expressions.NewSelectExpressionWithJoinType(rv, newQs, newPreds, sel.GetSourceAliases(), sel.GetJoinType())
}

// bakedBoxRefCallback returns a values.Replace callback (pre-order) that
// collapses BAKED references over a merged-away box alias through the box's
// result-value RC — the exact leg reference the ordinal named (a
// multi-accessor path collapses its root and fuses the suffix). LAZY
// references over the same alias are skipped whole (their QOV child is
// pointer-marked before the walk descends): a name-model read re-binds to
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
	// bare untyped RV).
	boxLevel := func(t values.Type, rcv *values.RecordConstructorValue) bool {
		rt, isRT := t.(*values.RecordType)
		if !isRT || rt == nil {
			return true
		}
		return len(rt.Fields) == len(rcv.Fields)
	}
	return func(node values.Value) values.Value {
		if _, s := skip[node]; s {
			return node
		}
		switch n := node.(type) {
		case *values.FieldValue:
			qov, isQOV := n.Child.(*values.QuantifiedObjectValue)
			if !isQOV {
				return node
			}
			rc, hit := rcByAlias[qov.Correlation]
			if !hit {
				return node
			}
			if rcv, isRC := rc.(*values.RecordConstructorValue); isRC && n.Resolved != nil && boxLevel(qov.Typ, rcv) {
				accs := n.Resolved.Accessors
				if len(accs) >= 1 && accs[0].Ordinal >= 0 && accs[0].Ordinal < len(rcv.Fields) && rcv.Fields[accs[0].Ordinal].Value != nil {
					slot := rcv.Fields[accs[0].Ordinal].Value
					if len(accs) == 1 {
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
					if slotFV, isFV := slot.(*values.FieldValue); isFV && slotFV.Resolved != nil && slotFV.Child != nil {
						fused := slotFV.Resolved.WithSuffix(&values.FieldPath{Accessors: accs[1:]})
						out := &values.FieldValue{Field: fused.Last().Field, Typ: n.Typ, Child: slotFV.Child, Resolved: fused}
						mark(out)
						return out
					}
				}
			}
			// Lazy, leg-level, or non-collapsible reference: keep it INTACT,
			// including its QOV child — it re-binds by name to the pulled-up
			// leg quantifier of the same name.
			mark(n.Child)
			return node
		case *values.QuantifiedObjectValue:
			if rc, hit := rcByAlias[n.Correlation]; hit {
				if rcv, isRC := rc.(*values.RecordConstructorValue); isRC && !boxLevel(n.Typ, rcv) {
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
