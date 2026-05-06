package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}
	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}

	plan := plans.NewRecordQueryTempTableInsertPlan(
		innerPlan,
		insert.GetTempTableAlias(),
		insert.IsOwning(),
	)

	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(newPhysicalTempTableInsertWrapper(plan, innerQ))
}

var _ ExpressionRule = (*ImplementTempTableInsertRule)(nil)
