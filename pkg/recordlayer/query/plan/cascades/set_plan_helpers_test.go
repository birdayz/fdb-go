package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// fieldVal builds an untyped field reference for ordering tests. The
// properties package's ordering tests carry an identical copy: unexported
// test helpers cannot cross a package boundary, and a three-line
// constructor is not worth an exported testutil package.
func fieldVal(name string) values.Value {
	return &values.FieldValue{Field: name, Typ: values.UnknownType}
}

func TestResolveComparisonDirection_AllDescending(t *testing.T) {
	t.Parallel()
	parts := []properties.ProvidedOrderingPart{
		{Value: fieldVal("a"), SortOrder: properties.ProvidedSortOrderDescending},
		{Value: fieldVal("b"), SortOrder: properties.ProvidedSortOrderDescending},
	}
	if !ResolveComparisonDirection(parts) {
		t.Fatal("all descending should be reverse")
	}
}

func TestResolveComparisonDirection_MixedDirections(t *testing.T) {
	t.Parallel()
	parts := []properties.ProvidedOrderingPart{
		{Value: fieldVal("a"), SortOrder: properties.ProvidedSortOrderAscending},
		{Value: fieldVal("b"), SortOrder: properties.ProvidedSortOrderDescending},
	}
	if ResolveComparisonDirection(parts) {
		t.Fatal("mixed directions should not be reverse")
	}
}

func TestResolveComparisonDirection_AllFixed(t *testing.T) {
	t.Parallel()
	parts := []properties.ProvidedOrderingPart{
		{Value: fieldVal("a"), SortOrder: properties.ProvidedSortOrderFixed},
	}
	if ResolveComparisonDirection(parts) {
		t.Fatal("all fixed should not be reverse")
	}
}

func TestResolveComparisonDirection_Empty(t *testing.T) {
	t.Parallel()
	if ResolveComparisonDirection(nil) {
		t.Fatal("empty should not be reverse")
	}
}

func TestAdjustFixedBindings_ForwardDirection(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	b := &values.FieldValue{Field: "b", Typ: values.UnknownType}
	parts := []properties.ProvidedOrderingPart{
		{Value: a, SortOrder: properties.ProvidedSortOrderAscending},
		{Value: b, SortOrder: properties.ProvidedSortOrderFixed},
	}
	adjusted := AdjustFixedBindings(parts, false)
	if adjusted[0].SortOrder != properties.ProvidedSortOrderAscending {
		t.Fatal("non-fixed should stay ascending")
	}
	if adjusted[1].SortOrder != properties.ProvidedSortOrderAscending {
		t.Fatal("fixed should become ascending when not reverse")
	}
}

func TestAdjustFixedBindings_ReverseDirection(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	b := &values.FieldValue{Field: "b", Typ: values.UnknownType}
	parts := []properties.ProvidedOrderingPart{
		{Value: a, SortOrder: properties.ProvidedSortOrderDescending},
		{Value: b, SortOrder: properties.ProvidedSortOrderFixed},
	}
	adjusted := AdjustFixedBindings(parts, true)
	if adjusted[0].SortOrder != properties.ProvidedSortOrderDescending {
		t.Fatal("non-fixed should stay descending")
	}
	if adjusted[1].SortOrder != properties.ProvidedSortOrderDescending {
		t.Fatal("fixed should become descending when reverse")
	}
}
