package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
			if wp, ok := member.(expressions.RelationalExpressionWithPredicates); ok {
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
	// TranslationMap (multi-quantifier children).
	aliasMap := values.AliasMap{}
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
			// result value via TranslationMap.
			childResultValue := target.childExpr.GetResultValue()
			capturedResult := childResultValue
			parentAlias := q.GetAlias()
			tmBuilder.When(parentAlias).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
				return capturedResult
			})
		}

		newQuantifiers = append(newQuantifiers, childQs...)
		pulledPredicates = append(pulledPredicates, target.child.GetPredicates()...)
	}

	// Rebase the parent's result value and predicates.
	newResultValue := sel.GetResultValue()
	if len(aliasMap) > 0 {
		newResultValue = values.RebaseValue(newResultValue, aliasMap)
	}
	// Apply TranslationMap for multi-quantifier children.
	tm := tmBuilder.Build()
	if !tm.DefinesOnlyIdentities() {
		newResultValue = translateValueCorrelations(newResultValue, tm, nil)
	}

	newPredicates := make([]predicates.QueryPredicate, 0, len(sel.GetPredicates())+len(pulledPredicates))
	for _, p := range sel.GetPredicates() {
		rp := p
		if len(aliasMap) > 0 {
			rp = predicates.RebasePredicate(rp, aliasMap)
		}
		if !tm.DefinesOnlyIdentities() {
			rp = translatePredicateCorrelations(rp, tm, nil)
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
				for range target.childExpr.GetQuantifiers() {
					newAliases = append(newAliases, "")
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
