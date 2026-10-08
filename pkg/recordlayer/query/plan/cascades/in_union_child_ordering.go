package cascades

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// InUnionChildOrderingError reports an extracted in-union whose child does not
// provide the order its comparison keys merge on. The in-union rule ranges
// over one ordering partition, every member of which provides that order, so
// this is a planner defect: the merge would return rows out of order and drop
// the wrong ties. It is returned from planning, wrapped as a
// PlannerInvariantViolationError, and never accepted as a plan (RFC-257 WS-F
// 4.3 item 3).
type InUnionChildOrderingError struct {
	// ComparisonKeys are the in-union's comparison keys, rendered.
	ComparisonKeys []string
	// Reverse is the merge direction.
	Reverse bool
	// ChildOrdering is the extracted child's ordering keys and bindings.
	ChildOrdering string
}

func (e *InUnionChildOrderingError) Error() string {
	direction := "ascending"
	if e.Reverse {
		direction = "descending"
	}
	return fmt.Sprintf("in-union child does not provide its comparison keys (%s, %s); child ordering %s",
		strings.Join(e.ComparisonKeys, ", "), direction, e.ChildOrdering)
}

// checkInUnionChildOrdering verifies, for an in-union rebuilt over its
// extracted child, that the child's ordering satisfies the in-union's
// comparison keys in the merge direction. Any other expression passes.
func checkInUnionChildOrdering(e expressions.RelationalExpression) error {
	inUnion, ok := e.(*plans.RecordQueryInUnionPlan)
	if !ok || len(inUnion.GetComparisonKeys()) == 0 {
		return nil
	}
	keys := inUnion.GetComparisonKeys()
	ordering := properties.EmptyOrdering()
	if ref := inUnion.GetInnerQuantifier().GetRangesOver(); ref != nil {
		if members := ref.AllMembers(); len(members) == 1 {
			if child, isPlan := members[0].(physicalPlanExpression); isPlan {
				ordering = computeWrapperRichOrdering(child)
			}
		}
	}
	sortOrder := properties.RequestedSortOrderAscending
	if inUnion.IsReverse() {
		sortOrder = properties.RequestedSortOrderDescending
	}
	parts := make([]properties.RequestedOrderingPart, len(keys))
	for i, key := range keys {
		parts[i] = properties.RequestedOrderingPart{Value: key, SortOrder: sortOrder}
	}
	if ordering.Satisfies(properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false)) {
		return nil
	}
	rendered := make([]string, len(keys))
	for i, key := range keys {
		rendered[i] = values.ExplainValue(key)
	}
	return &PlannerInvariantViolationError{Cause: &InUnionChildOrderingError{
		ComparisonKeys: rendered,
		Reverse:        inUnion.IsReverse(),
		ChildOrdering:  describeRichOrdering(ordering),
	}}
}

// describeRichOrdering renders an ordering's keys with their bindings.
func describeRichOrdering(o *properties.RichOrdering) string {
	if o == nil || len(o.GetKeys()) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(o.GetKeys()))
	for _, key := range o.GetKeys() {
		_, bindings, _ := o.BindingsFor(key)
		parts = append(parts, values.ExplainValue(key)+" "+providedSortOrderName(properties.SortOrderOf(bindings)))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func providedSortOrderName(s properties.ProvidedSortOrder) string {
	switch s {
	case properties.ProvidedSortOrderAscending:
		return "ASC"
	case properties.ProvidedSortOrderDescending:
		return "DESC"
	case properties.ProvidedSortOrderAscendingNullsLast:
		return "ASC NULLS LAST"
	case properties.ProvidedSortOrderDescendingNullsFirst:
		return "DESC NULLS FIRST"
	case properties.ProvidedSortOrderFixed:
		return "FIXED"
	case properties.ProvidedSortOrderChoose:
		return "CHOOSE"
	}
	return fmt.Sprintf("sort order %d", int(s))
}
