package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushRequestedOrderingThroughGroupByRule is a PLANNING-phase
// ImplementationRule that synthesizes compatible orderings from a
// RequestedOrdering constraint and a GroupByExpression's grouping keys,
// then pushes the synthesized ordering to the child Reference.
//
// GroupBy is NOT ordering-transparent — the pushed ordering must cover
// all grouping keys (for streaming aggregation) while respecting the
// requested ordering's key prefix (for the outer ORDER BY). The rule
// synthesizes the ordering as:
//
//   - Each requested ordering part that matches a grouping key (by
//     field name, case-insensitive) retains its sort direction.
//   - Remaining grouping keys not in the request are appended with
//     ANY sort order (direction doesn't matter for streaming
//     aggregation — they just need to be contiguous).
//
// If any requested ordering part does NOT match a grouping key, the
// ordering is incompatible and is not pushed. If there are no grouping
// keys (scalar aggregation), a preserve ordering is pushed — scalar
// aggregation produces 0-1 rows, so any ordering is trivially satisfied.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op.
//
// Ports Java's PushRequestedOrderingThroughGroupByRule.
type PushRequestedOrderingThroughGroupByRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughGroupByRule() *PushRequestedOrderingThroughGroupByRule {
	return &PushRequestedOrderingThroughGroupByRule{
		matcher: NewExpressionMatcher[*expressions.GroupByExpression]("push_requested_ordering_through_groupby"),
	}
}

func (r *PushRequestedOrderingThroughGroupByRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughGroupByRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	gb := call.Bindings.Get(r.matcher).(*expressions.GroupByExpression)
	innerRef := gb.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	groupingKeys := gb.GetGroupingKeys()

	var synthesized []*properties.RequestedOrdering
	for _, reqOrd := range orderings {
		if reqOrd.IsPreserve() {
			// Preserve ordering: push the grouping keys with ANY sort
			// order. Scalar aggregation (no grouping keys) trivially
			// satisfies any ordering.
			if len(groupingKeys) == 0 {
				synthesized = append(synthesized, properties.PreserveOrdering())
			} else {
				parts := make([]properties.RequestedOrderingPart, len(groupingKeys))
				for i, gk := range groupingKeys {
					parts[i] = properties.RequestedOrderingPart{
						Value:     gk,
						SortOrder: properties.RequestedSortOrderAny,
					}
				}
				synthesized = append(synthesized, properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false))
			}
			continue
		}

		if len(groupingKeys) == 0 {
			// No grouping keys — scalar aggregation produces 0-1 rows.
			// Any ordering is trivially satisfied.
			synthesized = append(synthesized, properties.PreserveOrdering())
			continue
		}

		result := synthesizeGroupByOrdering(reqOrd, groupingKeys)
		if result != nil {
			synthesized = append(synthesized, result)
		}
	}

	if len(synthesized) > 0 {
		call.PushConstraint(innerRef, synthesized)
	}
}

// synthesizeGroupByOrdering checks that every ordering part matches a
// grouping key and returns a synthesized ordering covering all grouping
// keys. Returns nil if the ordering is incompatible.
func synthesizeGroupByOrdering(reqOrd *properties.RequestedOrdering, groupingKeys []values.Value) *properties.RequestedOrdering {
	// Build a map of grouping key field names for quick lookup.
	type groupKeyEntry struct {
		index int
		value values.Value
	}
	groupKeyMap := make(map[string]groupKeyEntry, len(groupingKeys))
	for i, gk := range groupingKeys {
		// Key by full accessor path, not leaf name (RFC-187 S9): a nested
		// grouping key must not be satisfied by an ordering on a same-leaf-named
		// top-level column.
		key, ok := values.AccessorNamePathKey(gk)
		if !ok {
			// Non-column / ambiguous grouping key — can't match ordering parts.
			return nil
		}
		groupKeyMap[key] = groupKeyEntry{index: i, value: gk}
	}

	consumed := make([]bool, len(groupingKeys))
	parts := make([]properties.RequestedOrderingPart, 0, len(groupingKeys))

	for _, p := range reqOrd.GetParts() {
		key, ok := values.AccessorNamePathKey(p.Value)
		if !ok {
			return nil
		}
		entry, found := groupKeyMap[key]
		if !found {
			// Ordering part doesn't match any grouping key — incompatible.
			return nil
		}
		if consumed[entry.index] {
			// Duplicate ordering part referencing same grouping key.
			return nil
		}
		consumed[entry.index] = true
		// Push the GROUPING KEY's Value, not the requested part's. The
		// requested part is stated in the aggregate's OUTPUT row (an ORDER BY
		// above a GROUP BY addresses output slots); the child below this
		// quantifier speaks the INPUT row. Java does this as an explicit
		// translation before it matches at all —
		// PushRequestedOrderingThroughGroupByRule.java:108-110 pushes the
		// requested ordering down through the group-by's result value ("we need
		// to do that in any case") and :152-153 then matches the PUSHED part
		// against the grouping values by Value equality, so the part it pushes
		// IS the grouping value.
		//
		// Pushing the output-space Value instead leaves the ordering match at
		// the access path with two values from two different rows, reconcilable
		// only by their spelling — and once both sides state an ordinal, that
		// spelling bridge is exactly what an ordinal-across-layouts conflation
		// hides behind.
		parts = append(parts, properties.RequestedOrderingPart{
			Value:     entry.value,
			SortOrder: p.SortOrder,
		})
	}

	// Append remaining grouping keys with ANY sort order.
	for i, gk := range groupingKeys {
		if !consumed[i] {
			parts = append(parts, properties.RequestedOrderingPart{
				Value:     gk,
				SortOrder: properties.RequestedSortOrderAny,
			})
		}
	}

	return properties.NewRequestedOrdering(parts, reqOrd.GetDistinctness(), reqOrd.IsExhaustive())
}

var _ ImplementationRule = (*PushRequestedOrderingThroughGroupByRule)(nil)
