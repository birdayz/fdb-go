package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// UnionMergeRule flattens a LogicalUnion whose any child Quantifier
// ranges over another LogicalUnion. The flattened result has all
// inner-Union children promoted to siblings of the outer-Union's
// other children.
//
//	Union(A, Union(B, C), D)
//	→
//	Union(A, B, C, D)
//
// SQL-equivalent: UNION ALL is associative, so chained nested
// UNION ALL collapses without semantic change. Java's planner would
// derive this via cost preference for fewer operator nodes; the seed
// implements it directly.
//
// Fires once per OnMatch — the first inner-Union child triggers a
// rewrite that promotes ALL inner-Union children at once. If multiple
// children are themselves Unions, repeated rule fires (driven by the
// planner's iteration loop) collapse them in turn.
type UnionMergeRule struct {
	matcher matching.BindingMatcher
}

// NewUnionMergeRule constructs the rule.
func NewUnionMergeRule() *UnionMergeRule {
	return &UnionMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("logical_union"),
	}
}

// Matcher returns the pattern.
func (r *UnionMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch examines each child Quantifier; if any ranges over a
// LogicalUnion, yields a flattened Union with that inner-Union's
// children promoted in place. Yields nothing if no child is a Union.
func (r *UnionMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalUnionExpression](call.Bindings, r.matcher)
	flat := flattenUnionChildren(outer.GetQuantifiers())
	// If flatten didn't change the child list, the rule declines.
	if len(flat) == len(outer.GetQuantifiers()) {
		return
	}
	call.Yield(expressions.NewLogicalUnionExpression(flat))
}

// flattenUnionChildren walks `qs`, replacing any Quantifier ranging
// over a LogicalUnionExpression with that inner Union's children.
func flattenUnionChildren(qs []expressions.Quantifier) []expressions.Quantifier {
	out := make([]expressions.Quantifier, 0, len(qs))
	for _, q := range qs {
		inner := q.GetRangesOver().Get()
		if u, ok := inner.(*expressions.LogicalUnionExpression); ok {
			out = append(out, u.GetQuantifiers()...)
		} else {
			out = append(out, q)
		}
	}
	return out
}

var _ ExpressionRule = (*UnionMergeRule)(nil)
