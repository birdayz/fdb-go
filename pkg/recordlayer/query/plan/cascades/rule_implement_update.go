package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementUpdateRule implements a logical UpdateExpression as a
// physical RecordQueryUpdatePlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	Update(target, [transforms], inner-with-physical-member)
//	  →  UpdatePlan(target, [transforms], inner-physical)
//
// Per-row transform application happens at execution time (not
// rule-fire time) — transforms pass through unchanged. The rule
// structure is identical to ImplementInsert/Delete; the
// transforms-evaluation gating is in the executor, not the rule.
//
// Java's ImplementUpdateRule consults StoredRecordProperty for
// dispatch; Go always emits.
type ImplementUpdateRule struct {
	matcher matching.BindingMatcher
}

// NewImplementUpdateRule constructs the rule.
func NewImplementUpdateRule() *ImplementUpdateRule {
	return &ImplementUpdateRule{
		matcher: NewExpressionMatcher[*expressions.UpdateExpression]("update"),
	}
}

// Matcher returns the pattern.
func (r *ImplementUpdateRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every UpdateExpression with a physical inner.
func (r *ImplementUpdateRule) OnMatch(call *ExpressionRuleCall) {
	upd := matching.Get[*expressions.UpdateExpression](call.Bindings, r.matcher)
	innerRef := upd.GetInner().GetRangesOver()
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
	// The UPDATE plan is its own cascades expression now (RFC-184 W2) — it carries
	// the live child edge directly, no physicalUpdateWrapper.
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	updPlan := plans.NewRecordQueryUpdatePlanFromQuantifier(innerQ, upd.GetTargetRecordType(), upd.GetTransforms())
	call.Yield(updPlan)
}

var _ ExpressionRule = (*ImplementUpdateRule)(nil)
