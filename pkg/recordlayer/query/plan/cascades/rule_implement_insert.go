package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementInsertRule implements a logical InsertExpression as a
// physical RecordQueryInsertPlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	Insert(target, type, inner-with-physical-member)
//	  →  InsertPlan(target, type, inner-physical)
//
// Same gating pattern as Implement{Filter,Sort,Distinct,TypeFilter}.
//
// Java's ImplementInsertRule consults PlanPartition properties for
// dispatch; the seed always emits the simple INSERT plan. Per-row
// transforms (UPSERT, ON CONFLICT, etc.) are not in the seed.
type ImplementInsertRule struct {
	matcher matching.BindingMatcher
}

// NewImplementInsertRule constructs the rule.
func NewImplementInsertRule() *ImplementInsertRule {
	return &ImplementInsertRule{
		matcher: NewExpressionMatcher[*expressions.InsertExpression]("insert"),
	}
}

// Matcher returns the pattern.
func (r *ImplementInsertRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every InsertExpression with a physical inner.
func (r *ImplementInsertRule) OnMatch(call *ExpressionRuleCall) {
	ins := matching.Get[*expressions.InsertExpression](call.Bindings, r.matcher)
	innerRef := ins.GetInner().GetRangesOver()
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
	insPlan := plans.NewRecordQueryInsertPlan(ph.GetRecordQueryPlan(), ins.GetTargetRecordType(), ins.GetTargetType())
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	call.Yield(NewPhysicalInsertWrapper(insPlan, innerQ))
}

var _ ExpressionRule = (*ImplementInsertRule)(nil)
