package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PhysicalizeSortRule converts a LogicalSortExpression into a physical
// RecordQuerySortPlan during exploration phase. Used by the non-Cascades
// (plangen) pipeline where sort needs to become a physical operator.
//
// In the Cascades pipeline, sort is handled differently: ImplementSortRule
// (RemoveSortRule pattern) removes the sort during PLANNING when the
// inner already satisfies the ordering. This rule is NOT in
// DefaultImplementationRules; it's in BatchAExpressionRules for backward
// compatibility with the plangen path.
type PhysicalizeSortRule struct {
	matcher matching.BindingMatcher
}

func NewPhysicalizeSortRule() *PhysicalizeSortRule {
	return &PhysicalizeSortRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("physicalize_sort"),
	}
}

func (r *PhysicalizeSortRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PhysicalizeSortRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	sortPlan := plans.NewRecordQuerySortPlan(s.GetSortKeys(), innerPlan)

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(NewPhysicalSortWrapper(sortPlan, innerQ))
}

var _ ExpressionRule = (*PhysicalizeSortRule)(nil)
