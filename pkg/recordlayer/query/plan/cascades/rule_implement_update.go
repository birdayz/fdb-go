package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementUpdateRule implements a logical UpdateExpression as a
// physical RecordQueryUpdatePlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	Update(target, [transforms], inner-with-physical-member)
//	  →  UpdatePlan(target, [transforms], inner-physical)
//
// Per-row transform application happens at execution time (not
// rule-fire time) — transforms pass through unchanged. The seed
// rule structure is identical to ImplementInsert/Delete; the
// transforms-evaluation gating is in the executor, not the rule.
//
// Java's ImplementUpdateRule consults StoredRecordProperty for
// dispatch; the seed always emits.
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
	winner := getWinnerForOrdering(innerRef, PreserveOrdering())
	if winner == nil {
		return
	}
	ph, ok := winner.(physicalPlanExpression)
	if !ok {
		return
	}
	updPlan := plans.NewRecordQueryUpdatePlan(ph.GetRecordQueryPlan(), upd.GetTargetRecordType(), upd.GetTransforms())
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	call.Yield(NewPhysicalUpdateWrapper(updPlan, innerQ))
}

var _ ExpressionRule = (*ImplementUpdateRule)(nil)
