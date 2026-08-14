package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPinnedSpineConstruct[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("construct pinned-spine fixture: %v", err)
	}
	return value
}

// TestPinOrderedSpineMarksPrivateChildSelection pins the distinction between a
// selected physical edge and an ordinary one-member equivalence group. The
// latter is still eligible for physical rewrites, which can replace the exact
// ordered member after a parent has dropped its sort. PinnedFinalOf carries the
// explicit marker that makes ExploreGroup preserve the selected member.
//
// MUTATION: replace PinnedFinalOf with FinalOf in pinOrderedSpine. The child
// remains a finals-only singleton, so membership-only tests stay green, but its
// IsPinnedFinal marker becomes false and this test fails.
func TestPinOrderedSpineMarksPrivateChildSelection(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("PinnedOrderingSpineRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 1},
	})
	index, err := plans.NewRecordQueryIndexPlan(
		"IDX_PINNED_SPINE_X", nil, []string{"PinnedOrderingSpineRow"}, rowType, false)
	index = mustPinnedSpineConstruct(t, index, err)
	index = index.WithIndexMetadata([]string{"X"}, []string{"ID"}, false)
	richOrdering := index.HintRichOrdering()
	if richOrdering == nil || len(richOrdering.GetKeys()) == 0 {
		t.Fatal("fixture index exposes no ordering; the pinning premise is not exercised")
	}
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     richOrdering.GetKeys()[0],
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	if !memberSatisfiesOrdering(index, requested) {
		t.Fatal("fixture index does not satisfy its requested ascending ordering")
	}

	source := expressions.InitialOf(index)
	filter, err := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		expressions.ForEachQuantifier(source),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	)
	filter = mustPinnedSpineConstruct(t, filter, err)
	pinned := pinOrderedSpine(filter, requested, nil)
	if pinned == nil {
		t.Fatal("ordered delegator over a satisfying member was not pinned")
	}
	quantifiers := pinned.GetQuantifiers()
	if len(quantifiers) != 1 {
		t.Fatalf("pinned delegator has %d quantifiers, want 1", len(quantifiers))
	}
	privateRef := quantifiers[0].GetRangesOver()
	if privateRef == nil {
		t.Fatal("pinned delegator has no private child reference")
	}
	if !privateRef.IsPinnedFinal() {
		t.Fatal("ordered spine child is an ordinary singleton, not an explicit pinned selection")
	}
	finals := privateRef.FinalMembers()
	if len(finals) != 1 || finals[0] != index {
		t.Fatalf("pinned child finals = %#v, want exactly the selected index member", finals)
	}
}
