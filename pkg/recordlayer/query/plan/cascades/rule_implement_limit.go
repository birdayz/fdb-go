package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementLimitRule converts a LogicalLimitExpression into a physical
// RecordQueryLimitPlan. LIMIT/OFFSET is a pure pass-through that caps
// the row count â it applies to whatever physical plan the inner
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

// IsPhysicalLimit reports whether the given RelationalExpression is a physical
// LIMIT. Since RFC-184 W2 the memo holds *plans.RecordQueryLimitPlan directly
// (no physicalLimitWrapper), so this is a bare type check on the plan.
func IsPhysicalLimit(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*plans.RecordQueryLimitPlan)
	return ok
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
		orderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	}

	seen := make(map[expressions.RelationalExpression]bool)
	for _, ordering := range orderings {
		// satisfied deliberately DISCARDED (RFC-186 §2C): this wrapper is an
		// orderingDelegator — its ordering claim is re-derived through
		// OrderingSourceRef at lookup time and pinOrderedSpine declines
		// unsatisfied spines, so an unordered fallback yield here can never
		// be CLAIMED as ordered; it is the member the in-memory-sort
		// enforcer wraps (declining instead would empty the group — no plan).
		winner, _ := getWinnerForOrdering(innerRef, ordering, call.CostModel())
		if winner == nil {
			continue
		}
		if seen[winner] {
			continue
		}
		seen[winner] = true
		if _, ok := winner.(physicalPlanExpression); !ok {
			continue
		}
		// Build the LIMIT over the SAME live memo edge it reports as its child â
		// no separate snapshot inner. The plan IS the cascades expression the memo
		// holds (RFC-184 W2): GetQuantifiers / OrderingSourceRef / GetInner all
		// resolve through this one quantifier, so there is no nil-inner shell to
		// leave stale (the class the physicalLimitWrapper's WithChildren pinned).
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
		var limitPlan *plans.RecordQueryLimitPlan
		if lv := lim.GetLimitValue(); lv != nil {
			// Runtime cap: static limit is the no-cap sentinel (-1), only lv consulted.
			limitPlan = plans.NewRecordQueryLimitPlanFromQuantifier(innerQ, -1, lim.GetOffset(), lv)
		} else {
			limitPlan = plans.NewRecordQueryLimitPlanFromQuantifier(innerQ, lim.GetLimit(), lim.GetOffset(), nil)
		}
		call.Yield(limitPlan)
	}
}

var _ ExpressionRule = (*ImplementLimitRule)(nil)
