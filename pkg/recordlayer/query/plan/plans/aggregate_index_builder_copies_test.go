package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// A plan's WithXxx builders must COPY, and for these two the reason is plan
// IDENTITY rather than style. liveGroupsOnly is folded into structuralKey because a
// scan that drops vacated groups is a different plan from one that does not, so an
// in-place write changes the identity of an object the memo may already hold — and
// the memo goes on serving it under the key it was interned with. The key's own doc
// spells out the consequence: the filtering scan and the unfiltered one collapse into
// one expression and whichever arrived first wins.
//
// WithGroupColumnLayout and WithLiveGroupsOnly wrote to the receiver and returned it,
// alone among 57 sibling builders that do `cp := *p`. That was LATENT, not live, and
// only because every caller invokes the copying WithGroupColumns first, so the
// in-place write landed on a fresh copy. Reordering one chain arms it. Nothing pinned
// the ordering, which is why this test pins the copy instead.
//
// This is also a prerequisite for memoizing the structural key per plan object: a
// memo is only sound while a copy cannot inherit a key that no longer describes it.
func aggregateIndexPlanForBuilderTest(t *testing.T) *RecordQueryAggregateIndexPlan {
	t.Helper()
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(1))
	index, err := NewRecordQueryIndexPlan(
		"idx", []*predicates.ComparisonRange{eq}, []string{"T"}, exactTestRecordType(), false)
	if err != nil {
		t.Fatalf("index plan: %v", err)
	}
	agg, err := NewRecordQueryAggregateIndexPlan(index, "T", exactTestRecordType(), "COUNT")
	if err != nil {
		t.Fatalf("aggregate index plan: %v", err)
	}
	return agg
}

func TestAggregateIndexBuildersCopyRatherThanMutateIdentity(t *testing.T) {
	t.Parallel()

	original := aggregateIndexPlanForBuilderTest(t)
	if original.IsLiveGroupsOnly() {
		t.Fatal("fixture already drops vacated groups, so flipping it proves nothing")
	}
	beforeKey := original.structuralKey()

	// The identity-bearing one. A returned variant must differ; the RECEIVER must not.
	variant := original.WithLiveGroupsOnly(true)
	if variant == original {
		t.Error("WithLiveGroupsOnly returned the receiver, so it mutated in place")
	}
	if !variant.IsLiveGroupsOnly() {
		t.Error("the variant did not take the new value")
	}
	if original.IsLiveGroupsOnly() {
		t.Fatal("WithLiveGroupsOnly mutated the RECEIVER's liveGroupsOnly. That field is " +
			"folded into structuralKey, so this rewrites the identity of a plan the memo " +
			"may already hold under the old key")
	}
	if !original.structuralKey().Equal(beforeKey) {
		t.Error("the receiver's structural key changed, i.e. its identity moved under it")
	}
	if variant.structuralKey().Equal(beforeKey) {
		t.Error("the variant has the same structural key as the original despite " +
			"differing in liveGroupsOnly; the memo would intern the filtering scan and " +
			"the unfiltered one as one expression")
	}

	// And the layout builder, which is not identity-bearing but must still not
	// reach back into a shared receiver.
	layoutVariant := original.WithGroupColumnLayout(exactTestRecordType())
	if layoutVariant == original {
		t.Error("WithGroupColumnLayout returned the receiver, so it mutated in place")
	}
	if original.GetGroupColumnLayout() != nil {
		t.Error("WithGroupColumnLayout wrote the layout onto the RECEIVER")
	}
	if layoutVariant.GetGroupColumnLayout() == nil {
		t.Error("the variant did not take the layout")
	}
}
