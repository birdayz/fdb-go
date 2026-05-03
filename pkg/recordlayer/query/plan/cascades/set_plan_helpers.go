package cascades

// ResolveComparisonDirection examines the comparison ordering parts
// and determines if the comparison is reverse (all descending).
// Returns true if all directional parts are descending.
// Mirrors Java's RecordQuerySetPlan.resolveComparisonDirection.
func ResolveComparisonDirection(parts []ProvidedOrderingPart) bool {
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
func AdjustFixedBindings(parts []ProvidedOrderingPart, isReverse bool) []ProvidedOrderingPart {
	result := make([]ProvidedOrderingPart, len(parts))
	for i, p := range parts {
		if p.SortOrder == ProvidedSortOrderFixed {
			if isReverse {
				result[i] = ProvidedOrderingPart{Value: p.Value, SortOrder: ProvidedSortOrderDescending}
			} else {
				result[i] = ProvidedOrderingPart{Value: p.Value, SortOrder: ProvidedSortOrderAscending}
			}
		} else {
			result[i] = p
		}
	}
	return result
}
