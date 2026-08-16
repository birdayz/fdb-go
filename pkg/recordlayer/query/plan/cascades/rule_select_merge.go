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
		for _, member := range childRef.AllMembers() {
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
			// A DISSOLVED outer-join box — RewriteOuterJoinRule's INNER select
			// whose null-supplying leg rides a NULL-ON-EMPTY quantifier — is the
			// SAME outer-join box as the arm above, with the outer-join edge
			// moved from the JoinType onto the quantifier flag. Merging it up
			// into a ForEach-only parent produces a flat noe-carrying select
			// that Go's PLANNING cannot re-partition (PartitionSelectRule's
			// positional-merge arm declines to collapse a null-on-empty
			// quantifier into a lower, and a dissolved box's flat form has NO
			// select-level predicates left to connect a lower) — so when the
			// REWRITING phase then prunes the child's group to that flat member
			// as its canonical seed, a nested outer box (`(a LEFT b) LEFT c`)
			// under any enclosing select strands unimplementable ("best
			// expression is not a physical plan"). Java DOES flatten this form
			// and re-derives the join from the flat seed (its
			// PartitionSelectRule collapses defaultOnEmpty quantifiers into
			// positional lowers and its NLJ implements any 2-quantifier
			// select); until Go's partitioning reaches that parity, the
			// dissolved box stays nested under a ForEach-only parent — the same
			// hard barrier as the un-dissolved arm, so the binary NLJ/FlatMap
			// implementation over the nested form (the route the un-enclosed
			// nested box already plans through) survives the rewrite prune.
			// An EXISTENTIAL-carrying parent is EXEMPT: its flat form never
			// reaches PartitionSelectRule (≤1 existential returns; ≥2 peel),
			// and the [ForEach×N, Existential] implementer handles noe legs
			// via buildCorrelatedFlatMapPlan — the correlated DefaultOnEmpty
			// step-1 that the LEFT+EXISTS plan-shape pins require (declining
			// there degraded step-1 to a materialized LEFT NLJ).
			// Never wrong rows — strictly a narrower merge.
			if isChildSel && !hasExistential {
				childHasNullOnEmpty := false
				for _, cq := range childSel.GetQuantifiers() {
					if cq.IsNullOnEmpty() {
						childHasNullOnEmpty = true
						break
					}
				}
				if childHasNullOnEmpty {
					continue
				}
			}
			// The chained-unnest correlation barrier: decline to merge a
			// ForEach target when a RETAINED
			// sibling quantifier is FREE-correlated to it AND the target is a
			// LATERAL-UNNEST first link. A chained `FROM t, t.arr AS x, x.sub AS
			// y` reuses the alias `x` for both the first unnest's output (this
			// target) and the second unnest's Explode collection (a retained
			// sibling correlated to `x`); flattening the first unnest up would
			// strand that collection on the merged-away alias. Java never
			// flattens a lateral chain either (generateCorrelatedFieldAccess
			// nests them). Keep the nested FlatMap-over-FlatMap.
			//
			// The barrier covers BOTH result-value shapes of the first link:
			//   - NON-SEED first link (childRefResultIsNonSeed): the
			//     retained-sibling rebase runs only for positional-seed children
			//     (rcByAlias empty for a non-seed child), so the Explode arm
			//     below would not fire — merging strands the collection.
			//   - ORDINAL first link (childRefIsPositionalUnnestSelect):
			//     the chained link takes an ordinal seed, so its
			//     first-link child is a POSITIONAL unnest select. The
			//     positional-seed rebase DOES fire but cannot compose the chained
			//     collection's fused ofOrdinal+name suffix (the deferred ordinal
			//     compose direction), leaving the flattened root select
			//     unimplementable. Keep it nested — Java's shape — where each
			//     link implements as its own FlatMap. This is NARROWER than a
			//     positional-child check: a GATED BOX child (no Explode
			//     quantifier of its own) still merges via the rebase, unaffected.
			// Conservative — worst case a missed flattening, never wrong rows.
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

	// Build alias rebase map (single-quantifier children) and
	// TranslationMap (multi-quantifier children). rcByAlias mirrors the
	// TranslationMap's entries as plain data so the retained-quantifier
	// subtree translation below can rebuild SCOPED maps per nesting level
	// (a locally-rebound alias shadows the merge's substitution).
	aliasMapBuilder := NewAliasMapBuilder()
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
			if !aliasMapBuilder.Put(q.GetAlias(), childQs[0].GetAlias()) {
				return
			}
		} else if len(childQs) > 1 {
			// Multi-quantifier child (e.g., Select with 2+ sources):
			// the parent's alias must be replaced with the child's
			// result value. TWO regimes by the child RV's shape:
			//   - POSITIONAL ordinal-seed RC (a dissolved gated box): the
			//     SURGICAL substitution (rcByAlias → bakedBoxRefCallback) —
			//     baked refs collapse through the RC to the exact leg
			//     reference; LAZY refs stay intact and re-bind by name to
			//     the pulled-up leg quantifier (the box is NAMED by its
			//     rightmost leaf, so the name stays bound). A blanket
			//     TranslationMap substitution would put the dup-bare-named
			//     concat under a lazy read, where the bare name matches
			//     TWO of the concat's columns: every by-name lookup on
			//     the bake path declines on that ambiguity, so the
			//     reference never re-binds and the plan dies loud at
			//     evaluation instead of staying bound to its leg column.
			//   - NON-SEED child: the TranslationMap
			//     substitution — the child alias disappears with the RV
			//     substituted in place.
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
	aliasMap := aliasMapBuilder.Build()
	valueAliasMap, err := aliasMap.ForwardMap()
	if err != nil {
		call.Fail(err)
		return
	}

	// Rebase the parent's result value and predicates.
	newResultValue := sel.GetResultValue()
	if !aliasMap.IsEmpty() {
		newResultValue, err = values.RebaseValueChecked(newResultValue, valueAliasMap)
		if err != nil {
			call.Fail(err)
			return
		}
	}
	// Apply TranslationMap for non-seed multi-quantifier children, and the
	// surgical baked-collapse callback for positional-seed children (lazy
	// refs re-bind by name to the pulled-up leg — see the regime comment at
	// the multi-quantifier arm above).
	tm := tmBuilder.Build()
	if !tm.DefinesOnlyIdentities() {
		// A merge that cannot re-express the result value against the child's
		// program does not happen at all. Publishing the untranslated value
		// would leave references to an alias this rewrite is about to dissolve.
		translatedResult, ok := translateValueCorrelations(newResultValue, tm)
		if !ok {
			return
		}
		newResultValue = translatedResult
	}
	cb := bakedBoxRefCallback(rcByAlias)
	if len(rcByAlias) > 0 {
		newResultValue = values.Replace(newResultValue, cb)
	}

	// RETAINED quantifiers whose subtrees hold BAKED
	// references over a MERGED-AWAY alias get those references translated
	// through the merged child's result value — Java's
	// Quantifier.translateCorrelations, which Go's merge never needed until
	// the dissolved-LEFT fold produced the first stranded case: a
	// box-level-baked reference (an enclosing scope's ON conjunct addressing
	// a buried column) addresses the box quantifier POSITIONALLY; when the
	// box's child select merges up, that positional read would
	// re-bind by name to the same-named pulled-up LEG with the wrong type —
	// the divergent-baked-types class. Collapsing through the RC resolves
	// it to the exact leg reference the ordinal named, so the retained
	// quantifier's correlation set names the spliced legs — what the
	// partition/orientation machinery needs for the correlated probe. LAZY
	// references are deliberately LEFT ALONE: outside references address
	// legs by their own aliases (Go's box quantifier is NAMED by its
	// rightmost leaf), so after the merge pulls the legs up a lazy
	// read re-binds to the correct leg quantifier by construction —
	// substituting the RC under a lazy read would instead put a duplicate
	// bare name in front of the whole concat, which no by-name lookup will
	// resolve — they decline on the ambiguity, so the reference is stranded
	// lazy and fails loud at evaluation rather than reaching its leg
	// column. A subtree the helper cannot rebuild keeps its original
	// reference (dangling stays LOUD, never silently rebound).
	if len(rcByAlias) > 0 {
		mergedAliases := make(map[values.CorrelationIdentifier]struct{}, len(targets))
		for _, tgt := range targets {
			mergedAliases[quantifiers[tgt.idx].GetAlias()] = struct{}{}
		}
		for i, q := range newQuantifiers {
			nq, ok, err := translateQuantifierCorrelations(q, mergedAliases, aliasMap, rcByAlias, call)
			if err != nil {
				call.Fail(err)
				return
			}
			if ok {
				newQuantifiers[i] = nq
			}
		}
	}

	newPredicates := make([]predicates.QueryPredicate, 0, len(sel.GetPredicates())+len(pulledPredicates))
	for _, p := range sel.GetPredicates() {
		rp := p
		if !aliasMap.IsEmpty() {
			// CHECKED: the error-less spelling returns nil on failure, and this
			// merged select would then carry a predicate that silently is not
			// there — the merge widens the result rather than declining.
			rebased, rerr := predicates.RebasePredicateChecked(rp, valueAliasMap)
			if rerr != nil {
				call.Fail(rerr)
				return
			}
			rp = rebased
		}
		if !tm.DefinesOnlyIdentities() {
			translated, ok := translatePredicateCorrelations(rp, tm)
			if !ok {
				return
			}
			rp = translated
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
		merged, err = expressions.NewSelectExpressionWithJoinType(
			newResultValue, newQuantifiers, newPredicates, newAliases, sel.GetJoinType(),
		)
	} else {
		merged, err = expressions.NewSelectExpressionWithJoinType(
			newResultValue, newQuantifiers, newPredicates, nil, sel.GetJoinType(),
		)
	}
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(merged)
}

var _ ExpressionRule = (*SelectMergeRule)(nil)

// translateQuantifierCorrelations rebuilds a RETAINED quantifier whose
// subtree references a merged-away alias FREELY (see the call site in
// OnMatch): the first rebuildable member of the referenced group — a
// SelectExpression (predicates + result value translated through the merge's
// aliasMap/RC substitutions, recursively) or an ExplodeExpression (its
// collection translated, the lateral-unnest arm below) — is translated,
// re-memoized, and wrapped in a quantifier preserving the original's alias
// and flags (nullOnEmpty carries the dissolved-LEFT pad; strictSingle the
// scalar contract). ok=false when nothing references a merged alias or no
// member is a rebuildable shape — the caller keeps the original quantifier
// (a dangling reference stays LOUD, never silently rebound). The hit test is
// on the reference's FREE correlations (Java-aligned GetCorrelatedTo): a
// subtree whose OWN quantifier rebinds a merged name — the dissolved box's
// null-on-empty leg reuses the null-supplying leg's alias, which also names
// the box — is NOT correlated to the merge and must not be rewritten
// (rewriting it would capture the inner binding).
//
// The rebuild takes the FIRST rebuildable member and re-memoizes only its
// translation — deliberate, not lossy: OnMatch YIELDS the merged select
// alongside the ORIGINAL, whose quantifier still ranges over the full
// multi-member group, so alternative members stay reachable through the
// pre-merge expression; the translated quantifier only needs one sound
// member to plan through.
func translateQuantifierCorrelations(
	q expressions.Quantifier,
	mergedAliases map[values.CorrelationIdentifier]struct{},
	aliasMap *AliasMap,
	rcByAlias map[values.CorrelationIdentifier]values.Value,
	call *ExpressionRuleCall,
) (expressions.Quantifier, bool, error) {
	ref := q.GetRangesOver()
	if ref == nil {
		return q, false, nil
	}
	hit := false
	for a := range ref.GetCorrelatedTo() {
		if _, merged := mergedAliases[a]; merged {
			hit = true
			break
		}
	}
	if !hit {
		return q, false, nil
	}
	var newMember expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		switch me := m.(type) {
		case *expressions.SelectExpression:
			ns, err := translateSelectCorrelations(me, mergedAliases, aliasMap, rcByAlias, call)
			if err != nil {
				return q, false, err
			}
			if ns != nil {
				newMember = ns
			}
		case *expressions.ExplodeExpression:
			// A lateral unnest's Explode sibling whose COLLECTION baked-
			// references a merged-away box alias gets that reference
			// translated the same way a SelectExpression member would
			// (Java: Quantifier.translateCorrelations reaches
			// ExplodeExpression.translateCorrelations). DEFENSIVE, not a
			// live-bug fix: no CURRENT path makes a box leg mergeable while
			// a sibling Explode references it — outer boxes are opaque to
			// this rule (the ChildrenAsSet guard above) and inner boxes are
			// pre-flattened by legsOfGatedJoin before they reach the memo as
			// a mergeable child. The arm exists so that if a future rewrite
			// DOES make box legs mergeable, the stranded-collection shape is
			// handled rather than silently dangling; it is pinned white-box
			// by TestSelectMergeRule_TranslatesExplodeSiblingCollection
			// (a hand-built memo that forces the merge), which is its only
			// exercise today.
			cb := bakedBoxRefCallback(rcByAlias)
			valueAliasMap, err := aliasMap.ForwardMap()
			if err != nil {
				return q, false, err
			}
			col, err := values.RebaseValueChecked(me.GetCollectionValue(), valueAliasMap)
			if err != nil {
				return q, false, err
			}
			col = values.Replace(col, cb)
			if col != me.GetCollectionValue() {
				newMember, err = expressions.NewExplodeExpressionWithOrdinality(col, me.GetWithOrdinality())
				if err != nil {
					return q, false, err
				}
			}
		}
		if newMember != nil {
			break
		}
	}
	if newMember == nil {
		return q, false, nil
	}
	newRef := call.MemoizeExpression(newMember)
	switch {
	case q.IsNullOnEmpty():
		return expressions.NamedForEachNullOnEmptyQuantifier(q.GetAlias(), newRef), true, nil
	case q.IsStrictSingle():
		return expressions.NamedForEachStrictSingleQuantifier(q.GetAlias(), newRef), true, nil
	case q.Kind() == expressions.QuantifierExistential:
		return expressions.NamedExistentialQuantifier(q.GetAlias(), newRef), true, nil
	default:
		return expressions.NamedForEachQuantifier(q.GetAlias(), newRef), true, nil
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
	aliasMap *AliasMap,
	rcByAlias map[values.CorrelationIdentifier]values.Value,
	call *ExpressionRuleCall,
) (*expressions.SelectExpression, error) {
	shadowed := false
	for _, q := range sel.GetQuantifiers() {
		a := q.GetAlias()
		inAlias := aliasMap.ContainsSource(a)
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
		effBuilder := NewAliasMapBuilder()
		for _, k := range aliasMap.Sources() {
			if _, s := bound[k]; !s {
				if !effBuilder.Put(k, aliasMap.GetTarget(k)) {
					return nil, nil
				}
			}
		}
		effAliasMap = effBuilder.Build()
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
	valueAliasMap, err := effAliasMap.ForwardMap()
	if err != nil {
		return nil, err
	}
	cb := bakedBoxRefCallback(effRCByAlias)
	changed := false
	newPreds := make([]predicates.QueryPredicate, len(sel.GetPredicates()))
	for i, p := range sel.GetPredicates() {
		np := p
		if !effAliasMap.IsEmpty() {
			// CHECKED — see the sibling loop in OnMatch: a nil here is a
			// predicate that silently is not there.
			rebased, rerr := predicates.RebasePredicateChecked(np, valueAliasMap)
			if rerr != nil {
				return nil, rerr
			}
			np = rebased
		}
		np = predicates.ReplaceValues(np, cb)
		if np != p {
			changed = true
		}
		newPreds[i] = np
	}
	rv := sel.GetResultValue()
	if !effAliasMap.IsEmpty() {
		var rebaseErr error
		rv, rebaseErr = values.RebaseValueChecked(rv, valueAliasMap)
		if rebaseErr != nil {
			return nil, rebaseErr
		}
	}
	rv = values.Replace(rv, cb)
	if rv != sel.GetResultValue() {
		changed = true
	}
	qs := sel.GetQuantifiers()
	newQs := make([]expressions.Quantifier, len(qs))
	for i, q := range qs {
		nq, ok, err := translateQuantifierCorrelations(q, effMerged, effAliasMap, effRCByAlias, call)
		if err != nil {
			return nil, err
		}
		if ok {
			newQs[i] = nq
			changed = true
		} else {
			newQs[i] = q
		}
	}
	if !changed {
		return nil, nil
	}
	return expressions.NewSelectExpressionWithJoinType(rv, newQs, newPreds, sel.GetSourceAliases(), sel.GetJoinType())
}

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
				// A SOURCE-RELATIVE baked reference's ordinal is relative to its
				// OWN leg's row, NOT the box's concatenated RC — and the box is
				// NAMED by its rightmost leg, so a leg-addressed reference
				// (QOV(E).ID#0) arrives under the very alias being collapsed.
				// Resolving it by the RAW ordinal against the concat picks the
				// FIRST leg's slot (D.ID for ord 0) — the wrong leg: a bug once
				// turned `e.id IS NULL` into `d.id IS NULL`, mis-partitioning
				// below the null-extension so the LEFT-join anti-join returned
				// zero rows. Re-base by the reference's OWN leg exactly like the
				// values.Replace collapse (LegAwareRootOrdinal): match the seed
				// field over the SAME correlation at the leg-local ordinal.
				// Keyed on the ROOT's leg-relativity (RootIsLegRelativeUnpinned),
				// NOT the accessor count: a FUSED unpinned path (fused by an earlier
				// merge round) keeps its leg-relative ROOT and MUST be rebased too —
				// the raw collapse would fuse its suffix (the arm below) onto the
				// WRONG leg's seed field for a non-first leg. Only FrontierPinned
				// bakes keep
				// the raw collapse — their ordinal IS box-relative by
				// construction.
				if !path.IsFrontierPinned() && len(ordinals) >= 1 {
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

// childRefResultIsNonSeed reports whether a merge candidate's child result
// value is NOT a positional ordinal seed. A positional seed
// child routes through the rcByAlias/bakedBoxRefCallback rebase (which handles
// a retained Explode sibling); a non-seed child does not, so the chained
// barrier applies only to non-seed children.
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
