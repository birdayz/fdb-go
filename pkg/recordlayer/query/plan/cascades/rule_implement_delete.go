package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
// dispatch; Go always emits.
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
	winner := getWinnerForOrdering(innerRef, properties.PreserveOrdering(), call.CostModel())
	if winner == nil {
		return
	}
	if _, ok := winner.(physicalPlanExpression); !ok {
		return
	}
	// The DELETE plan is its own cascades expression now (RFC-184 W2) — it carries
	// the live child edge directly, no physicalDeleteWrapper.
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	delPlan := plans.NewRecordQueryDeletePlanFromQuantifier(innerQ, del.GetTargetRecordType())
	call.Yield(delPlan)
}

var _ ExpressionRule = (*ImplementDeleteRule)(nil)
