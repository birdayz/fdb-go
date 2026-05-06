package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// ImplementSortRule removes a logical LogicalSortExpression when the
// inner plan already satisfies the requested ordering. This is Java's
// RemoveSortRule pattern: sort is a constraint, not a physical operator.
//
// During PLANNING's top-down pass, the sort expression's requested
// ordering is pushed as a constraint to the inner reference (via
// GetRequestedOrderings). During the bottom-up pass, this rule checks
// if the inner partition's ordering satisfies the request, and if so,
// yields the inner plans directly (removing the sort).
//
// Ports Java's RemoveSortRule (ImplementationCascadesRule).
type ImplementSortRule struct {
	matcher matching.BindingMatcher
}

func NewImplementSortRule() *ImplementSortRule {
	return &ImplementSortRule{
		matcher: &logicalSortMatcher{},
	}
}

func (r *ImplementSortRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementSortRule) OnMatch(call *ImplementationRuleCall) {
	s := call.Bindings.Get(r.matcher).(*expressions.LogicalSortExpression)

	requestedOrdering := sortExpressionToRequestedOrdering(s)

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	if requestedOrdering.IsPreserve() {
		for _, m := range innerRef.FinalMembers() {
			call.YieldFinalExpression(m)
		}
		return
	}

	partitions := ToPlanPartitions(innerRef)
	for _, partition := range partitions {
		ordering := computePartitionOrdering(partition)
		if ordering == nil {
			continue
		}

		preserveDistinctReq := NewRequestedOrdering(
			requestedOrdering.GetParts(),
			DistinctnessPreserveDistinctness,
			requestedOrdering.IsExhaustive(),
		)
		if !ordering.Satisfies(preserveDistinctReq) {
			continue
		}

		for _, expr := range partition.GetExpressions() {
			call.YieldFinalExpression(expr)
		}
	}
}

func (r *ImplementSortRule) GetRequestedOrderings(
	expr expressions.RelationalExpression,
) []*RequestedOrdering {
	s, ok := expr.(*expressions.LogicalSortExpression)
	if !ok {
		return nil
	}
	return []*RequestedOrdering{sortExpressionToRequestedOrdering(s)}
}

func sortExpressionToRequestedOrdering(s *expressions.LogicalSortExpression) *RequestedOrdering {
	keys := s.GetSortKeys()
	if len(keys) == 0 {
		return PreserveOrdering()
	}
	parts := make([]RequestedOrderingPart, len(keys))
	for i, k := range keys {
		sortOrder := RequestedSortOrderAscending
		if k.Reverse {
			sortOrder = RequestedSortOrderDescending
		}
		parts[i] = RequestedOrderingPart{
			Value:     k.Value,
			SortOrder: sortOrder,
		}
	}
	return NewRequestedOrdering(parts, DistinctnessNotDistinct, false)
}

func computePartitionOrdering(partition *PlanPartition) *RichOrdering {
	for _, expr := range partition.GetExpressions() {
		if ph, ok := expr.(physicalPlanExpression); ok {
			return computeWrapperRichOrdering(ph)
		}
	}
	return nil
}

type logicalSortMatcher struct{}

func (m *logicalSortMatcher) RootType() string { return "LogicalSortExpression" }

func (m *logicalSortMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalSortExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

var _ ImplementationRule = (*ImplementSortRule)(nil)
