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

// TestCombineRequestedOrderings ports the semantics of Java
// RequestedOrderingConstraint.combine: union with subsumption.
func TestCombineRequestedOrderings(t *testing.T) {
	t.Parallel()
	partX := RequestedOrderingPart{Value: values.NewFlatFieldValue("X", values.UnknownType), SortOrder: RequestedSortOrderAscending}
	partY := RequestedOrderingPart{Value: values.NewFlatFieldValue("Y", values.UnknownType), SortOrder: RequestedSortOrderAscending}
	roX := NewRequestedOrdering([]RequestedOrderingPart{partX}, DistinctnessNotDistinct, false)
	roXdup := NewRequestedOrdering([]RequestedOrderingPart{partX}, DistinctnessNotDistinct, false)
	roY := NewRequestedOrdering([]RequestedOrderingPart{partY}, DistinctnessNotDistinct, false)
	roXexh := NewRequestedOrdering([]RequestedOrderingPart{partX}, DistinctnessNotDistinct, true)
	roXY := NewRequestedOrdering([]RequestedOrderingPart{partX, partY}, DistinctnessNotDistinct, false)
	roXdistinct := NewRequestedOrdering([]RequestedOrderingPart{partX}, DistinctnessDistinct, false)

	// Distinct orderings union.
	got, changed := CombineRequestedOrderings([]*RequestedOrdering{roX}, []*RequestedOrdering{roY})
	if !changed || len(got) != 2 {
		t.Fatalf("X + Y must union to 2, got %d (changed=%v)", len(got), changed)
	}
	// An exact duplicate is subsumed — nothing new (Java's Optional.empty).
	got, changed = CombineRequestedOrderings([]*RequestedOrdering{roX}, []*RequestedOrdering{roXdup})
	if changed || len(got) != 1 {
		t.Fatalf("duplicate X must be subsumed, got %d (changed=%v)", len(got), changed)
	}
	// An exhaustive current subsumes a new ordering extending its prefix.
	got, changed = CombineRequestedOrderings([]*RequestedOrdering{roXexh}, []*RequestedOrdering{roXY})
	if changed || len(got) != 1 {
		t.Fatalf("exhaustive X must subsume (X,Y), got %d (changed=%v)", len(got), changed)
	}
	// A NON-exhaustive current does NOT prefix-subsume.
	got, changed = CombineRequestedOrderings([]*RequestedOrdering{roX}, []*RequestedOrdering{roXY})
	if !changed || len(got) != 2 {
		t.Fatalf("non-exhaustive X must NOT subsume (X,Y), got %d (changed=%v)", len(got), changed)
	}
	// Distinctness mismatch never subsumes.
	got, changed = CombineRequestedOrderings([]*RequestedOrdering{roX}, []*RequestedOrdering{roXdistinct})
	if !changed || len(got) != 2 {
		t.Fatalf("distinctness mismatch must not subsume, got %d (changed=%v)", len(got), changed)
	}
}

// TestPushConstraint_CombinesAcrossParents pins the clobber fix: two
// parents pushing requested orderings onto ONE shared child ref must
// UNION (Java ConstraintsMap.pushProperty → combine), not last-write-wins
// — a blind replace pruned the first parent's ordered alternative via the
// requested-ordering winner retention.
func TestPushConstraint_CombinesAcrossParents(t *testing.T) {
	t.Parallel()
	childRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	cm := NewConstraintMap()
	call := &ImplementationRuleCall{Constraints: cm}

	roX := NewRequestedOrdering([]RequestedOrderingPart{{Value: values.NewFlatFieldValue("X", values.UnknownType), SortOrder: RequestedSortOrderAscending}}, DistinctnessNotDistinct, false)
	roY := NewRequestedOrdering([]RequestedOrderingPart{{Value: values.NewFlatFieldValue("Y", values.UnknownType), SortOrder: RequestedSortOrderDescending}}, DistinctnessNotDistinct, false)

	call.PushConstraint(childRef, []*RequestedOrdering{roX})
	call.PushConstraint(childRef, []*RequestedOrdering{roY})

	got, ok := Get(cm, childRef, RequestedOrderingConstraintKey)
	if !ok || len(got) != 2 {
		t.Fatalf("two parents' orderings must both survive on the shared child; got %d (ok=%v)", len(got), ok)
	}
}

// TestConstraintMap_CanonicalKeys pins group-identity keying: a constraint
// pushed via a since-merged (forwarded) Reference must be visible through
// the canonical survivor and vice versa — the pruning task reads with the
// canonical root while push rules may hold pre-merge aliases.
func TestConstraintMap_CanonicalKeys(t *testing.T) {
	t.Parallel()
	survivor := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType))
	loser := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType))
	survivor.Absorb(loser)

	cm := NewConstraintMap()
	roX := NewRequestedOrdering([]RequestedOrderingPart{{Value: values.NewFlatFieldValue("X", values.UnknownType), SortOrder: RequestedSortOrderAscending}}, DistinctnessNotDistinct, false)
	Set(cm, loser, RequestedOrderingConstraintKey, []*RequestedOrdering{roX})
	if got, ok := Get(cm, survivor, RequestedOrderingConstraintKey); !ok || len(got) != 1 {
		t.Fatalf("constraint set via the forwarded alias must be visible via the canonical ref (ok=%v)", ok)
	}
}
