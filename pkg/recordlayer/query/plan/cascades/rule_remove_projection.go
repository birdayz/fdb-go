package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// RemoveProjectionRule removes a LogicalProjectionExpression when the
// inner is already a physical plan (not a fetch). The projection is
// unnecessary because the inner plan already produces the correct row
// shape. The rule simply yields the inner plan, dropping the
// projection wrapper.
//
// Mirrors Java's RemoveProjectionRule.
type RemoveProjectionRule struct {
	matcher matching.BindingMatcher
}

func NewRemoveProjectionRule() *RemoveProjectionRule {
	return &RemoveProjectionRule{
		matcher: NewExpressionMatcher[*physicalProjectionWrapper]("phys_projection_remove"),
	}
}

func (r *RemoveProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *RemoveProjectionRule) OnMatch(call *ImplementationRuleCall) {
	projW := matching.Get[*physicalProjectionWrapper](call.Bindings, r.matcher)

	innerRef := projW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Java's RemoveProjectionRule removes projections over physical
	// plans. Only safe when the projection is an identity mapping
	// (all columns passed through unchanged). A projection that
	// selects a subset of columns (like SELECT status FROM orders)
	// changes the output shape and MUST NOT be removed — it affects
	// DISTINCT, GROUP BY, and other shape-dependent operators above.
	if !projW.plan.IsIdentity() {
		return
	}

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}

	call.Yield(innerExpr)
}

var _ ImplementationRule = (*RemoveProjectionRule)(nil)
