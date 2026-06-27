package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestConstraintMap_SetAndGet(t *testing.T) {
	t.Parallel()
	cm := NewConstraintMap()
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))

	orderings := []*RequestedOrdering{
		NewRequestedOrdering([]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		}, DistinctnessNotDistinct, false),
	}
	Set(cm, ref, RequestedOrderingConstraintKey, orderings)

	got, ok := Get(cm, ref, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("expected constraint to be set")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 ordering, got %d", len(got))
	}
}

func TestConstraintMap_GetMissing(t *testing.T) {
	t.Parallel()
	cm := NewConstraintMap()
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))

	got, ok := Get(cm, ref, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("expected no constraint")
	}
	if got != nil {
		t.Fatal("expected nil")
	}
}

func TestConstraintMap_NilMap(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))

	got, ok := Get[*ConstraintMap](nil, ref, &PlannerConstraint[*ConstraintMap]{})
	if ok {
		t.Fatal("nil map should return false")
	}
	if got != nil {
		t.Fatal("nil map should return nil")
	}

	Set[*ConstraintMap](nil, ref, &PlannerConstraint[*ConstraintMap]{}, nil)
}

func TestConstraintMap_DifferentRefs(t *testing.T) {
	t.Parallel()
	cm := NewConstraintMap()
	ref1 := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"A"}, nil))
	ref2 := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"B"}, nil))

	orderings1 := []*RequestedOrdering{PreserveOrdering()}
	orderings2 := []*RequestedOrdering{
		NewRequestedOrdering([]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, SortOrder: RequestedSortOrderDescending},
		}, DistinctnessDistinct, true),
	}

	Set(cm, ref1, RequestedOrderingConstraintKey, orderings1)
	Set(cm, ref2, RequestedOrderingConstraintKey, orderings2)

	got1, _ := Get(cm, ref1, RequestedOrderingConstraintKey)
	got2, _ := Get(cm, ref2, RequestedOrderingConstraintKey)

	if len(got1) != 1 || !got1[0].IsPreserve() {
		t.Fatal("ref1 should have preserve ordering")
	}
	if len(got2) != 1 || got2[0].IsPreserve() {
		t.Fatal("ref2 should have non-preserve ordering")
	}
}

func TestImplementationRuleCall_GetRequestedOrderings_NoConstraints(t *testing.T) {
	t.Parallel()
	call := &ImplementationRuleCall{
		Constraints: nil,
	}
	if call.GetRequestedOrderings() != nil {
		t.Fatal("expected nil when no constraints set")
	}
}

func TestImplementationRuleCall_GetRequestedOrderings_WithConstraints(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	cm := NewConstraintMap()
	orderings := []*RequestedOrdering{PreserveOrdering()}
	Set(cm, ref, RequestedOrderingConstraintKey, orderings)

	call := &ImplementationRuleCall{
		Reference:   ref,
		Constraints: cm,
	}
	got := call.GetRequestedOrderings()
	if len(got) != 1 {
		t.Fatalf("expected 1 ordering, got %d", len(got))
	}
}
