package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementLimitRule converts a LogicalLimitExpression into a physical
// RecordQueryLimitPlan. LIMIT/OFFSET is a pure pass-through that caps
// the row count — it applies to whatever physical plan the inner
// produces.
//
// Go-only extension: Java doesn't support LIMIT in SQL; it uses
// ExecuteProperties.setReturnedRowLimit() at the JDBC layer.
type ImplementLimitRule struct {
	matcher matching.BindingMatcher
}

func NewImplementLimitRule() *ImplementLimitRule {
	return &ImplementLimitRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("limit_impl"),
	}
}

func (r *ImplementLimitRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementLimitRule) OnMatch(call *ExpressionRuleCall) {
	lim := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)

	innerRef := lim.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}

	limitPlan := plans.NewRecordQueryLimitPlan(innerPlan, lim.GetLimit(), lim.GetOffset())
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(newPhysicalLimitWrapper(limitPlan, innerQ))
}

var _ ExpressionRule = (*ImplementLimitRule)(nil)
