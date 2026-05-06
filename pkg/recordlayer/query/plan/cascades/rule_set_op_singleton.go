package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// UnionSingletonElimRule eliminates a LogicalUnion with exactly one
// child — UNION ALL of a single input is just that input.
//
//	Union([Q]) → inner of Q
type UnionSingletonElimRule struct {
	matcher matching.BindingMatcher
}

// NewUnionSingletonElimRule constructs the rule.
func NewUnionSingletonElimRule() *UnionSingletonElimRule {
	return &UnionSingletonElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("logical_union"),
	}
}

// Matcher returns the pattern.
func (r *UnionSingletonElimRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the union has exactly one child.
func (r *UnionSingletonElimRule) OnMatch(call *ExpressionRuleCall) {
	u := matching.Get[*expressions.LogicalUnionExpression](call.Bindings, r.matcher)
	qs := u.GetQuantifiers()
	if len(qs) != 1 {
		return
	}
	call.Yield(qs[0].GetRangesOver().Get())
}

var _ ExpressionRule = (*UnionSingletonElimRule)(nil)

// IntersectionSingletonElimRule eliminates a LogicalIntersection with
// exactly one child — INTERSECTION of a single input is just that
// input.
//
//	Intersection([Q]) → inner of Q
type IntersectionSingletonElimRule struct {
	matcher matching.BindingMatcher
}

// NewIntersectionSingletonElimRule constructs the rule.
func NewIntersectionSingletonElimRule() *IntersectionSingletonElimRule {
	return &IntersectionSingletonElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalIntersectionExpression]("logical_intersection"),
	}
}

// Matcher returns the pattern.
func (r *IntersectionSingletonElimRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the intersection has exactly one child.
func (r *IntersectionSingletonElimRule) OnMatch(call *ExpressionRuleCall) {
	x := matching.Get[*expressions.LogicalIntersectionExpression](call.Bindings, r.matcher)
	qs := x.GetQuantifiers()
	if len(qs) != 1 {
		return
	}
	call.Yield(qs[0].GetRangesOver().Get())
}

var _ ExpressionRule = (*IntersectionSingletonElimRule)(nil)
