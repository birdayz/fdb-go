package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RequestedSortOrder specifies the desired sort direction for one
// ordering part. Mirrors Java's OrderingPart.RequestedSortOrder.
//
// Cross-enum NULL-variant asymmetry (intentional, faithful to Java): the three
// sort-order enums each carry only the TWO null variants they need, and the
// "extra two" are mirror-flips of each other —
//   - RequestedSortOrder / MatchedSortOrder carry the NON-natural explicit
//     variants: AscendingNullsLast, DescendingNullsFirst.
//   - ProvidedSortOrder (ordering_part.go) carries the NATURAL-explicit pair:
//     AscendingNullsFirst, DescendingNullsLast.
//
// So a provided AscendingNullsFirst mapping to nullsFirst()==true is correct, not
// a bug; the requested side only ever names a placement when it differs from the
// natural one (a plain Ascending already means ASC NULLS FIRST).
type RequestedSortOrder int

const (
	RequestedSortOrderAny RequestedSortOrder = iota
	// RequestedSortOrderAscending is ascending with the NATURAL null placement
	// (NULLS FIRST — the FDB forward-scan tuple order). Mirrors Java's ASCENDING.
	RequestedSortOrderAscending
	// RequestedSortOrderDescending is descending with the NATURAL null placement
	// (NULLS LAST). Mirrors Java's DESCENDING.
	RequestedSortOrderDescending
	// RequestedSortOrderAscendingNullsLast is ascending with the NON-natural
	// NULLS LAST placement (an explicit `ORDER BY x ASC NULLS LAST`). A forward
	// index scan provides ASC NULLS FIRST, so it does NOT satisfy this — the sort
	// must be retained. Mirrors Java's ASCENDING_NULLS_LAST.
	RequestedSortOrderAscendingNullsLast
	// RequestedSortOrderDescendingNullsFirst is descending with the non-natural
	// NULLS FIRST placement (`ORDER BY x DESC NULLS FIRST`). Mirrors Java's
	// DESCENDING_NULLS_FIRST.
	RequestedSortOrderDescendingNullsFirst
)

func (s RequestedSortOrder) IsDirectional() bool {
	return s == RequestedSortOrderAscending || s == RequestedSortOrderDescending ||
		s == RequestedSortOrderAscendingNullsLast || s == RequestedSortOrderDescendingNullsFirst
}

// IsAscending reports whether s is any ascending variant (natural or NULLS LAST).
func (s RequestedSortOrder) IsAscending() bool {
	return s == RequestedSortOrderAscending || s == RequestedSortOrderAscendingNullsLast
}

// IsDescending reports whether s is any descending variant (natural or NULLS FIRST).
func (s RequestedSortOrder) IsDescending() bool {
	return s == RequestedSortOrderDescending || s == RequestedSortOrderDescendingNullsFirst
}

// NullsFirst reports the requested NULL placement for a directional order: the
// natural placement is ASC→NULLS FIRST and DESC→NULLS LAST; the *NullsLast /
// *NullsFirst variants invert it. For Any / non-directional orders the result is
// unconstrained (reported as natural true); callers gate on IsDirectional first.
func (s RequestedSortOrder) NullsFirst() bool {
	switch s {
	case RequestedSortOrderAscending, RequestedSortOrderDescendingNullsFirst:
		return true
	case RequestedSortOrderDescending, RequestedSortOrderAscendingNullsLast:
		return false
	}
	return true
}

// RequestedOrderingPart is a (Value, RequestedSortOrder) pair specifying
// one element of a requested ordering. Mirrors Java's
// OrderingPart.RequestedOrderingPart.
type RequestedOrderingPart struct {
	Value     values.Value
	SortOrder RequestedSortOrder
}

// Distinctness specifies whether ordered records should be distinct.
type Distinctness int

const (
	DistinctnessDistinct Distinctness = iota
	DistinctnessNotDistinct
	DistinctnessPreserveDistinctness
)

// RequestedOrdering captures a desired sort order for a query's output.
// Used to communicate ordering requirements upward (toward sources)
// during planning. Mirrors Java's RequestedOrdering.
type RequestedOrdering struct {
	parts        []RequestedOrderingPart
	distinctness Distinctness
	exhaustive   bool
}

// NewRequestedOrdering creates a new RequestedOrdering.
func NewRequestedOrdering(parts []RequestedOrderingPart, d Distinctness, exhaustive bool) *RequestedOrdering {
	copied := make([]RequestedOrderingPart, len(parts))
	copy(copied, parts)
	return &RequestedOrdering{
		parts:        copied,
		distinctness: d,
		exhaustive:   exhaustive,
	}
}

// Preserve returns a RequestedOrdering that preserves the incoming order.
func PreserveOrdering() *RequestedOrdering {
	return &RequestedOrdering{
		distinctness: DistinctnessPreserveDistinctness,
	}
}

// IsPreserve returns true if this ordering requests preservation of the
// existing order (empty parts).
func (o *RequestedOrdering) IsPreserve() bool {
	return len(o.parts) == 0
}

// IsExhaustive returns true if we want all satisfying plans, not just
// the best.
func (o *RequestedOrdering) IsExhaustive() bool {
	return o.exhaustive
}

// Exhaustive returns a copy of this ordering with exhaustive=true. If
// already exhaustive, returns the receiver unchanged. Ports Java's
// RequestedOrdering.exhaustive().
func (o *RequestedOrdering) Exhaustive() *RequestedOrdering {
	if o.exhaustive {
		return o
	}
	return NewRequestedOrdering(o.parts, o.distinctness, true)
}

// IsDistinct returns true if the ordering requires distinct records.
func (o *RequestedOrdering) IsDistinct() bool {
	return o.distinctness == DistinctnessDistinct
}

// GetDistinctness returns the distinctness constraint.
func (o *RequestedOrdering) GetDistinctness() Distinctness {
	return o.distinctness
}

// GetParts returns the ordering parts.
func (o *RequestedOrdering) GetParts() []RequestedOrderingPart {
	return o.parts
}

// Size returns the number of ordering parts.
func (o *RequestedOrdering) Size() int {
	return len(o.parts)
}

// GetValueRequestedSortOrderMap returns a map from Value to its
// requested sort order.
func (o *RequestedOrdering) GetValueRequestedSortOrderMap() map[values.Value]RequestedSortOrder {
	m := make(map[values.Value]RequestedSortOrder, len(o.parts))
	for _, p := range o.parts {
		if existing, ok := m[p.Value]; ok {
			if existing != p.SortOrder {
				m[p.Value] = RequestedSortOrderAny
			}
		} else {
			m[p.Value] = p.SortOrder
		}
	}
	return m
}

// PushDownThroughValue translates this requested ordering's keys
// through a result value, expressing them in terms of the result
// value's inputs. Each ordering part's value is pushed down; sort
// orders are preserved. Parts that cannot be pushed down are dropped.
// If all parts are dropped, returns a preserve ordering.
//
// Ports Java's RequestedOrdering.pushDown(Value, CorrelationIdentifier,
// EvaluationContext, AliasMap, Set<CorrelationIdentifier>).
func (o *RequestedOrdering) PushDownThroughValue(resultValue values.Value, upperAlias values.CorrelationIdentifier) *RequestedOrdering {
	if o.IsPreserve() {
		return PreserveOrdering()
	}

	keyValues := make([]values.Value, len(o.parts))
	for i, p := range o.parts {
		keyValues[i] = p.Value
	}

	pushed := values.PushDownValues(keyValues, resultValue, upperAlias)

	var newParts []RequestedOrderingPart
	for i, p := range o.parts {
		if pushed[i] != nil {
			newParts = append(newParts, RequestedOrderingPart{
				Value:     pushed[i],
				SortOrder: p.SortOrder,
			})
		}
	}
	if len(newParts) == 0 {
		return PreserveOrdering()
	}
	return NewRequestedOrdering(newParts, DistinctnessPreserveDistinctness, o.exhaustive)
}
