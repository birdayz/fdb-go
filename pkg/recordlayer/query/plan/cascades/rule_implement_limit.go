package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		orderings = []*RequestedOrdering{PreserveOrdering()}
	}

	seen := make(map[expressions.RelationalExpression]bool)
	for _, ordering := range orderings {
		winner := getWinnerForOrdering(innerRef, ordering, call.CostModel())
		if winner == nil {
			continue
		}
		if seen[winner] {
			continue
		}
		seen[winner] = true
		ph, ok := winner.(physicalPlanExpression)
		if !ok {
			continue
		}
		var limitPlan *plans.RecordQueryLimitPlan
		if lv := lim.GetLimitValue(); lv != nil {
			limitPlan = plans.NewRecordQueryLimitPlanWithValue(ph.GetRecordQueryPlan(), lv, lim.GetOffset())
		} else {
			limitPlan = plans.NewRecordQueryLimitPlan(ph.GetRecordQueryPlan(), lim.GetLimit(), lim.GetOffset())
		}
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
		call.Yield(newPhysicalLimitWrapper(limitPlan, innerQ))
	}
}

var _ ExpressionRule = (*ImplementLimitRule)(nil)
