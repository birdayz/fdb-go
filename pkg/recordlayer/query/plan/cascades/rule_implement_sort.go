package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

	// Top-down: push ordering constraint to inner reference so
	// downstream rules (index scans) can satisfy it.
	call.PushConstraint(innerRef, []*RequestedOrdering{requestedOrdering})

	if requestedOrdering.IsPreserve() {
		for _, m := range innerRef.FinalMembers() {
			call.YieldFinalExpression(m)
		}
		return
	}

	requestedParts := requestedOrdering.GetParts()
	sortValuesSet := make(map[values.Value]struct{}, len(requestedParts))
	for _, part := range requestedParts {
		sortValuesSet[part.Value] = struct{}{}
	}

	partitions := ToPlanPartitions(innerRef)
	for _, partition := range partitions {
		ordering := computePartitionOrdering(partition)
		if ordering == nil {
			continue
		}

		equalityBoundKeys := ordering.GetEqualityBoundValues()
		equalityBoundUnsorted := len(equalityBoundKeys)
		for _, part := range requestedParts {
			if _, ok := equalityBoundKeys[part.Value]; ok {
				equalityBoundUnsorted--
			}
		}

		preserveDistinctReq := NewRequestedOrdering(
			requestedParts,
			DistinctnessPreserveDistinctness,
			requestedOrdering.IsExhaustive(),
		)
		if !ordering.Satisfies(preserveDistinctReq) {
			continue
		}

		// Java RemoveSortRule lines 112-125: when the partition is
		// distinct and all ordering values are covered by sort keys
		// or equality-bound keys, yield strictlySorted copies.
		if partition.IsDistinct() {
			allCovered := true
			for _, v := range ordering.GetOrderingKeys() {
				_, inSort := sortValuesSet[v]
				_, inEq := equalityBoundKeys[v]
				if !inSort && !inEq {
					allCovered = false
					break
				}
			}
			if allCovered {
				for _, expr := range partition.GetExpressions() {
					call.YieldFinalExpression(makeStrictlySorted(expr))
				}
				continue
			}
		}

		// Java RemoveSortRule lines 127-141: check each plan for
		// unique-index coverage → strictlySorted.
		numKeys := len(requestedParts) + equalityBoundUnsorted
		for _, expr := range partition.GetExpressions() {
			if strictlyOrderedIfUnique(expr, numKeys) {
				call.YieldFinalExpression(makeStrictlySorted(expr))
			} else {
				call.YieldFinalExpression(expr)
			}
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

// strictlyOrderedIfUnique checks whether the given expression is a unique
// index scan whose column count is covered by numKeys (requested sort keys +
// equality-bound unsorted keys). Mirrors Java's RemoveSortRule.strictlyOrderedIfUnique.
func strictlyOrderedIfUnique(expr expressions.RelationalExpression, numKeys int) bool {
	w, ok := expr.(*physicalIndexScanWrapper)
	if !ok {
		return false
	}
	return w.unique && numKeys >= len(w.columnNames)
}

// makeStrictlySorted returns an expression with its inner plan marked
// as strictlySorted. For index scans, this creates a new wrapper with
// a cloned plan. For other plan types, returns the expression unchanged.
func makeStrictlySorted(expr expressions.RelationalExpression) expressions.RelationalExpression {
	w, ok := expr.(*physicalIndexScanWrapper)
	if !ok {
		return expr
	}
	return &physicalIndexScanWrapper{
		plan:        w.plan.WithStrictlySorted(),
		columnNames: w.columnNames,
		unique:      w.unique,
	}
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
