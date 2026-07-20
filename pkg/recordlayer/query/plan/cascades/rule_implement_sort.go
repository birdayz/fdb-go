package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("implement_sort"),
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
	call.PushConstraint(innerRef, []*properties.RequestedOrdering{requestedOrdering})

	if requestedOrdering.IsPreserve() {
		for _, m := range innerRef.AllMembers() {
			if _, ok := m.(physicalPlanExpression); !ok {
				continue
			}
			call.YieldFinalExpression(m)
		}
		return
	}

	requestedParts := requestedOrdering.GetParts()
	sortValueNames := make(map[string]struct{}, len(requestedParts))
	for _, part := range requestedParts {
		sortValueNames[values.ExplainValue(part.Value)] = struct{}{}
	}

	partitions := ToPlanPartitions(innerRef)
	for _, partition := range partitions {
		ordering := computePartitionOrdering(partition)
		if ordering == nil {
			continue
		}

		eqBound := ordering.GetEqualityBoundValues()
		eqBoundNames := make(map[string]struct{}, len(eqBound))
		for v := range eqBound {
			eqBoundNames[values.ExplainValue(v)] = struct{}{}
		}
		equalityBoundUnsorted := len(eqBound)
		seenEqBound := make(map[string]bool, len(requestedParts))
		for _, part := range requestedParts {
			name := values.ExplainValue(part.Value)
			if _, ok := eqBoundNames[name]; ok && !seenEqBound[name] {
				seenEqBound[name] = true
				equalityBoundUnsorted--
			}
		}

		preserveDistinctReq := properties.NewRequestedOrdering(
			requestedParts,
			properties.DistinctnessPreserveDistinctness,
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
				name := values.ExplainValue(v)
				_, inSort := sortValueNames[name]
				// inEq can be true for mixed-binding keys (one fixed +
				// one sorted) — GetOrderingKeys excludes all-fixed but
				// GetEqualityBoundValues includes any-fixed.
				_, inEq := eqBoundNames[name]
				if !inSort && !inEq {
					allCovered = false
					break
				}
			}
			if allCovered {
				for _, expr := range partition.GetExpressions() {
					if pinned := pinOrderedSpine(expr, preserveDistinctReq, call.CostModel()); pinned != nil {
						call.YieldFinalExpression(makeStrictlySorted(pinned))
					}
				}
				continue
			}
		}

		// Java RemoveSortRule lines 127-141: check each plan for
		// unique-index coverage → strictlySorted.
		//
		// Every yield here DROPS the sort on the strength of expr's claimed
		// ordering — an order-preserving wrapper must therefore have its
		// delegation spine pinned (pinOrderedSpine): otherwise extraction's
		// generic rebuild can relink its child group to a cheaper UNORDERED
		// sibling after the sort is gone. Unpinnable expressions are simply
		// not yielded — the in-memory sort alternative still competes.
		numKeys := len(requestedParts) + equalityBoundUnsorted
		for _, expr := range partition.GetExpressions() {
			pinned := pinOrderedSpine(expr, preserveDistinctReq, call.CostModel())
			if pinned == nil {
				continue
			}
			if strictlyOrderedIfUnique(pinned, numKeys) {
				call.YieldFinalExpression(makeStrictlySorted(pinned))
			} else {
				call.YieldFinalExpression(pinned)
			}
		}
	}
}

func (r *ImplementSortRule) GetRequestedOrderings(
	expr expressions.RelationalExpression,
) []*properties.RequestedOrdering {
	s, ok := expr.(*expressions.LogicalSortExpression)
	if !ok {
		return nil
	}
	return []*properties.RequestedOrdering{sortExpressionToRequestedOrdering(s)}
}

func sortExpressionToRequestedOrdering(s *expressions.LogicalSortExpression) *properties.RequestedOrdering {
	keys := s.GetSortKeys()
	if len(keys) == 0 {
		return properties.PreserveOrdering()
	}
	parts := make([]properties.RequestedOrderingPart, len(keys))
	for i, k := range keys {
		// Carry the explicit NULL placement (k.NullsFirst, nil = natural:
		// ASC→FIRST, DESC→LAST). An explicit non-natural placement maps to the
		// *NullsLast/*NullsFirst variant so the satisfaction check retains the
		// sort instead of eliding it against an opposite-null-placement scan.
		var sortOrder properties.RequestedSortOrder
		if !k.Reverse {
			sortOrder = properties.RequestedSortOrderAscending
			if k.NullsFirst != nil && !*k.NullsFirst {
				sortOrder = properties.RequestedSortOrderAscendingNullsLast
			}
		} else {
			sortOrder = properties.RequestedSortOrderDescending
			if k.NullsFirst != nil && *k.NullsFirst {
				sortOrder = properties.RequestedSortOrderDescendingNullsFirst
			}
		}
		parts[i] = properties.RequestedOrderingPart{
			Value:     k.Value,
			SortOrder: sortOrder,
		}
	}
	return properties.NewRequestedOrdering(parts, properties.DistinctnessNotDistinct, false)
}

// strictlyOrderedIfUnique checks whether the given expression is a unique
// index scan whose column count is covered by numKeys (requested sort keys +
// equality-bound unsorted keys). Mirrors Java's RemoveSortRule.strictlyOrderedIfUnique.
// Looks through Fetch wrappers to find the underlying index scan.
func strictlyOrderedIfUnique(expr expressions.RelationalExpression, numKeys int) bool {
	if p, ok := expr.(*plans.RecordQueryIndexPlan); ok {
		return p.IsUnique() && numKeys >= len(p.GetColumnNames())
	}
	if fw, ok := expr.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		ref := fw.GetInnerQuantifier().GetRangesOver()
		if ref == nil {
			return false
		}
		for _, m := range ref.AllMembers() {
			if p, ok := m.(*plans.RecordQueryIndexPlan); ok {
				return p.IsUnique() && numKeys >= len(p.GetColumnNames())
			}
		}
	}
	return false
}

// makeStrictlySorted returns an expression with its inner plan marked
// as strictlySorted. For index scans, this creates a new wrapper with
// a cloned plan. For Fetch wrappers, creates a new Fetch wrapping a
// strictlySorted index plan. For other plan types, returns unchanged.
func makeStrictlySorted(expr expressions.RelationalExpression) expressions.RelationalExpression {
	if p, ok := expr.(*plans.RecordQueryIndexPlan); ok {
		// WithStrictlySorted is a struct copy — it preserves the index metadata
		// (columns/pk/unique/covering) the plan already carries (RFC-184 W2).
		return p.WithStrictlySorted()
	}
	if fw, ok := expr.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		inner := fw.GetInner()
		if idxPlan, ok := inner.(*plans.RecordQueryIndexPlan); ok {
			// The index scan is its own cascades expression (RFC-184 W2), carrying its
			// metadata on the plan; WithStrictlySorted preserves it (struct copy). The
			// strictly-sorted path is only reached for a unique index (see
			// strictlyOrderedIfUnique), so the plan's unique flag is already true.
			newIdxPlan := idxPlan.WithStrictlySorted()
			newIdxRef := expressions.InitialOf(newIdxPlan)
			newFetchQ := expressions.ForEachQuantifier(newIdxRef)
			// The fetch is its own cascades expression carrying the live newIdxRef
			// edge (RFC-184 W2).
			return plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
				newFetchQ,
				fw.GetTranslateValueFunction(),
				fw.GetResultType(),
				fw.GetFetchIndexRecords(),
			)
		}
	}
	return expr
}

// computePartitionOrdering returns the ordering of the first physical
// plan in the partition. All members share the same ordering by
// construction (partitions are keyed on ordering properties).
func computePartitionOrdering(partition *PlanPartition) *properties.RichOrdering {
	for _, expr := range partition.GetExpressions() {
		if ph, ok := expr.(physicalPlanExpression); ok {
			return computeWrapperRichOrdering(ph)
		}
	}
	return nil
}

var _ ImplementationRule = (*ImplementSortRule)(nil)
