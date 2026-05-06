package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementDeleteRule implements a logical DeleteExpression as a
// physical RecordQueryDeletePlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	Delete(target, inner-with-physical-member)
//	  →  DeletePlan(target, inner-physical)
//
// Same gating pattern as the other Implement rules. Java's
// ImplementDeleteRule consults StoredRecordProperty for partition
// dispatch; the seed always emits.
type ImplementDeleteRule struct {
	matcher matching.BindingMatcher
}

// NewImplementDeleteRule constructs the rule.
func NewImplementDeleteRule() *ImplementDeleteRule {
	return &ImplementDeleteRule{
		matcher: NewExpressionMatcher[*expressions.DeleteExpression]("delete"),
	}
}

// Matcher returns the pattern.
func (r *ImplementDeleteRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every DeleteExpression with a physical inner.
func (r *ImplementDeleteRule) OnMatch(call *ExpressionRuleCall) {
	del := matching.Get[*expressions.DeleteExpression](call.Bindings, r.matcher)
	innerRef := del.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	delPlan := plans.NewRecordQueryDeletePlan(innerPlan, del.GetTargetRecordType())

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(NewPhysicalDeleteWrapper(delPlan, innerQ))
}

var _ ExpressionRule = (*ImplementDeleteRule)(nil)
