package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PushDistinctBelowFilterRule moves a physical
// RecordQueryUnorderedPrimaryKeyDistinctPlan below a
// RecordQueryPredicatesFilterPlan. This ensures generated plans match
// the form produced by Java's heuristic planner (distinct below filter).
//
// Pattern:
//
//	Distinct(Filter([P], inner))  →  Filter([P'], Distinct(inner))
//
// Predicates P are rebased from the old filter quantifier alias to the
// new quantifier over the distinct plan.
//
// Mirrors Java's `PushDistinctBelowFilterRule`.
type PushDistinctBelowFilterRule struct {
	matcher matching.BindingMatcher
}

func NewPushDistinctBelowFilterRule() *PushDistinctBelowFilterRule {
	return &PushDistinctBelowFilterRule{
		// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryDistinctPlan
		// (no physicalDistinctWrapper).
		matcher: NewExpressionMatcher[*plans.RecordQueryDistinctPlan]("phys_distinct"),
	}
}

func (r *PushDistinctBelowFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushDistinctBelowFilterRule) OnMatch(call *ImplementationRuleCall) {
	distinctW := matching.Get[*plans.RecordQueryDistinctPlan](call.Bindings, r.matcher)

	innerRef := distinctW.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find a physical filter in the distinct's inner. Since RFC-184 W2 the memo
	// holds the bare *plans.RecordQueryPredicatesFilterPlan (no wrapper).
	var filterW *plans.RecordQueryPredicatesFilterPlan
	for _, m := range innerRef.AllMembers() {
		if fw, ok := m.(*plans.RecordQueryPredicatesFilterPlan); ok {
			filterW = fw
			break
		}
	}
	if filterW == nil {
		return
	}

	// Get the filter's inner reference.
	filterInnerRef := filterW.GetInnerQuantifier().GetRangesOver()
	if filterInnerRef == nil {
		return
	}

	// Build: Distinct(filterInner)
	filterInnerExpr := findPhysicalExpr(filterInnerRef)
	if filterInnerExpr == nil {
		return
	}

	filterInnerPlan := bakedInnerPlan(filterInnerExpr)
	if filterInnerPlan == nil {
		return
	}

	// Build: Distinct(filterInner) as its own cascades expression (RFC-184 W2, no
	// physicalDistinctWrapper). Recompute the streaming mode against the NEW inner
	// (the filter's inner): a filter preserves ordering, so a distinct that
	// streamed above the filter is still streaming-eligible below it — but a
	// constructor reset would drop a resume-clean streaming distinct to the
	// cross-page-buggy hash-set as a competing memo alternative (TODO C5).
	streaming := distinctStreamingEligible(filterInnerExpr, filterInnerPlan)
	// The distinct's edge ranges over the BAKED concrete inner frozen in a
	// detached single-member final reference — the memo-canonical structure
	// push_filter_through_fetch case-2 uses, NOT the live filterInnerExpr memo
	// edge (whose children may still be holes). The alias is carried from the
	// disentangled member ref so the flowed value stays stable.
	baseQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(filterInnerRef, []expressions.RelationalExpression{filterInnerExpr}),
	)
	newDistinctInnerQ := expressions.NamedForEachQuantifier(baseQ.GetAlias(),
		call.MemoizeFinalExpression(filterInnerPlan))
	newDistinctPlan := plans.NewRecordQueryDistinctPlanFromQuantifier(newDistinctInnerQ, streaming)

	// Memoize the new distinct.
	distinctRef := call.MemoizeFinalExpression(newDistinctPlan)

	// Create new quantifier over the distinct plan.
	newQOverDistinct := expressions.ForEachQuantifier(distinctRef)

	// Rebase predicates: translate from old filter's inner alias to new quantifier alias.
	oldAlias := filterW.GetInnerQuantifier().GetAlias()
	newAlias := newQOverDistinct.GetAlias()
	rebasedPreds := rebasePredicates(filterW.GetPredicates(), oldAlias, newAlias)

	// Build: Filter([P'], Distinct(inner)) as its own cascades expression carrying
	// a DISENTANGLED FINAL edge over the concrete newDistinctPlan
	// (constraint-preserving disentangle, RFC-184 W2). The edge keeps
	// newQOverDistinct's alias so GetResultValue/derivations are unchanged, but
	// ranges over a private single-member reference — planFromQuantifier resolves
	// newDistinctPlan, not the shared-group winner.
	newFilterInnerQ := expressions.NamedForEachQuantifier(newQOverDistinct.GetAlias(),
		call.MemoizeFinalExpression(newDistinctPlan))
	newFilterPlan := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(newFilterInnerQ, rebasedPreds)

	call.Yield(newFilterPlan)
}

// rebasePredicates translates predicate alias references from old to
// new via a single-entry AliasMap.
func rebasePredicates(preds []predicates.QueryPredicate, oldAlias, newAlias values.CorrelationIdentifier) []predicates.QueryPredicate {
	if oldAlias == newAlias {
		return preds
	}
	am := values.AliasMap{oldAlias: newAlias}
	result := make([]predicates.QueryPredicate, len(preds))
	for i, p := range preds {
		result[i] = predicates.RebasePredicate(p, am)
	}
	return result
}

var _ ImplementationRule = (*PushDistinctBelowFilterRule)(nil)
