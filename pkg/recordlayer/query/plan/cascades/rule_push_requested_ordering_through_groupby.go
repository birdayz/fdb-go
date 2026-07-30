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

// synthesizeGroupByOrdering translates the requested ordering into the child's
// row space, requires every translated part to be one of the grouping keys, and
// returns the ordering covering all of them. nil means incompatible — do not
// push.
//
// THE MATCH IS STILL BY ACCESSOR NAME PATH, and converting it to Java's shape is
// BLOCKED — on a divergence one level down, which is worth stating precisely
// because it looks like a matching problem and is not.
//
// Java translates before it matches. It pushes the request through the group-by's
// result value unconditionally (:108-110, "we need to do that in any case") and
// then matches the PUSHED part against the grouping values by Value equality
// (:152-153), which works because Java's result value IS the aggregate's output
// row: GroupByExpression.getResultValue() returns
// resultValueFunction.apply(groupingValue, aggregateValue)
// (GroupByExpression.java:129), a RecordConstructorValue of the grouping and
// aggregate columns (:756-759). Pushing an output-slot reference through that
// yields the grouping value, so the two sides meet as one Value.
//
// Go's GroupByExpression.GetResultValue() returns e.inner.GetFlowedObjectValue()
// — the INPUT row, not the output row. So RequestedOrdering.PushDownThroughValue
// through it is the IDENTITY, measured over the corpus: a request for
// CUSTOMER_ID#0 (output slot 0) pushes to CUSTOMER_ID#0 and the grouping key is
// CUSTOMER_ID#1 (input slot 1). Value equality then fails on every group-by in
// the corpus, the rule pushes nothing, and the index that served the grouping
// order is replaced by a materialized sort —
// SELECT customer_id, COUNT(*) FROM orders WHERE status = 'shipped'
// GROUP BY customer_id ORDER BY customer_id loses
// Fetch(IndexScan(IDX_CUST_STATUS)) for InMemorySort(Scan(ORDERS)).
//
// So the prerequisite is making GetResultValue() return the output row as Java
// does. That changes what every consumer of the group-by's result value sees —
// its result type and the space downstream references are baked against — so it
// is its own reviewed unit, not a rider on a comparator change. Until then the
// name-path match stays, and it is the one remaining name comparison on this
// path; the test named for this records what unblocks it.
//
// Java's DISTINCTNESS GATE AT :161 IS DELIBERATELY NOT PORTED, and that is a
// finding rather than an omission. The gate reads
// `!pushedRequestedOrdering.isDistinct() || requiredOrderingValues.isEmpty()`,
// and its left disjunct is a tautology: `pushedRequestedOrdering` is the result
// of `RequestedOrdering.pushDown`, whose ONLY implementation returns either
// `preserve()` (RequestedOrdering.java:289) or an ordering built with
// `Distinctness.PRESERVE_DISTINCTNESS` (:236). So `isDistinct()` is false on
// every value that reaches :161, the gate always admits, and porting it would
// add a branch that is dead in Java. It would not be dead HERE, though, which is
// the point — it only looks live because Go used to propagate the ORIGINAL
// request's distinctness into the synthesized ordering. Taking the distinctness
// from the pushed ordering, as Java does, is what makes the gate vacuous in Go
// too, and that is the divergence actually worth closing.
func synthesizeGroupByOrdering(
	reqOrd *properties.RequestedOrdering,
	groupingKeys []values.Value,
) *properties.RequestedOrdering {
	// Key by full accessor path, not leaf name (RFC-187 S9): a nested grouping
	// key must not be satisfied by an ordering on a same-leaf-named top-level
	// column.
	type groupKeyEntry struct {
		index int
		value values.Value
	}
	groupKeyMap := make(map[string]groupKeyEntry, len(groupingKeys))
	for i, gk := range groupingKeys {
		key, ok := values.AccessorNamePathKey(gk)
		if !ok {
			// Non-column / ambiguous grouping key — can't match ordering parts.
			return nil
		}
		groupKeyMap[key] = groupKeyEntry{index: i, value: gk}
	}

	// Java :148-158: requiredOrderingValues is a MUTABLE set of the grouping
	// values, and each part must REMOVE one from it. A part matching nothing
	// makes the pushed and required orderings incompatible, and a second part
	// matching an already-consumed key fails the same way — a LinkedHashSet
	// cannot yield the same element twice.
	consumed := make([]bool, len(groupingKeys))
	parts := make([]properties.RequestedOrderingPart, 0, len(groupingKeys))
	for _, p := range reqOrd.GetParts() {
		key, ok := values.AccessorNamePathKey(p.Value)
		if !ok {
			return nil
		}
		entry, found := groupKeyMap[key]
		if !found || consumed[entry.index] {
			return nil
		}
		consumed[entry.index] = true
		// Push the GROUPING KEY's Value, not the requested part's: the part is
		// stated in the OUTPUT row and the child speaks the INPUT row. Java adds
		// the PUSHED part at :153, which is the same value because its pushDown
		// really translated; here the grouping key is the only one of the two
		// that is in the child's space at all.
		parts = append(parts, properties.RequestedOrderingPart{
			Value:     entry.value,
			SortOrder: p.SortOrder,
		})
	}

	// Java :162-166: the grouping keys no pushed part claimed, in the order they
	// were written, with ANY sort order — streaming aggregation needs them
	// contiguous, not in a particular direction.
	for i, gk := range groupingKeys {
		if !consumed[i] {
			parts = append(parts, properties.RequestedOrderingPart{
				Value:     gk,
				SortOrder: properties.RequestedSortOrderAny,
			})
		}
	}

	// Java :167-168: the distinctness of the PUSHED ordering (always
	// PRESERVE_DISTINCTNESS, see the gate note above) and exhaustive=false,
	// hardcoded there. Go previously forwarded the ORIGINAL request's
	// distinctness and its exhaustive flag, so a DISTINCT or exhaustive request
	// above a GROUP BY imposed a constraint on the child that Java never
	// imposes.
	return properties.NewRequestedOrdering(
		parts, properties.DistinctnessPreserveDistinctness, false)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughGroupByRule)(nil)
