package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/properties"

// ResolveComparisonDirection examines the comparison ordering parts
// and determines if the comparison is reverse (all descending).
// Returns true if all directional parts are descending.
// Mirrors Java's RecordQuerySetPlan.resolveComparisonDirection.
func ResolveComparisonDirection(parts []properties.ProvidedOrderingPart) bool {
	for _, p := range parts {
		if p.SortOrder.IsDirectional() && !p.SortOrder.IsAnyDescending() {
			return false
		}
	}
	for _, p := range parts {
		if p.SortOrder.IsDirectional() {
			return true
		}
	}
	return false
}

// AdjustFixedBindings adjusts the sort order of FIXED ordering parts
// to match the resolved comparison direction. If the comparison is
// reverse, fixed parts get descending; otherwise ascending.
// Mirrors Java's RecordQuerySetPlan.adjustFixedBindings.
func AdjustFixedBindings(parts []properties.ProvidedOrderingPart, isReverse bool) []properties.ProvidedOrderingPart {
	result := make([]properties.ProvidedOrderingPart, len(parts))
	for i, p := range parts {
		if p.SortOrder == properties.ProvidedSortOrderFixed {
			if isReverse {
				result[i] = properties.ProvidedOrderingPart{Value: p.Value, SortOrder: properties.ProvidedSortOrderDescending}
			} else {
				result[i] = properties.ProvidedOrderingPart{Value: p.Value, SortOrder: properties.ProvidedSortOrderAscending}
			}
		} else {
			result[i] = p
		}
	}
	return result
}
