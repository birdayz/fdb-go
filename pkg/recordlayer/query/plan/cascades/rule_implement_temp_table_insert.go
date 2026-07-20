package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
	winner := getWinnerForOrdering(innerRef, properties.PreserveOrdering(), call.CostModel())
	if winner == nil {
		return
	}
	if _, ok := winner.(physicalPlanExpression); !ok {
		return
	}

	// Build the insert over the SAME live memo edge it reports as its child — no
	// separate snapshot inner. The plan IS the cascades expression the memo holds
	// (RFC-184 W2), no physicalTempTableInsertWrapper adapter needed.
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	plan := plans.NewRecordQueryTempTableInsertPlanFromQuantifier(
		innerQ,
		insert.GetTempTableAlias(),
		insert.IsOwning(),
	)
	call.Yield(plan)
}

var _ ExpressionRule = (*ImplementTempTableInsertRule)(nil)
