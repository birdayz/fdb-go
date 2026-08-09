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
	// projected values can push through, yield a Projection over a
	// covering IndexScan — the Fetch is dropped, the Projection is
	// retained (see rule_merge_projection_and_fetch.go: Go's covering
	// plans carry the FULL partial-record result value, so dropping the
	// Projection would leak the whole record).
	//
	// This is an ExpressionRule from BatchAExpressionRules, so it fires
	// during PLANNING, the same phase as Java's
	// MergeProjectionAndFetchRule and the same phase as Go's own
	// MergeProjectionAndFetchRule implementation rule — the difference
	// between the two Go stampers is expression-rule versus
	// implementation-rule, not phase. Firing here means the covering
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
		// Every projected value pushes through the fetch, so the fetch goes and
		// its CHILD is used as-is (Java MergeProjectionAndFetchRule). Nothing is
		// stamped: the child is already Covering(IndexScan) from the access path.
		// The projection is RETAINED above it — Go's deliberate, permanent
		// divergence from Java's `yieldPlan(fetchPlan.getChild())`, see
		// rule_merge_projection_and_fetch.go and DIVERGENCES.md.
		//
		// The projection is its own cascades expression carrying the live
		// fetch-inner edge (RFC-184 W2).
		call.Yield(plans.NewRecordQueryProjectionPlanFromQuantifierWithProvenance(
			projectedValues, proj.GetAliases(), proj.GetAliasMinted(),
			expressions.ForEachQuantifier(fetchInnerRef)))
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
		call.Yield(plans.NewRecordQueryProjectionPlanFromQuantifierWithProvenance(
			proj.GetProjectedValues(), proj.GetAliases(), proj.GetAliasMinted(), innerQ))
	}
}

var _ ExpressionRule = (*ImplementProjectionRule)(nil)
