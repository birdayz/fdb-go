package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// getWinnerForOrdering returns the best physical plan in ref that
// satisfies the given RequestedOrdering. Uses the winner map first
// (stamped by OptimizeGroupTask / stampOrderingWinners), falling back
// to scanning all physical members when winners aren't yet available.
//
// For PRESERVE / nil ordering, returns the globally cheapest physical
// plan (NoProperties winner or findBestPhysicalExpr fallback).
func getWinnerForOrdering(ref *expressions.Reference, ordering *RequestedOrdering) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}

	if ordering == nil || ordering.IsPreserve() {
		if w := ref.Winner(expressions.NoProperties); w != nil {
			return w
		}
		return findBestValidPhysicalExpr(ref)
	}

	required := requestedOrderingToProps(ordering)

	if !required.IsEmpty() {
		if w := ref.Winner(required); w != nil {
			return w
		}
		for key, winner := range ref.GetWinners() {
			props, ok := key.(expressions.PhysicalProperties)
			if !ok {
				continue
			}
			if props.Satisfies(required) {
				return winner
			}
		}
	}

	// Winners not stamped yet — scan physical members for the cheapest
	// that satisfies the requested ordering.
	var bestOrdered expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		if isNilInnerFetch(m) {
			continue
		}
		if memberSatisfiesOrdering(m, required) {
			if bestOrdered == nil || PlanningCostModelLess(m, bestOrdered) {
				bestOrdered = m
			}
		}
	}
	if bestOrdered != nil {
		return bestOrdered
	}

	// No plan satisfies the ordering — return globally cheapest.
	if w := ref.Winner(expressions.NoProperties); w != nil {
		return w
	}
	return findBestValidPhysicalExpr(ref)
}

// findBestValidPhysicalExpr returns the cheapest physical member of ref
// under PlanningCostModelLess, excluding nil-inner Fetch shells.
func findBestValidPhysicalExpr(ref *expressions.Reference) expressions.RelationalExpression {
	var best expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		if isNilInnerFetch(m) {
			continue
		}
		if best == nil || PlanningCostModelLess(m, best) {
			best = m
		}
	}
	return best
}

// getWinnerPlan returns the RecordQueryPlan from the winner for the
// given ordering, or nil if no physical plan exists.
func getWinnerPlan(ref *expressions.Reference, ordering *RequestedOrdering) plans.RecordQueryPlan {
	winner := getWinnerForOrdering(ref, ordering)
	if winner == nil {
		return nil
	}
	if ph, ok := winner.(physicalPlanExpression); ok {
		return ph.GetRecordQueryPlan()
	}
	return nil
}

// memberSatisfiesOrdering checks whether a physical member's ordering
// satisfies the given PhysicalProperties requirement.
func memberSatisfiesOrdering(m expressions.RelationalExpression, required expressions.PhysicalProperties) bool {
	if required.IsEmpty() {
		return true
	}
	h, ok := m.(orderingHinter)
	if !ok {
		return false
	}
	ord := h.HintOrdering()
	if !ord.IsKnown || len(ord.Keys) == 0 {
		return false
	}
	provided := orderingToProps(ord)
	return provided.Satisfies(required)
}

// requestedOrderingToProps converts a RequestedOrdering to
// PhysicalProperties for winner-map lookup.
func requestedOrderingToProps(ordering *RequestedOrdering) expressions.PhysicalProperties {
	if ordering == nil || ordering.IsPreserve() {
		return expressions.NoProperties
	}
	parts := ordering.GetParts()
	names := make([]string, len(parts))
	desc := make([]bool, len(parts))
	for i, p := range parts {
		if fv, ok := p.Value.(*values.FieldValue); ok {
			names[i] = fv.Field
		} else {
			names[i] = p.Value.Name()
		}
		desc[i] = p.SortOrder == RequestedSortOrderDescending
	}
	return expressions.OrderingFromNameDir(names, desc)
}
