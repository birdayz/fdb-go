package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementProjectionRule implements a logical LogicalProjectionExpression
// as a physical RecordQueryProjectionPlan, gated on the inner Reference
// having at least one physical-plan member.
type ImplementProjectionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementProjectionRule() *ImplementProjectionRule {
	return &ImplementProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

func (r *ImplementProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementProjectionRule) OnMatch(call *ExpressionRuleCall) {
	proj := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	qs := proj.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	// Try covering merge: if inner has a Fetch wrapper and all
	// projected values can push through, yield a covering IndexScan
	// directly (no Projection, no Fetch). This fires during EXPLORE
	// (not PLANNING like Java's MergeProjectionAndFetchRule) because
	// Go's ExpressionRules see the Fetch wrapper in the inner
	// Reference before PLANNING runs. The EXPLORE-phase covering
	// scan participates in sort elimination and cost comparison.
	projectedValues := proj.GetProjectedValues()
	for _, m := range innerRef.AllMembers() {
		fetchW, ok := m.(*physicalFetchFromPartialRecordWrapper)
		if !ok {
			continue
		}
		srcAlias := fetchW.innerQuant.GetAlias()
		tgtAlias := values.UniqueCorrelationIdentifier()
		allPushable := true
		for _, v := range projectedValues {
			if _, ok := fetchW.plan.PushValue(v, srcAlias, tgtAlias); !ok {
				allPushable = false
				break
			}
		}
		if !allPushable {
			continue
		}
		fetchInnerRef := fetchW.innerQuant.GetRangesOver()
		if fetchInnerRef == nil {
			continue
		}
		if idxW := findIndexScanWrapper(fetchInnerRef); idxW != nil {
			coveredPlan := idxW.plan.WithCovering(idxW.columnNames)
			coveringIdxW := &physicalIndexScanWrapper{
				plan:        coveredPlan,
				columnNames: idxW.columnNames,
				unique:      idxW.unique,
				covering:    true,
			}
			coveringRef := call.MemoizeExpression(coveringIdxW)
			cq := expressions.ForEachQuantifier(coveringRef)
			wrapPlan := plans.NewRecordQueryProjectionPlanWithAliases(
				projectedValues, proj.GetAliases(), coveredPlan)
			call.Yield(NewPhysicalProjectionWrapper(wrapPlan, cq))
		}
	}

	// Normal path: for each requested ordering, wrap the child winner.
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
		projPlan := plans.NewRecordQueryProjectionPlanWithAliases(proj.GetProjectedValues(), proj.GetAliases(), ph.GetRecordQueryPlan())
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
		call.Yield(NewPhysicalProjectionWrapper(projPlan, innerQ))
	}
}

var _ ExpressionRule = (*ImplementProjectionRule)(nil)
