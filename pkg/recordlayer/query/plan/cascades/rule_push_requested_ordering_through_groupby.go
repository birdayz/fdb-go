package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

	var synthesized []*RequestedOrdering
	for _, reqOrd := range orderings {
		if reqOrd.IsPreserve() {
			// Preserve ordering: push the grouping keys with ANY sort
			// order. Scalar aggregation (no grouping keys) trivially
			// satisfies any ordering.
			if len(groupingKeys) == 0 {
				synthesized = append(synthesized, PreserveOrdering())
			} else {
				parts := make([]RequestedOrderingPart, len(groupingKeys))
				for i, gk := range groupingKeys {
					parts[i] = RequestedOrderingPart{
						Value:     gk,
						SortOrder: RequestedSortOrderAny,
					}
				}
				synthesized = append(synthesized, NewRequestedOrdering(parts, DistinctnessPreserveDistinctness, false))
			}
			continue
		}

		if len(groupingKeys) == 0 {
			// No grouping keys — scalar aggregation produces 0-1 rows.
			// Any ordering is trivially satisfied.
			synthesized = append(synthesized, PreserveOrdering())
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
func synthesizeGroupByOrdering(reqOrd *RequestedOrdering, groupingKeys []values.Value) *RequestedOrdering {
	// Build a map of grouping key field names for quick lookup.
	type groupKeyEntry struct {
		index int
		value values.Value
	}
	groupKeyMap := make(map[string]groupKeyEntry, len(groupingKeys))
	for i, gk := range groupingKeys {
		fv, ok := gk.(*values.FieldValue)
		if !ok {
			// Non-FieldValue grouping key — can't match ordering parts.
			return nil
		}
		groupKeyMap[strings.ToUpper(fv.Field)] = groupKeyEntry{index: i, value: gk}
	}

	consumed := make([]bool, len(groupingKeys))
	parts := make([]RequestedOrderingPart, 0, len(groupingKeys))

	for _, p := range reqOrd.GetParts() {
		fv, ok := p.Value.(*values.FieldValue)
		if !ok {
			return nil
		}
		entry, found := groupKeyMap[strings.ToUpper(fv.Field)]
		if !found {
			// Ordering part doesn't match any grouping key — incompatible.
			return nil
		}
		if consumed[entry.index] {
			// Duplicate ordering part referencing same grouping key.
			return nil
		}
		consumed[entry.index] = true
		parts = append(parts, p)
	}

	// Append remaining grouping keys with ANY sort order.
	for i, gk := range groupingKeys {
		if !consumed[i] {
			parts = append(parts, RequestedOrderingPart{
				Value:     gk,
				SortOrder: RequestedSortOrderAny,
			})
		}
	}

	return NewRequestedOrdering(parts, reqOrd.GetDistinctness(), reqOrd.IsExhaustive())
}

var _ ImplementationRule = (*PushRequestedOrderingThroughGroupByRule)(nil)
