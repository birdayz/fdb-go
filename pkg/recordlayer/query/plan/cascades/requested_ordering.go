package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RequestedSortOrder specifies the desired sort direction (and NULL
// placement) for one ordering part. Mirrors Java's
// OrderingPart.RequestedSortOrder, where each directional value maps to a
// TupleOrdering.Direction:
//
//	Ascending            -> ASC_NULLS_FIRST  (ascending,  nulls first; counterflow=false)
//	Descending           -> DESC_NULLS_LAST  (descending, nulls last;  counterflow=false)
//	AscendingNullsLast   -> ASC_NULLS_LAST   (ascending,  nulls last;  counterflow=true)
//	DescendingNullsFirst -> DESC_NULLS_FIRST (descending, nulls first; counterflow=true)
//
// "Counterflow nulls" means the requested NULL placement runs against the
// natural FDB tuple order for that direction (ASC defaults nulls-first,
// DESC defaults nulls-last). This is the property Java's
// ProvidedSortOrder.isCompatibleWithRequestedSortOrder gates on.
type RequestedSortOrder int

const (
	RequestedSortOrderAny RequestedSortOrder = iota
	RequestedSortOrderAscending
	RequestedSortOrderDescending
	RequestedSortOrderAscendingNullsLast
	RequestedSortOrderDescendingNullsFirst
)

func (s RequestedSortOrder) IsDirectional() bool {
	switch s {
	case RequestedSortOrderAscending, RequestedSortOrderDescending,
		RequestedSortOrderAscendingNullsLast, RequestedSortOrderDescendingNullsFirst:
		return true
	default:
		return false
	}
}

// IsAnyAscending reports whether this is an ascending direction (nulls
// first or last). Mirrors Java SortOrder.isAnyAscending().
func (s RequestedSortOrder) IsAnyAscending() bool {
	return s == RequestedSortOrderAscending || s == RequestedSortOrderAscendingNullsLast
}

// IsAnyDescending reports whether this is a descending direction (nulls
// first or last). Mirrors Java SortOrder.isAnyDescending().
func (s RequestedSortOrder) IsAnyDescending() bool {
	return s == RequestedSortOrderDescending || s == RequestedSortOrderDescendingNullsFirst
}

// IsCounterflowNulls reports whether the requested NULL placement runs
// against the natural tuple order for this direction. Mirrors Java
// TupleOrdering.Direction.isCounterflowNulls(). Only valid for
// directional values (Java verifies isDirectional first).
func (s RequestedSortOrder) IsCounterflowNulls() bool {
	return s == RequestedSortOrderAscendingNullsLast || s == RequestedSortOrderDescendingNullsFirst
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

// PartsEqual reports whether two requested-ordering part lists are equal
// element-wise: same Value (structural equality) and same sort order.
// Mirrors Java's List<RequestedOrderingPart>.equals in
// RequestedOrderingConstraint.combine.
func PartsEqual(a, b []RequestedOrderingPart) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SortOrder != b[i].SortOrder || !valuesEqual(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}

// CombineRequestedOrderings unions `new` into `current` with subsumption,
// porting Java RequestedOrderingConstraint.combine exactly
// (RequestedOrderingConstraint.java:38-76): a new ordering already
// subsumed by a current one (same distinctness; an exhaustive current
// subsumes any new whose parts extend its prefix; otherwise exact part
// equality) is dropped. ok=false means nothing new remained — the
// existing constraint already covers the push (Java's Optional.empty,
// where the caller leaves the map untouched).
func CombineRequestedOrderings(current, added []*RequestedOrdering) ([]*RequestedOrdering, bool) {
	var fresh []*RequestedOrdering
	for _, n := range added {
		subsumed := false
		for _, cur := range current {
			if n.IsDistinct() != cur.IsDistinct() {
				continue
			}
			if !cur.IsExhaustive() && n.IsExhaustive() {
				continue
			}
			if cur.IsExhaustive() && len(n.parts) >= len(cur.parts) {
				if PartsEqual(n.parts[:len(cur.parts)], cur.parts) {
					subsumed = true
					break
				}
				continue
			}
			if PartsEqual(n.parts, cur.parts) {
				subsumed = true
				break
			}
		}
		if !subsumed {
			// Java's newConstraint is a Set: exact duplicates WITHIN the
			// pushed batch collapse too.
			dup := false
			for _, f := range fresh {
				if f.IsDistinct() == n.IsDistinct() && f.IsExhaustive() == n.IsExhaustive() && PartsEqual(f.parts, n.parts) {
					dup = true
					break
				}
			}
			if !dup {
				fresh = append(fresh, n)
			}
		}
	}
	if len(fresh) == 0 {
		return current, false
	}
	out := make([]*RequestedOrdering, 0, len(current)+len(fresh))
	out = append(out, current...)
	out = append(out, fresh...)
	return out, true
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
