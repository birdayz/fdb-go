package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RequestedSortOrder specifies the desired sort direction for one
// ordering part. Mirrors Java's OrderingPart.RequestedSortOrder.
type RequestedSortOrder int

const (
	RequestedSortOrderAny RequestedSortOrder = iota
	RequestedSortOrderAscending
	RequestedSortOrderDescending
)

func (s RequestedSortOrder) IsDirectional() bool {
	return s == RequestedSortOrderAscending || s == RequestedSortOrderDescending
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
