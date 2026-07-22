package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
		fetchW, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan)
		if !ok {
			continue
		}
		srcAlias := fetchW.GetInnerQuantifier().GetAlias()
		tgtAlias := values.UniqueCorrelationIdentifier()
		allPushable := true
		for _, v := range projectedValues {
			if _, ok := fetchW.PushValue(v, srcAlias, tgtAlias); !ok {
				allPushable = false
				break
			}
		}
		if !allPushable {
			continue
		}
		fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
		if fetchInnerRef == nil {
			continue
		}
		if idxPlan := findIndexScanPlan(fetchInnerRef); idxPlan != nil {
			// The covering index scan is its own cascades expression (RFC-184 W2);
			// WithCovering preserves the metadata already on the plan (struct copy).
			coveredPlan := idxPlan.WithCovering(idxPlan.GetColumnNames())
			cq := expressions.ForEachQuantifier(call.MemoizeExpression(coveredPlan))
			// The projection is its own cascades expression carrying the live cq
			// edge (RFC-184 W2).
			call.Yield(plans.NewRecordQueryProjectionPlanFromQuantifier(
				projectedValues, proj.GetAliases(), cq))
		}
	}

	// Normal path: for each requested ordering, wrap the child winner.
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
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
		// The projection is its own cascades expression carrying the live innerQ
		// edge (RFC-184 W2).
		call.Yield(plans.NewRecordQueryProjectionPlanFromQuantifier(
			proj.GetProjectedValues(), proj.GetAliases(), innerQ))
	}
}

var _ ExpressionRule = (*ImplementProjectionRule)(nil)
