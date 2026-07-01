package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ProvidedSortOrder represents the sort direction (and NULL placement)
// of a provided ordering part. Mirrors Java's
// OrderingPart.ProvidedSortOrder — exactly 6 constants, each directional
// value mapping to a TupleOrdering.Direction:
//
//	Ascending            -> ASC_NULLS_FIRST  (counterflow=false)
//	Descending           -> DESC_NULLS_LAST  (counterflow=false)
//	AscendingNullsLast   -> ASC_NULLS_LAST   (counterflow=true)
//	DescendingNullsFirst -> DESC_NULLS_FIRST (counterflow=true)
//	Fixed / Choose       -> non-directional
type ProvidedSortOrder int

const (
	ProvidedSortOrderAscending ProvidedSortOrder = iota
	ProvidedSortOrderDescending
	ProvidedSortOrderAscendingNullsLast
	ProvidedSortOrderDescendingNullsFirst
	ProvidedSortOrderFixed
	ProvidedSortOrderChoose
)

func (s ProvidedSortOrder) IsDirectional() bool {
	switch s {
	case ProvidedSortOrderAscending, ProvidedSortOrderDescending,
		ProvidedSortOrderAscendingNullsLast, ProvidedSortOrderDescendingNullsFirst:
		return true
	default:
		return false
	}
}

// IsAnyAscending reports whether this is an ascending direction (nulls
// first or last). Mirrors Java SortOrder.isAnyAscending().
func (s ProvidedSortOrder) IsAnyAscending() bool {
	return s == ProvidedSortOrderAscending || s == ProvidedSortOrderAscendingNullsLast
}

func (s ProvidedSortOrder) IsAnyDescending() bool {
	return s == ProvidedSortOrderDescending || s == ProvidedSortOrderDescendingNullsFirst
}

// IsCounterflowNulls reports whether the NULL placement runs against the
// natural tuple order for this direction. Mirrors Java
// TupleOrdering.Direction.isCounterflowNulls(). Only valid for
// directional values.
func (s ProvidedSortOrder) IsCounterflowNulls() bool {
	return s == ProvidedSortOrderAscendingNullsLast || s == ProvidedSortOrderDescendingNullsFirst
}

// ToRequestedSortOrder maps a provided sort order to the requested sort
// order with the same TupleOrdering.Direction (preserving NULL
// placement), mirroring Java ProvidedSortOrder.toRequestedSortOrder()
// (a by-Direction map, not a collapse to the natural order).
func (s ProvidedSortOrder) ToRequestedSortOrder() RequestedSortOrder {
	switch s {
	case ProvidedSortOrderAscending:
		return RequestedSortOrderAscending
	case ProvidedSortOrderDescending:
		return RequestedSortOrderDescending
	case ProvidedSortOrderAscendingNullsLast:
		return RequestedSortOrderAscendingNullsLast
	case ProvidedSortOrderDescendingNullsFirst:
		return RequestedSortOrderDescendingNullsFirst
	default:
		return RequestedSortOrderAny
	}
}

// ProvidedOrderingPart is a (Value, ProvidedSortOrder) pair for a
// provided ordering element.
type ProvidedOrderingPart struct {
	Value     values.Value
	SortOrder ProvidedSortOrder
}

// OrderingBinding represents a binding in the ordering: either a fixed
// comparison binding or a directional sort binding. Mirrors Java's
// Ordering.Binding.
type OrderingBinding struct {
	kind       OrderingBindingKind
	sortOrder  ProvidedSortOrder
	comparison any // *predicates.Comparison or nil for sorted bindings
}

type OrderingBindingKind int

const (
	OrderingBindingSorted OrderingBindingKind = iota
	OrderingBindingFixed
	OrderingBindingChoose
)

// SortedBinding creates a sorted ordering binding.
func SortedBinding(sortOrder ProvidedSortOrder) OrderingBinding {
	return OrderingBinding{kind: OrderingBindingSorted, sortOrder: sortOrder}
}

// FixedBinding creates a fixed (equality-bound) ordering binding.
func FixedBinding(comparison any) OrderingBinding {
	return OrderingBinding{kind: OrderingBindingFixed, comparison: comparison}
}

// ChooseBinding creates a binding where the sort order will be chosen
// during enumeration.
func ChooseBinding() OrderingBinding {
	return OrderingBinding{kind: OrderingBindingChoose}
}

func (b OrderingBinding) IsSorted() bool { return b.kind == OrderingBindingSorted }
func (b OrderingBinding) IsFixed() bool  { return b.kind == OrderingBindingFixed }
func (b OrderingBinding) IsChoose() bool { return b.kind == OrderingBindingChoose }

func (b OrderingBinding) GetSortOrder() ProvidedSortOrder { return b.sortOrder }
func (b OrderingBinding) GetComparison() any              { return b.comparison }
