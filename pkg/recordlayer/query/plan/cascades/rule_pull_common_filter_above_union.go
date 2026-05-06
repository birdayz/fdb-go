package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// PullCommonFilterAboveUnionRule combines a Union whose every child
// is a LogicalFilter with the SAME predicate list into a single
// Filter above the Union. The reverse of PushFilterThroughUnion.
//
//	Union(Filter([P], A), Filter([P], B), ...)
//	→
//	Filter([P], Union(A, B, ...))
//
// Soundness: Union(Filter(P, A), Filter(P, B)) admits the same rows
// as Filter(P, Union(A, B)) — distributivity of filter over union.
// All children must share the SAME predicate list (compared by
// Explain text); a mixed-predicate set can't be pulled in one step.
//
// Optimization argument: collapsing N filters into 1 reduces the
// memo's operator count and gives downstream rules a single Filter
// to optimise rather than N independent ones.
//
// Termination via SemanticEquals fallback (the rule produces a
// fresh-Reference Union, then wraps with Filter; both new wrappers
// dedup against equivalent existing alternatives).
type PullCommonFilterAboveUnionRule struct {
	matcher matching.BindingMatcher
}

// NewPullCommonFilterAboveUnionRule constructs the rule.
func NewPullCommonFilterAboveUnionRule() *PullCommonFilterAboveUnionRule {
	return &PullCommonFilterAboveUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("logical_union"),
	}
}

// Matcher returns the pattern.
func (r *PullCommonFilterAboveUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when every child Quantifier ranges over a Filter
// AND every Filter has the SAME predicate list (Explain-equal).
func (r *PullCommonFilterAboveUnionRule) OnMatch(call *ExpressionRuleCall) {
	u := matching.Get[*expressions.LogicalUnionExpression](call.Bindings, r.matcher)
	children := u.GetQuantifiers()
	if len(children) < 2 {
		return // single-child case: UnionSingletonElim handles it
	}
	commonPreds, allFilters := commonFilterPredicates(children)
	if !allFilters || commonPreds == nil {
		return
	}
	// Build new Union over each filter's INNER (skip the Filter wrapper).
	newQs := make([]expressions.Quantifier, 0, len(children))
	for _, q := range children {
		f := q.GetRangesOver().Get().(*expressions.LogicalFilterExpression)
		newQs = append(newQs, f.GetInner())
	}
	newUnion := expressions.NewLogicalUnionExpression(newQs)
	newUnionQ := expressions.ForEachQuantifier(call.MemoizeExpression(newUnion))
	call.Yield(expressions.NewLogicalFilterExpression(commonPreds, newUnionQ))
}

// commonFilterPredicates returns the shared predicate list if every
// child Quantifier ranges over a LogicalFilter with the same predicate
// list (Explain-equal), else (nil, false). The second return is true
// iff every child was a Filter (independent of whether they all
// match) — used to disambiguate "non-Filter child" from "Filter
// children with different predicates".
func commonFilterPredicates(children []expressions.Quantifier) ([]predicates.QueryPredicate, bool) {
	if len(children) == 0 {
		return nil, false
	}
	first, ok := children[0].GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		return nil, false
	}
	firstKey := predicateListKey(first.GetPredicates())
	for i := 1; i < len(children); i++ {
		f, ok := children[i].GetRangesOver().Get().(*expressions.LogicalFilterExpression)
		if !ok {
			return nil, false
		}
		if predicateListKey(f.GetPredicates()) != firstKey {
			return nil, true // all-Filters but predicates differ
		}
	}
	return first.GetPredicates(), true
}

// predicateListKey produces a deterministic Explain-text key for a
// QueryPredicate list, used for set comparison across union children.
func predicateListKey(ps []predicates.QueryPredicate) string {
	if len(ps) == 0 {
		return ""
	}
	out := ""
	for i, p := range ps {
		if i > 0 {
			out += "|"
		}
		out += p.Explain()
	}
	return out
}

var _ ExpressionRule = (*PullCommonFilterAboveUnionRule)(nil)
