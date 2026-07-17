package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// getWinnerForOrdering returns the best physical plan in ref that
// satisfies the given RequestedOrdering.
//
// Ordering satisfaction is judged on each member's DERIVED RICH ordering
// (computeWrapperRichOrdering → RichOrdering.Satisfies): full Value
// identity plus the four-state sort order, including the counterflow-nulls
// gate (ProvidedSortOrder.IsCompatibleWithRequestedSortOrder). There is
// deliberately NO name-keyed winner map in front of this scan — a
// name+direction key drops NULL placement (an ASC_NULLS_LAST requirement
// would map-hit a plain-ASC winner and elide the enforcer sort with the
// wrong null order) and collides distinct Values that share a rendered
// name. The members of a group are few; the scan is the memo.
//
// For PRESERVE / nil ordering, returns the globally cheapest physical
// plan (the group's OPTIMIZE winner, or findBestValidPhysicalExpr when
// none is stamped yet). less is the cost comparator; pass a stats-aware
// comparator (call.CostModel()) so join sub-product winners are chosen by
// real cardinality rather than the default-stats tie (RFC-041).
func getWinnerForOrdering(ref *expressions.Reference, ordering *RequestedOrdering, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	if less == nil {
		less = PlanningCostModelLess
	}

	if ordering == nil || ordering.IsPreserve() {
		if w := ref.Winner(); w != nil {
			return w
		}
		return findBestValidPhysicalExpr(ref, less)
	}

	if best := bestSatisfyingMember(ref, ordering, less); best != nil {
		return best
	}

	// No plan satisfies the ordering — return globally cheapest.
	if w := ref.Winner(); w != nil {
		return w
	}
	return findBestValidPhysicalExpr(ref, less)
}

// bestSatisfyingMember returns the cheapest physical member of ref whose
// derived rich ordering satisfies the requested ordering, or nil when no
// member does. The comparator is wrapped with the deterministic plan-hash
// tie-break so the chosen member is unique — cost ties otherwise resolve
// by member iteration order, flipping the plan across plannings.
func bestSatisfyingMember(ref *expressions.Reference, ordering *RequestedOrdering, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	if ref == nil || ordering == nil {
		return nil
	}
	if less == nil {
		less = PlanningCostModelLess
	}
	tieBrokenLess := lessWithHashTieBreak(less)
	var best expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		if !memberSatisfiesOrdering(m, ordering) {
			continue
		}
		if best == nil || tieBrokenLess(m, best) {
			best = m
		}
	}
	return best
}

// lessWithHashTieBreak wraps a cost comparator with the deterministic
// structural plan-hash tie-break (PlanningCostModel criterion #17) so the
// derived order is TOTAL: a<b, b<a, or — when the comparator ties — a strict
// order by costExprHash. Winner selection iterates the members slice; a
// merely cost-PARTIAL comparator would let the chosen winner depend on
// member insertion order, so the tie-break is what makes the minimum unique
// and the selection reproducible. Mirrors Java PlanningCostModel.compare,
// whose final planHash arm exists so the planner "select[s] the same plan on
// subsequent plannings". The default PlanningCostModelLess already ends in
// this exact hash tie-break, so wrapping it is a no-op there; the wrap is
// what makes determinism hold for ANY custom comparator a caller passes
// (which need not carry its own tie-break).
func lessWithHashTieBreak(less func(a, b expressions.RelationalExpression) bool) func(a, b expressions.RelationalExpression) bool {
	return func(a, b expressions.RelationalExpression) bool {
		if less(a, b) {
			return true
		}
		if less(b, a) {
			return false
		}
		return costExprHash(a) < costExprHash(b)
	}
}

// findBestValidPhysicalExpr returns the cheapest physical member of ref
// under `less`, excluding nil-inner Fetch shells.
func findBestValidPhysicalExpr(ref *expressions.Reference, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	if less == nil {
		less = PlanningCostModelLess
	}
	var best expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		if isNilInnerFetch(m) {
			continue
		}
		if best == nil || less(m, best) {
			best = m
		}
	}
	return best
}

// getWinnerPlan returns the RecordQueryPlan from the winner for the
// given ordering, or nil if no physical plan exists.
func getWinnerPlan(ref *expressions.Reference, ordering *RequestedOrdering, less func(a, b expressions.RelationalExpression) bool) plans.RecordQueryPlan {
	winner := getWinnerForOrdering(ref, ordering, less)
	if winner == nil {
		return nil
	}
	if ph, ok := winner.(physicalPlanExpression); ok {
		return ph.GetRecordQueryPlan()
	}
	return nil
}

// memberSatisfiesOrdering reports whether a physical member's derived rich
// ordering satisfies the requested ordering. Non-physical members and
// nil-inner Fetch shells never satisfy (a Fetch shell's inner is resolved
// only at extraction; costed without it, it ranks artificially cheap).
func memberSatisfiesOrdering(m expressions.RelationalExpression, requested *RequestedOrdering) bool {
	// Physicality gates FIRST: a preserve request must not admit a logical
	// member or a nil-inner Fetch shell either — bestSatisfyingMember
	// promises "cheapest PHYSICAL member" unconditionally.
	pe, ok := m.(physicalPlanExpression)
	if !ok {
		return false
	}
	if isNilInnerFetch(m) {
		return false
	}
	if requested == nil || requested.IsPreserve() {
		return true
	}
	ro := computeWrapperRichOrdering(pe)
	if ro == nil {
		return false
	}
	return ro.Satisfies(requested)
}
