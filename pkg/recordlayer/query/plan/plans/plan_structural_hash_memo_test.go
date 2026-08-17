package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// The structural-hash memo is a CORRECTNESS-sensitive cache: a stale hit is a wrong
// identity, and a wrong identity makes the memo intern two different plans as one and
// serve whichever arrived first. It is also shared by construction — a plan is copied
// with `cp := *p`, which copies the cell POINTER — so every invariant below is about
// what happens when two plans share one cell.
//
// Nothing here is about speed. A hit that returns the wrong plan's hash is the failure
// mode, and it is silent: both values are perfectly good hashes.

func memoTestIndexPlan(t *testing.T) *RecordQueryIndexPlan {
	t.Helper()
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(1))
	plan, err := NewRecordQueryIndexPlan(
		"idx", []*predicates.ComparisonRange{eq}, []string{"T"}, exactTestRecordType(), false)
	if err != nil {
		t.Fatalf("index plan: %v", err)
	}
	return plan
}

// TestStructuralHashMemoStartsEmptyAndIsLazy pins that no constructor populates the
// cell. This is a correctness requirement rather than a policy: several plans finish
// initialising AFTER their constructor returns — the WithQuantifiers rebuild paths write
// drivingAlias / outputNameOverrides / distinctProofIndexName onto a freshly built plan,
// and those fields are in the structural key — so a hash taken inside the constructor
// would describe a plan that does not exist yet.
func TestStructuralHashMemoStartsEmptyAndIsLazy(t *testing.T) {
	t.Parallel()

	plan := memoTestIndexPlan(t)
	if plan.hashMemo == nil {
		t.Fatal("no memo cell was allocated, so the memo is inert and every other test " +
			"here would pass vacuously")
	}
	if state := plan.hashMemo.state.Load(); state != nil {
		t.Errorf("the constructor populated the memo (owner=%v); a plan that completes "+
			"its initialisation after construction would then carry a hash for its "+
			"unfinished self", state.owner)
	}

	// First read populates it, and populates it for THIS plan.
	want := plan.HashCodeWithoutChildren()
	state := plan.hashMemo.state.Load()
	if state == nil {
		t.Fatal("reading the hash did not populate the memo")
	}
	if state.owner != any(plan) {
		t.Error("the memo recorded an owner that is not the plan that computed it")
	}
	if state.hash != want {
		t.Errorf("memoized %d, returned %d", state.hash, want)
	}
	if again := plan.HashCodeWithoutChildren(); again != want {
		t.Errorf("second read gave %d, first gave %d", again, want)
	}
}

// TestStructuralHashMemoIsNotInheritedByACopy pins the read-side owner check.
//
// `cp := *p` copies the cell pointer, so a copy that skipped the owner check would read
// the ORIGINAL's hash as its own. Since WithXxx exists precisely to change
// identity-bearing fields, that hash describes a different plan — and the two would
// then compare equal under the memo's hash gate despite being different plans.
func TestStructuralHashMemoIsNotInheritedByACopy(t *testing.T) {
	t.Parallel()

	original := memoTestIndexPlan(t)
	originalHash := original.HashCodeWithoutChildren()

	// A copy that differs in an identity-bearing field.
	variant := original.WithScanComparisons([]*predicates.ComparisonRange{
		scanCostRange(t, predicates.ComparisonEquals, int64(2)),
	})
	if variant == original {
		t.Fatal("WithScanComparisons returned the receiver, so there is no copy to test")
	}
	if variant.hashMemo != original.hashMemo {
		t.Fatal("the copy got its own cell, so this test is not exercising the shared-cell " +
			"path it was written for")
	}

	variantHash := variant.HashCodeWithoutChildren()
	if variantHash == originalHash {
		t.Error("the copy reported the original's hash despite differing in a field that " +
			"is in the structural key; the read-side owner check did not fire, and the " +
			"memo would intern these two plans as one")
	}
	// And the copy's own key really does differ, so the check above is meaningful.
	if original.structuralKey().Equal(variant.structuralKey()) {
		t.Error("the two plans have equal structural keys, so this test cannot detect a " +
			"stale hit")
	}
}

// TestACopyDoesNotEvictTheOriginalsMemo pins the WRITE-side owner check, which is the
// half that is easy to omit.
//
// A copy shares the cell. If its store were unconditional it would overwrite the
// original's entry; the original would then miss its own owner check and store back,
// and the two would evict each other on every comparison — a ping-pong in the
// sequential case and a live race on the cell in the concurrent one. A copy that finds
// the cell owned by someone else must compute and simply not memoize.
func TestACopyDoesNotEvictTheOriginalsMemo(t *testing.T) {
	t.Parallel()

	original := memoTestIndexPlan(t)
	originalHash := original.HashCodeWithoutChildren()
	if state := original.hashMemo.state.Load(); state == nil || state.owner != any(original) {
		t.Fatal("the original does not own its own cell, so eviction cannot be observed")
	}

	variant := original.WithScanComparisons([]*predicates.ComparisonRange{
		scanCostRange(t, predicates.ComparisonEquals, int64(2)),
	})
	_ = variant.HashCodeWithoutChildren() // the copy computes; it must not store

	state := original.hashMemo.state.Load()
	if state == nil {
		t.Fatal("the copy cleared the cell the original owned")
	}
	if state.owner != any(original) {
		t.Error("the copy took ownership of the shared cell; the original now misses its " +
			"own memo and will store back, so the two evict each other every comparison")
	}
	if state.hash != originalHash {
		t.Errorf("the cell holds %d, the original's hash is %d", state.hash, originalHash)
	}
	// The original still answers its own value from the memo.
	if again := original.HashCodeWithoutChildren(); again != originalHash {
		t.Errorf("the original now answers %d, was %d", again, originalHash)
	}
}

// TestMemoizingDoesNotChangeTheHashValue pins that the memo is transparent: two
// independently built equal plans must hash equal whether or not either has been
// hashed before. If the memo could change an answer, every memo-dedup decision in the
// planner would depend on read order.
func TestMemoizingDoesNotChangeTheHashValue(t *testing.T) {
	t.Parallel()

	first := memoTestIndexPlan(t)
	second := memoTestIndexPlan(t)

	// Warm only the first, then compare.
	warmed := first.HashCodeWithoutChildren()
	cold := second.HashCodeWithoutChildren()
	if warmed != cold {
		t.Errorf("equal plans hashed %d and %d; the memo is not transparent", warmed, cold)
	}
	if !first.EqualsPlanWithoutChildren(second) {
		t.Error("the two fixtures are not equal plans, so this test proves nothing about " +
			"equal plans hashing equal")
	}
	// A second read of the warmed one must still agree with the cold one.
	if again := first.HashCodeWithoutChildren(); again != cold {
		t.Errorf("memoized re-read gave %d, cold gave %d", again, cold)
	}
}
