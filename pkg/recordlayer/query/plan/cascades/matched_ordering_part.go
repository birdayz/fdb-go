package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// MatchedSortOrder represents the sort direction assigned during index
// matching. Mirrors Java's OrderingPart.MatchedSortOrder. All values
// are directional (IsDirectional always returns true).
//
// The naming "ascending" / "descending" is conventional: ascending
// becomes actual ascending with a forward scan; with a reverse scan
// it flips. The only semantic invariant is that ascending and
// descending are polar opposites.
type MatchedSortOrder int

const (
	MatchedSortOrderAscending MatchedSortOrder = iota
	MatchedSortOrderDescending
	MatchedSortOrderAscendingNullsLast
	MatchedSortOrderDescendingNullsFirst
)

// IsDirectional returns true. All MatchedSortOrder values are
// directional by definition (unlike ProvidedSortOrder which has FIXED
// and CHOOSE).
func (s MatchedSortOrder) IsDirectional() bool { return true }

// IsAnyAscending reports whether this sort order is any ascending
// variant.
func (s MatchedSortOrder) IsAnyAscending() bool {
	return s == MatchedSortOrderAscending || s == MatchedSortOrderAscendingNullsLast
}

// IsAnyDescending reports whether this sort order is any descending
// variant.
func (s MatchedSortOrder) IsAnyDescending() bool {
	return s == MatchedSortOrderDescending || s == MatchedSortOrderDescendingNullsFirst
}

// String returns a human-readable label for the sort order.
func (s MatchedSortOrder) String() string {
	switch s {
	case MatchedSortOrderAscending:
		return "ASCENDING"
	case MatchedSortOrderDescending:
		return "DESCENDING"
	case MatchedSortOrderAscendingNullsLast:
		return "ASCENDING_NULLS_LAST"
	case MatchedSortOrderDescendingNullsFirst:
		return "DESCENDING_NULLS_FIRST"
	default:
		return fmt.Sprintf("MatchedSortOrder(%d)", int(s))
	}
}

// ArrowIndicator returns a directional arrow for pretty-printing,
// matching Java's getArrowIndicator(). Uses the same Unicode arrows
// as ProvidedSortOrder for the corresponding direction.
func (s MatchedSortOrder) ArrowIndicator() string {
	switch s {
	case MatchedSortOrderAscending:
		return "↑" // ↑
	case MatchedSortOrderDescending:
		return "↓" // ↓
	case MatchedSortOrderAscendingNullsLast:
		return "↗" // ↗
	case MatchedSortOrderDescendingNullsFirst:
		return "↙" // ↙
	default:
		return "?"
	}
}

// ToProvidedSortOrder maps this matched sort order to the
// corresponding ProvidedSortOrder. When isReverse is true, the
// direction is flipped (ascending becomes descending, etc.).
//
// The mapping is by direction (Java's Direction enum), not by Go
// constant name. Both enums use the same iota ordering that maps to
// the same underlying Direction values.
func (s MatchedSortOrder) ToProvidedSortOrder(isReverse bool) ProvidedSortOrder {
	if isReverse {
		switch s {
		case MatchedSortOrderAscending:
			return ProvidedSortOrderDescending
		case MatchedSortOrderDescending:
			return ProvidedSortOrderAscending
		case MatchedSortOrderAscendingNullsLast:
			return ProvidedSortOrderDescendingNullsLast
		case MatchedSortOrderDescendingNullsFirst:
			return ProvidedSortOrderAscendingNullsFirst
		}
	}
	switch s {
	case MatchedSortOrderAscending:
		return ProvidedSortOrderAscending
	case MatchedSortOrderDescending:
		return ProvidedSortOrderDescending
	case MatchedSortOrderAscendingNullsLast:
		return ProvidedSortOrderAscendingNullsFirst
	case MatchedSortOrderDescendingNullsFirst:
		return ProvidedSortOrderDescendingNullsLast
	}
	return ProvidedSortOrderAscending
}

// MatchedOrderingPart is an OrderingPart that has been bound by a
// comparison during graph matching. It stores the parameter
// identifier, the value being ordered, the comparison range that
// was matched (equality/inequality/empty), and the matched sort
// direction.
//
// Mirrors Java's OrderingPart.MatchedOrderingPart.
type MatchedOrderingPart struct {
	parameterId      values.CorrelationIdentifier
	value            values.Value
	comparisonRange  *predicates.ComparisonRange
	matchedSortOrder MatchedSortOrder
}

// NewMatchedOrderingPart creates a MatchedOrderingPart. If
// comparisonRange is nil, it defaults to the empty (universe) range.
// Mirrors Java's MatchedOrderingPart.of() factory.
func NewMatchedOrderingPart(
	parameterId values.CorrelationIdentifier,
	value values.Value,
	comparisonRange *predicates.ComparisonRange,
	matchedSortOrder MatchedSortOrder,
) *MatchedOrderingPart {
	if comparisonRange == nil {
		comparisonRange = predicates.EmptyComparisonRange()
	}
	return &MatchedOrderingPart{
		parameterId:      parameterId,
		value:            value,
		comparisonRange:  comparisonRange,
		matchedSortOrder: matchedSortOrder,
	}
}

// GetParameterId returns the correlation identifier for this ordering
// part in the match candidate.
func (m *MatchedOrderingPart) GetParameterId() values.CorrelationIdentifier {
	return m.parameterId
}

// GetValue returns the value being ordered by.
func (m *MatchedOrderingPart) GetValue() values.Value {
	return m.value
}

// GetComparisonRange returns the comparison range.
func (m *MatchedOrderingPart) GetComparisonRange() *predicates.ComparisonRange {
	return m.comparisonRange
}

// GetComparisonRangeType delegates to the comparison range's
// GetRangeType.
func (m *MatchedOrderingPart) GetComparisonRangeType() predicates.ComparisonRangeType {
	return m.comparisonRange.GetRangeType()
}

// GetMatchedSortOrder returns the matched sort direction.
func (m *MatchedOrderingPart) GetMatchedSortOrder() MatchedSortOrder {
	return m.matchedSortOrder
}

// Demote converts an equality-bound ordering part to an empty
// (universe) range, preserving the parameter ID, value, and sort
// order. Panics if the comparison range is not an equality range.
// Returns a new MatchedOrderingPart (immutable pattern).
//
// This is used during ordering satisfaction when an equality-bound
// prefix part can be "demoted" to allow sorting on later parts.
func (m *MatchedOrderingPart) Demote() *MatchedOrderingPart {
	if !m.comparisonRange.IsEquality() {
		panic("MatchedOrderingPart.Demote: comparison range is not equality")
	}
	return &MatchedOrderingPart{
		parameterId:      m.parameterId,
		value:            m.value,
		comparisonRange:  predicates.EmptyComparisonRange(),
		matchedSortOrder: m.matchedSortOrder,
	}
}

// String returns a human-readable representation.
func (m *MatchedOrderingPart) String() string {
	return fmt.Sprintf("%s%s", values.ExplainValue(m.value), m.matchedSortOrder.ArrowIndicator())
}
