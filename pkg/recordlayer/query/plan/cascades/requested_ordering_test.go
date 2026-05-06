package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestRequestedOrdering_NewAndAccessors(t *testing.T) {
	t.Parallel()
	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	fieldB := &values.FieldValue{Field: "b", Typ: values.UnknownType}
	parts := []RequestedOrderingPart{
		{Value: fieldA, SortOrder: RequestedSortOrderAscending},
		{Value: fieldB, SortOrder: RequestedSortOrderDescending},
	}
	ro := NewRequestedOrdering(parts, DistinctnessDistinct, true)

	if ro.Size() != 2 {
		t.Fatalf("expected 2 parts, got %d", ro.Size())
	}
	if !ro.IsDistinct() {
		t.Fatal("expected distinct")
	}
	if !ro.IsExhaustive() {
		t.Fatal("expected exhaustive")
	}
	if ro.IsPreserve() {
		t.Fatal("expected not preserve")
	}
	if ro.GetDistinctness() != DistinctnessDistinct {
		t.Fatalf("expected distinct, got %d", ro.GetDistinctness())
	}
}

func TestRequestedOrdering_Preserve(t *testing.T) {
	t.Parallel()
	ro := PreserveOrdering()
	if !ro.IsPreserve() {
		t.Fatal("expected preserve")
	}
	if ro.Size() != 0 {
		t.Fatalf("expected 0 parts, got %d", ro.Size())
	}
	if ro.IsDistinct() {
		t.Fatal("expected not distinct")
	}
}

func TestRequestedOrdering_GetValueRequestedSortOrderMap(t *testing.T) {
	t.Parallel()
	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	fieldB := &values.FieldValue{Field: "b", Typ: values.UnknownType}
	parts := []RequestedOrderingPart{
		{Value: fieldA, SortOrder: RequestedSortOrderAscending},
		{Value: fieldB, SortOrder: RequestedSortOrderDescending},
	}
	ro := NewRequestedOrdering(parts, DistinctnessNotDistinct, false)

	m := ro.GetValueRequestedSortOrderMap()
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if m[fieldA] != RequestedSortOrderAscending {
		t.Fatal("expected ascending for fieldA")
	}
	if m[fieldB] != RequestedSortOrderDescending {
		t.Fatal("expected descending for fieldB")
	}
}

func TestRequestedOrdering_CopiesParts(t *testing.T) {
	t.Parallel()
	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	parts := []RequestedOrderingPart{
		{Value: fieldA, SortOrder: RequestedSortOrderAscending},
	}
	ro := NewRequestedOrdering(parts, DistinctnessNotDistinct, false)

	parts[0].SortOrder = RequestedSortOrderDescending
	if ro.GetParts()[0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("modifying original parts should not affect RequestedOrdering")
	}
}

func TestRequestedSortOrder_IsDirectional(t *testing.T) {
	t.Parallel()
	if RequestedSortOrderAny.IsDirectional() {
		t.Fatal("ANY should not be directional")
	}
	if !RequestedSortOrderAscending.IsDirectional() {
		t.Fatal("ASCENDING should be directional")
	}
	if !RequestedSortOrderDescending.IsDirectional() {
		t.Fatal("DESCENDING should be directional")
	}
}
