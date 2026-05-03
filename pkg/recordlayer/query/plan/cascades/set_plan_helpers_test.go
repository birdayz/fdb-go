package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestResolveComparisonDirection_AllDescending(t *testing.T) {
	t.Parallel()
	parts := []ProvidedOrderingPart{
		{Value: fieldVal("a"), SortOrder: ProvidedSortOrderDescending},
		{Value: fieldVal("b"), SortOrder: ProvidedSortOrderDescending},
	}
	if !ResolveComparisonDirection(parts) {
		t.Fatal("all descending should be reverse")
	}
}

func TestResolveComparisonDirection_MixedDirections(t *testing.T) {
	t.Parallel()
	parts := []ProvidedOrderingPart{
		{Value: fieldVal("a"), SortOrder: ProvidedSortOrderAscending},
		{Value: fieldVal("b"), SortOrder: ProvidedSortOrderDescending},
	}
	if ResolveComparisonDirection(parts) {
		t.Fatal("mixed directions should not be reverse")
	}
}

func TestResolveComparisonDirection_AllFixed(t *testing.T) {
	t.Parallel()
	parts := []ProvidedOrderingPart{
		{Value: fieldVal("a"), SortOrder: ProvidedSortOrderFixed},
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
	parts := []ProvidedOrderingPart{
		{Value: a, SortOrder: ProvidedSortOrderAscending},
		{Value: b, SortOrder: ProvidedSortOrderFixed},
	}
	adjusted := AdjustFixedBindings(parts, false)
	if adjusted[0].SortOrder != ProvidedSortOrderAscending {
		t.Fatal("non-fixed should stay ascending")
	}
	if adjusted[1].SortOrder != ProvidedSortOrderAscending {
		t.Fatal("fixed should become ascending when not reverse")
	}
}

func TestAdjustFixedBindings_ReverseDirection(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	b := &values.FieldValue{Field: "b", Typ: values.UnknownType}
	parts := []ProvidedOrderingPart{
		{Value: a, SortOrder: ProvidedSortOrderDescending},
		{Value: b, SortOrder: ProvidedSortOrderFixed},
	}
	adjusted := AdjustFixedBindings(parts, true)
	if adjusted[0].SortOrder != ProvidedSortOrderDescending {
		t.Fatal("non-fixed should stay descending")
	}
	if adjusted[1].SortOrder != ProvidedSortOrderDescending {
		t.Fatal("fixed should become descending when reverse")
	}
}
