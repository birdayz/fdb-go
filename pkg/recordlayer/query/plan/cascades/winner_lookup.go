package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// The satisfied return is RFC-186 §2C's contract-tightening: it reports
// whether the member SATISFIES the requested ordering (trivially true for
// nil/preserve), so no caller can mistake the load-bearing fallback for
// satisfaction. The fallback yield itself STAYS — under a sort the child
// group's only requested ordering is the sort's (no preserve), so
// returning nothing would empty the group and forfeit plannability; the
// unordered fallback is what the in-memory-sort enforcer wraps. Callers
// never stamp ordering off this return: the physical wrappers are
// orderingDelegators whose claim is re-derived through OrderingSourceRef,
// and pinOrderedSpine declines unsatisfied spines — `satisfied` makes that
// delegation explicit at the call site instead of implicit downstream.
func getWinnerForOrdering(ref *expressions.Reference, ordering *properties.RequestedOrdering, less func(a, b expressions.RelationalExpression) bool) (expressions.RelationalExpression, bool) {
	if ref == nil {
		return nil, false
	}
	if less == nil {
		less = PlanningCostModelLess
	}

	if ordering == nil || ordering.IsPreserve() {
		if w := ref.Winner(); w != nil {
			return w, true
		}
		return findBestValidPhysicalExpr(ref, less), true
	}

	if best := bestSatisfyingMember(ref, ordering, less); best != nil {
		return best, true
	}

	// No member satisfies the ordering — yield the globally cheapest as the
	// UNORDERED fallback (see the contract note above: the enforcer path
	// depends on this yield; satisfied=false is the caller's signal).
	if w := ref.Winner(); w != nil {
		return w, false
	}
	return findBestValidPhysicalExpr(ref, less), false
}

// bestSatisfyingMember returns the cheapest physical member of ref whose
// derived rich ordering satisfies the requested ordering, or nil when no
// member does. The comparator is wrapped with the deterministic plan-hash
// tie-break so the chosen member is unique — cost ties otherwise resolve
// by member iteration order, flipping the plan across plannings.
func bestSatisfyingMember(ref *expressions.Reference, ordering *properties.RequestedOrdering, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
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

// pinOrderedSpine returns expr with its ordering-delegation spine BAKED:
// each order-preserving wrapper's source group is resolved to its cheapest
// satisfying member and rebuilt over a fresh singleton Reference, so no
// later selection (extraction's generic rebuild, a parent's relink) can
// swap the spine to an unordered sibling after a sort has been dropped on
// the strength of this expression's ordering. Non-delegators return
// unchanged — their own plan carries the ordering. Returns nil when any
// spine level has no satisfying member or cannot relink (WithChildren
// unsupported): the caller must then keep its enforcer sort. Mirrors the
// extraction-side rebuildOrderedSpine; this is the RULE-time twin for
// yields that drop a sort during PLANNING (Java bakes concrete children at
// rule time via memoizePlan, so it has no unpinned window at all).
//
// REMOVAL CONDITION. This function is compensation, and the thing it
// compensates for is a defect with a known fix, so record what would retire it
// rather than leaving it to be re-argued. Two DISTINCT things are named below —
// the condition that must become true, and the measurement that would show it
// has. Neither substitutes for the other: the condition holding is what makes
// the pin unnecessary, and the measurement is the only way to know it holds.
//
// THE CONDITION — constraint propagation. MemoizeFinalExpressionsFromOther
// now propagates the requested-ordering entry to the reference it mints, so
// OptimizeGroup's per-ordering retention can see it. The pin remains as the
// rule-time equivalent of Java baking the selected child: it also protects the
// interval before that fresh reference is optimized and any relink that would
// otherwise consult a different compatible member.
//
// THE VERIFICATION — the StoredRecordProperty fetch arm in plan_properties.go
// is enabled and the ordered InUnion sentinels remain sort-free. That closes
// the previously booked propagation blocker; it does not make this explicit
// baked-spine guarantee redundant.
func pinOrderedSpine(expr expressions.RelationalExpression, ordering *properties.RequestedOrdering, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	return pinOrderedSpineDepth(expr, ordering, less, 0)
}

func pinOrderedSpineDepth(expr expressions.RelationalExpression, ordering *properties.RequestedOrdering, less func(a, b expressions.RelationalExpression) bool, depth int) expressions.RelationalExpression {
	d, ok := expr.(orderingDelegator)
	if !ok {
		return expr
	}
	if depth >= maxOrderingDelegationDepth {
		return nil
	}
	srcRef := d.OrderingSourceRef()
	if srcRef == nil {
		return nil
	}
	ordering, ok = requestedOrderingBelow(expr, ordering)
	if !ok {
		return nil
	}
	m := bestSatisfyingMember(srcRef, ordering, less)
	if m == nil {
		return nil
	}
	requirements, err := ordinalInputRequirementsOf(expr)
	if err != nil {
		return nil
	}
	if len(requirements) != 0 {
		compatible, compatibilityErr := memberSatisfiesOrdinalRequirement(m, requirements[0])
		if compatibilityErr != nil || !compatible {
			return nil
		}
	}
	inner := pinOrderedSpineDepth(m, ordering, less, depth+1)
	if inner == nil {
		return nil
	}
	rebuilder, ok := expr.(WithChildren)
	if !ok {
		return nil
	}
	if len(expr.GetQuantifiers()) != 1 {
		return nil
	}
	// PinnedFinalOf, not an ordinary FinalOf: this is a private physical
	// selection, not a one-member equivalence group that physical rewrite rules
	// may expand. ExploreGroup recognizes the explicit marker and preserves the
	// exact ordered member whose property licensed dropping the enforcer sort.
	pinnedQ := expressions.ForEachQuantifier(expressions.PinnedFinalOf(inner))
	pinned, err := rebuilder.WithChildren([]expressions.Quantifier{pinnedQ})
	if err != nil || pinned == nil {
		return nil
	}
	// WithChildren is allowed to keep its ORIGINAL concrete plan when the
	// new child isn't leaf-replaceable (several wrappers gate on
	// isLeafReplaceable) — the quantifier then points at the pinned member
	// while GetRecordQueryPlan() still executes the OLD child. A pin that
	// did not reach the executable plan is not a pin: verify the wrapper's
	// concrete child IS the pinned member's plan, else decline (the sort
	// stays, which is always order-correct).
	pinnedPE, ok := pinned.(physicalPlanExpression)
	if !ok {
		return nil
	}
	innerPE, ok := inner.(physicalPlanExpression)
	if !ok {
		return nil
	}
	if !planHasDirectChild(pinnedPE.GetRecordQueryPlan(), innerPE.GetRecordQueryPlan()) {
		return nil
	}
	return pinned
}

// planHasDirectChild reports whether child is among plan's immediate
// concrete children (pointer identity — WithChildren embeds the exact
// plan object it relinked to).
func planHasDirectChild(plan, child plans.RecordQueryPlan) bool {
	if plan == nil || child == nil {
		return false
	}
	for _, c := range plan.GetChildren() {
		if c == child {
			return true
		}
	}
	return false
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
// under `less`.
func findBestValidPhysicalExpr(ref *expressions.Reference, less func(a, b expressions.RelationalExpression) bool) expressions.RelationalExpression {
	if less == nil {
		less = PlanningCostModelLess
	}
	var best expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		if best == nil || less(m, best) {
			best = m
		}
	}
	return best
}

// orderingDelegator is implemented by physical wrappers that PRESERVE
// their input's order rather than producing one — their HintOrdering
// delegates to the inner child group. Ordering satisfaction and
// extraction-time sort elision must resolve through the SOURCE GROUP
// (which member actually provides the order), never through a flat
// first-known-member estimate: the estimate both over-claims (the group's
// eventual extraction winner may be a different, unordered member — the
// sort would be elided and then rebuilt over an unordered child) and
// under-claims (the first known ordering may be a different ordering
// than the one requested while a later member provides it).
// Contract: a delegator is a SINGLE-quantifier wrapper (its one child IS
// the ordering source) implementing WithChildren for the
// relink. A multi-quantifier delegator would need per-quantifier routing
// in pinOrderedSpine / rebuildOrderedSpine, which conservatively DECLINE
// (sort kept) on anything else — implement the routing before adding one.
type orderingDelegator interface {
	OrderingSourceRef() *expressions.Reference
}

// maxOrderingDelegationDepth bounds the delegator chain walk. Physical
// plan wrappers nest far shallower than this; hitting the cap means a
// cyclic reference (e.g. a recursive-CTE back-edge) reached the ordering
// spine — answered conservatively (not satisfied ⇒ the sort stays).
const maxOrderingDelegationDepth = 64

// memberSatisfiesOrdering reports whether a physical member's derived rich
// ordering satisfies the requested ordering. Non-physical members never
// satisfy. Order-preserving wrappers resolve through their source group (see
// orderingDelegator).
func memberSatisfiesOrdering(m expressions.RelationalExpression, requested *properties.RequestedOrdering) bool {
	return memberSatisfiesOrderingDepth(m, requested, 0)
}

func memberSatisfiesOrderingDepth(m expressions.RelationalExpression, requested *properties.RequestedOrdering, depth int) bool {
	// Physicality gates FIRST: a preserve request must not admit a logical
	// member either — bestSatisfyingMember promises "cheapest PHYSICAL
	// member" unconditionally.
	pe, ok := m.(physicalPlanExpression)
	if !ok {
		return false
	}
	if requested == nil || requested.IsPreserve() {
		return true
	}
	if d, ok := m.(orderingDelegator); ok {
		if depth >= maxOrderingDelegationDepth {
			return false
		}
		srcRef := d.OrderingSourceRef()
		if srcRef == nil {
			return false
		}
		below, ok := requestedOrderingBelow(m, requested)
		if !ok {
			return false
		}
		for _, sm := range srcRef.AllMembers() {
			if memberSatisfiesOrderingDepth(sm, below, depth+1) {
				return true
			}
		}
		return false
	}
	ro := computeWrapperRichOrdering(pe)
	if ro == nil {
		return false
	}
	return ro.Satisfies(requested)
}

// Compile-time proof that the delegator PLANS answer OrderingSourceRef, not
// just their physical wrappers (RFC-183 P5). The interface is satisfied from
// package plans only because the method is exported — an unexported method
// would confine the contract to this package.
var (
	_ orderingDelegator = (*plans.RecordQueryFilterPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryPredicatesFilterPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryTypeFilterPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryDistinctPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryProjectionPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryMapPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryLimitPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryDefaultOnEmptyPlan)(nil)
	_ orderingDelegator = (*plans.RecordQueryFetchFromPartialRecordPlan)(nil)
)

// requestedOrderingBelow translates a requested ordering through an
// order-preserving wrapper into the row space of the source group it
// delegates to. A filter, a limit, a distinct or a fetch flows its input row
// unchanged, so the request crosses as it is. A projection or a map RESHAPES
// the row: its output slot H may be the input's ID under another name, and a
// request stated over the output — `_current.H#0` — names nothing in the
// source's row until it is pushed through the wrapper's result value, exactly
// as PushRequestedOrderingThroughProjectionRule pushes the constraint and as
// Java's OrderingProperty.visitMapPlan pulls the child's ordering up through
// the map's result value (the dual of the same translation). Walking the
// delegator chain with the untranslated request matched the output name
// against the source's keys and so satisfied `ORDER BY u.h` over
// `(SELECT id AS h FROM t) u` only when the two names happened to coincide:
// every renamed column kept an in-memory sort over an input that was already
// in that order.
//
// The request reaching this walk is stated over the wrapper's output row —
// rooted at the group's current carrier when it is the constraint the sort
// rule pushed, or at the sort's own inner quantifier when it is the sort's
// keys as spelled — so the push-down's upper alias is the root the parts
// name, and the pushed parts, now rooted at the wrapper's child EDGE, are
// rebased into the source group's current-row space
// (requestedOrderingAtInnerCurrent). A part the result value cannot express
// (a computed slot) drops, a request whose parts name two roots is not one
// request, and a request that lost a part is not satisfiable below the
// wrapper: the sort stays.
func requestedOrderingBelow(
	m expressions.RelationalExpression,
	requested *properties.RequestedOrdering,
) (*properties.RequestedOrdering, bool) {
	if requested == nil || requested.IsPreserve() {
		return requested, true
	}
	var (
		resultValue values.Value
		innerQ      expressions.Quantifier
	)
	switch p := m.(type) {
	case *plans.RecordQueryProjectionPlan:
		resultValue, innerQ = p.GetResultValue(), p.GetInnerQuantifier()
	case *plans.RecordQueryMapPlan:
		resultValue, innerQ = p.GetResultValue(), p.GetInnerQuantifier()
	case *expressions.LogicalProjectionExpression:
		// The logical projection takes the same crossing when its
		// requested-ordering CONSTRAINT is pushed to its child group
		// (PushRequestedOrderingThroughProjectionRule): one translation for
		// the constraint going down and for the satisfaction walk.
		resultValue, innerQ = p.GetResultValue(), p.GetInner()
	default:
		return requested, true
	}
	if resultValue == nil || innerQ.GetRangesOver() == nil {
		return nil, false
	}
	upper, ok := requestedOrderingRoot(requested)
	if !ok {
		return nil, false
	}
	pushed := requested.PushDownThroughValue(resultValue, upper)
	if pushed.IsPreserve() || pushed.Size() != requested.Size() {
		return nil, false
	}
	below, err := requestedOrderingAtInnerCurrent(pushed, innerQ)
	if err != nil {
		return nil, false
	}
	return below, true
}

// requestedOrderingRoot reports the one quantifier every part of a requested
// ordering reads from. A part with no correlation (a constant, a parameter) or
// two parts rooted at different quantifiers make no single row to push the
// request through.
func requestedOrderingRoot(requested *properties.RequestedOrdering) (values.CorrelationIdentifier, bool) {
	var root values.CorrelationIdentifier
	for _, part := range requested.GetParts() {
		correlated := values.GetCorrelatedToOfValue(part.Value)
		if len(correlated) != 1 {
			return values.CorrelationIdentifier{}, false
		}
		for alias := range correlated {
			if !root.IsZero() && alias != root {
				return values.CorrelationIdentifier{}, false
			}
			root = alias
		}
	}
	return root, !root.IsZero()
}
