package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ProvidedSortOrder represents the sort direction of a provided
// ordering part. Mirrors Java's OrderingPart.ProvidedSortOrder.
type ProvidedSortOrder int

const (
	ProvidedSortOrderAscending ProvidedSortOrder = iota
	ProvidedSortOrderDescending
	ProvidedSortOrderAscendingNullsFirst
	ProvidedSortOrderDescendingNullsLast
	ProvidedSortOrderFixed
	ProvidedSortOrderChoose
)

func (s ProvidedSortOrder) IsDirectional() bool {
	switch s {
	case ProvidedSortOrderAscending, ProvidedSortOrderDescending,
		ProvidedSortOrderAscendingNullsFirst, ProvidedSortOrderDescendingNullsLast:
		return true
	default:
		return false
	}
}

func (s ProvidedSortOrder) IsAnyDescending() bool {
	return s == ProvidedSortOrderDescending || s == ProvidedSortOrderDescendingNullsLast
}

func (s ProvidedSortOrder) ToRequestedSortOrder() RequestedSortOrder {
	if s.IsDirectional() {
		if s.IsAnyDescending() {
			return RequestedSortOrderDescending
		}
		return RequestedSortOrderAscending
	}
	return RequestedSortOrderAny
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
