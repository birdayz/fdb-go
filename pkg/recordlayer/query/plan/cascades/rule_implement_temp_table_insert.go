package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementTempTableInsertRule converts a TempTableInsertExpression
// to a physical TempTableInsertPlan. Requires the inner reference to
// already contain a physical plan.
// Mirrors Java's ImplementTempTableInsertRule.
type ImplementTempTableInsertRule struct {
	matcher matching.BindingMatcher
}

func NewImplementTempTableInsertRule() *ImplementTempTableInsertRule {
	return &ImplementTempTableInsertRule{
		matcher: NewExpressionMatcher[*expressions.TempTableInsertExpression]("temp_table_insert"),
	}
}

func (r *ImplementTempTableInsertRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementTempTableInsertRule) OnMatch(call *ExpressionRuleCall) {
	insert := matching.Get[*expressions.TempTableInsertExpression](call.Bindings, r.matcher)

	innerRef := insert.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	winner := getWinnerForOrdering(innerRef, PreserveOrdering(), call.CostModel())
	if winner == nil {
		return
	}
	ph, ok := winner.(physicalPlanExpression)
	if !ok {
		return
	}

	plan := plans.NewRecordQueryTempTableInsertPlan(
		ph.GetRecordQueryPlan(),
		insert.GetTempTableAlias(),
		insert.IsOwning(),
	)

	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	call.Yield(newPhysicalTempTableInsertWrapper(plan, innerQ))
}

var _ ExpressionRule = (*ImplementTempTableInsertRule)(nil)
