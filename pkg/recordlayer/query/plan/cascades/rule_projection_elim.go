package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ProjectionElimRule eliminates a LogicalProjection whose projection
// list is exactly the inner Quantifier's flowed object value — i.e.
// `SELECT * FROM t` shapes that survive parsing but represent no
// computation.
//
// Pattern:
//
//	Projection([QOV(alias)]) over QuantifierForEach(alias) over X
//	→
//	X
//
// Detection: the projection list has exactly one Value, and that
// Value is a QuantifiedObjectValue whose CorrelationIdentifier matches
// the inner Quantifier's alias.
//
// Java equivalent: the planner doesn't have a dedicated rule for
// this; the Memo cost model would naturally prefer the single-X
// member over the wrapped-in-Projection version. Seed implements it
// directly so the optimiser produces a concretely-simpler tree.
type ProjectionElimRule struct {
	matcher matching.BindingMatcher
}

// NewProjectionElimRule constructs the rule.
func NewProjectionElimRule() *ProjectionElimRule {
	return &ProjectionElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

// Matcher returns the pattern.
func (r *ProjectionElimRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the projection is an identity over the inner
// quantifier; yields the inner expression directly.
func (r *ProjectionElimRule) OnMatch(call *ExpressionRuleCall) {
	p := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	pvs := p.GetProjectedValues()
	if len(pvs) != 1 {
		return
	}
	qov, ok := pvs[0].(*values.QuantifiedObjectValue)
	if !ok {
		return
	}
	innerAlias := p.GetInner().GetAlias()
	if qov.Correlation != innerAlias {
		return
	}
	call.Yield(p.GetInner().GetRangesOver().Get())
}

var _ ExpressionRule = (*ProjectionElimRule)(nil)
