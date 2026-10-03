package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// Go's query block is a LogicalProjection over its FROM/WHERE whose consumers
// read the block's legs by alias, so blocks are never merged into their parent
// the way Java's SelectMergeRule merges block Selects. These two rules carry a
// parent's predicates into a block instead, re-expressed over the projected row
// (pushThroughProjection), so the block's access paths see them.

// PushFilterThroughProjectionRule is PredicatePushDownRule for a WHERE over a
// single block, which Go states as a LogicalFilter rather than a Select:
//
//	Filter(P, A → Projection(vals, B → X))  →  Projection(vals, B → Filter(P[A := vals], B → X))
type PushFilterThroughProjectionRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughProjectionRule constructs the rule.
func NewPushFilterThroughProjectionRule() *PushFilterThroughProjectionRule {
	return &PushFilterThroughProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter").WithRootPredicate(
			func(f *expressions.LogicalFilterExpression) bool {
				return len(f.GetPredicates()) > 0 && rangesOverProjection(f.GetInner())
			}),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch yields each projection member under the filter with every predicate
// moved inside.
func (r *PushFilterThroughProjectionRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	inner := f.GetInner()
	if inner.Kind() != expressions.QuantifierForEach || inner.IsNullOnEmpty() || inner.IsStrictSingle() ||
		len(f.GetPredicates()) == 0 || inner.GetRangesOver() == nil {
		return
	}
	for _, member := range inner.GetRangesOver().AllMembers() {
		projection, ok := member.(*expressions.LogicalProjectionExpression)
		if !ok {
			continue
		}
		pushed, err := pushThroughProjection(call, f.GetPredicates(), inner, projection, false)
		if err != nil {
			call.Fail(err)
			return
		}
		if pushed != nil {
			call.Yield(pushed)
		}
	}
}

// PushPredicatesThroughProjectionRule carries a join predicate that
// PartitionBinarySelectRule leaves on a block leg into the block during
// PLANNING. Java partitions such a predicate straight onto a base quantifier of
// its merged Select, where candidate matching sees it:
//
//	Select(P, A → Projection(vals, B → X))  →  Select(A → Projection(vals, B → Select(P[A := vals], B → X)))
type PushPredicatesThroughProjectionRule struct {
	matcher matching.BindingMatcher
}

// NewPushPredicatesThroughProjectionRule constructs the rule.
func NewPushPredicatesThroughProjectionRule() *PushPredicatesThroughProjectionRule {
	return &PushPredicatesThroughProjectionRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("select").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool {
				if len(sel.GetPredicates()) == 0 || !sel.ChildrenAsSet() {
					return false
				}
				for _, q := range sel.GetQuantifiers() {
					if rangesOverProjection(q) {
						return true
					}
				}
				return false
			}),
	}
}

// Matcher returns the pattern.
func (r *PushPredicatesThroughProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch pushes the predicates that read no sibling quantifier into one block
// child, one quantifier per firing as PredicatePushDownRule does.
func (r *PushPredicatesThroughProjectionRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	if !sel.ChildrenAsSet() || len(sel.GetPredicates()) == 0 {
		return
	}
	quantifiers := sel.GetQuantifiers()
	for qIdx, pushQ := range quantifiers {
		if pushQ.Kind() != expressions.QuantifierForEach || pushQ.IsNullOnEmpty() || pushQ.IsStrictSingle() ||
			pushQ.GetRangesOver() == nil {
			continue
		}
		var pushable, fixed []predicates.QueryPredicate
		for _, pred := range sel.GetPredicates() {
			if readsSibling(pred, quantifiers, qIdx) {
				fixed = append(fixed, pred)
			} else {
				pushable = append(pushable, pred)
			}
		}
		if len(pushable) == 0 {
			continue
		}
		yielded := false
		for _, member := range pushQ.GetRangesOver().AllMembers() {
			projection, ok := member.(*expressions.LogicalProjectionExpression)
			if !ok {
				continue
			}
			pushed, err := pushThroughProjection(call, pushable, pushQ, projection, true)
			if err != nil {
				call.Fail(err)
				return
			}
			if pushed == nil {
				continue
			}
			newQuantifiers := make([]expressions.Quantifier, len(quantifiers))
			copy(newQuantifiers, quantifiers)
			newQuantifiers[qIdx] = expressions.NamedForEachQuantifier(pushQ.GetAlias(), call.MemoizeExpression(pushed))
			newSel, err := expressions.NewSelectExpressionWithJoinType(
				sel.GetResultValue(), newQuantifiers, fixed, sel.GetSourceAliases(), sel.GetJoinType())
			if err != nil {
				call.Fail(err)
				return
			}
			call.Yield(newSel)
			yielded = true
		}
		if yielded {
			return
		}
	}
}

// rangesOverProjection reports whether q is a plain ForEach edge over a query
// block.
func rangesOverProjection(q expressions.Quantifier) bool {
	if q.Kind() != expressions.QuantifierForEach || q.IsNullOnEmpty() || q.IsStrictSingle() || q.GetRangesOver() == nil {
		return false
	}
	for _, member := range q.GetRangesOver().AllMembers() {
		if _, ok := member.(*expressions.LogicalProjectionExpression); ok {
			return true
		}
	}
	return false
}

// readsSibling reports whether pred reads any quantifier of the expression
// other than the one at idx.
func readsSibling(pred predicates.QueryPredicate, quantifiers []expressions.Quantifier, idx int) bool {
	correlated := predicates.GetCorrelatedToOfPredicate(pred)
	for j, q := range quantifiers {
		if j == idx {
			continue
		}
		if _, reads := correlated[q.GetAlias()]; reads {
			return true
		}
	}
	return false
}

var (
	_ ExpressionRule = (*PushFilterThroughProjectionRule)(nil)
	_ ExpressionRule = (*PushPredicatesThroughProjectionRule)(nil)
)
