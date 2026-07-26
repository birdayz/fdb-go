package executor

// Regression coverage for the mergeSortCursor.OnNext choose-loop's heapPop
// error path: with 3+ legs still resident, heap.Pop's own internal
// sift-down can compare two legs that have NOTHING to do with the one
// heapPop() is about to return, and that comparison can fail (a genuinely
// invalid, cross-type comparison-key pair — a planner invariant violation).
// Before the fix, that failure was surfaced via `chosen = append(chosen,
// m.heapPop()); if err := m.takeHeapErr(); err != nil { return ..., err }`
// with NOTHING pushed back: every leg already accumulated in `chosen` —
// including the one heapPop() just returned, which has nothing to do with
// the failing comparison — vanished from the heap forever. A caller
// retrying OnNext after the error would silently never see that leg's row
// again, and the eventual continuation would be built from a heap short one
// leg.
//
// poisonInt is a distinct Go type from int64 (and every other type
// compareValues knows), so comparing it against ANY normal comparison-key
// value always hits compareValues' "no ordering defined" cross-type arm —
// deterministically and permanently, for as long as the poisoned leg and
// any real leg coexist in the heap. That permanence is deliberate: once
// introduced, a truly invalid comparison key can never be resolved (this
// mirrors Java — MergeCursor has no way to "recover" a comparator that
// throws), so this test does NOT expect the merge to ever fully drain. It
// asserts the actual invariant the fix restores: an error return must leave
// the heap holding every leg it held on entry. That is checked directly by
// leg count (m.heap.Len()) rather than by row content, because with a
// leg permanently unresolvable against the others there is no row-level
// observation available once the poison leg is in play — the leg-count
// invariant is exactly what stands between "permanently loud" (correct,
// Java-consistent, matches a real invariant violation) and "silently
// missing a row" (the bug).
import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

type poisonInt int64

// poisonRowsCursor builds a leg whose rows may be any mix of real int64 ids
// and poisonInt sentinels — plainCursor forces int64, this doesn't.
func poisonRowsCursor(rows ...any) recordlayer.RecordCursor[QueryResult] {
	items := make([]QueryResult, len(rows))
	for i, v := range rows {
		items[i] = qr("id", v)
	}
	return recordlayer.FromList(items)
}

// TestMergeSortCursorReference_AllRowsEmitted_NoTypeConflict pins that the
// 3-leg shape used below is a legitimate merge when nothing is poisoned:
// every row, including the one the poisoned variant loses, is expected
// output. Without this, TestMergeSortCursor_PopSiftError_PreservesAllLegs's
// claim that leg 0's row is a real, wrongly-dropped row (not just an
// artifact of the poisoned construction) would be unverified.
func TestMergeSortCursorReference_AllRowsEmitted_NoTypeConflict(t *testing.T) {
	t.Parallel()
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{
			poisonRowsCursor(int64(1)),
			poisonRowsCursor(int64(3)),
			poisonRowsCursor(int64(999)), // the poisoned variant's leg 2, unpoisoned
		},
		idKey(), false, false,
	)
	defer c.Close()

	got := collectMergeSortCursor(t, c)
	var ids []int64
	for _, r := range got {
		ids = append(ids, fieldVal(t, r, "id"))
	}
	assertInt64Slice(t, ids, []int64{1, 3, 999})
}

// TestMergeSortCursor_PopSiftError_PreservesAllLegs is the B-fix regression:
// 3 legs (>= 3 so heap.Pop's sift-down has enough remaining elements to
// compare two of them), leg 2 poisoned from its very first (only) row.
//
//   - Round 0: the very first OnNext call's initial admit pushes all 3 legs
//     one at a time; pushing leg 2 (poison) compares it against leg 0 during
//     the up-sift and fails — a PUSH-time error, already safe pre-fix (Push
//     always appends before sifting, so the leg is a heap member regardless
//     of the sift's outcome). Tolerated here as expected setup noise.
//   - Round 1: the choose loop's first heapPop() (heap.Len()==3, so the
//     sift-down has a real 2-way child comparison to make) removes the true
//     minimum (leg 0's row, id=1) cleanly, but the down-sift's OWN internal
//     comparison of the two REMAINING legs (leg 1 vs poisoned leg 2) fails
//     and latches m.heapErr. This is the exact bug: id=1 has nothing to do
//     with that failing comparison, but pre-fix it is discarded anyway.
//
// After round 1, m.heap.Len() is checked directly: 3 with the fix (every
// leg the heap held on entry is still there, including leg 0), 2 without it
// (leg 0 silently gone). The same check is repeated over several more
// retries to confirm the leg count doesn't quietly leak away on a later
// round either — leg 1 and the poisoned leg 2 can never be resolved against
// each other (see the poisonInt doc comment), so every further OnNext call
// keeps erroring, and each of those error returns must ALSO preserve every
// leg it started with.
func TestMergeSortCursor_PopSiftError_PreservesAllLegs(t *testing.T) {
	t.Parallel()
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{
			poisonRowsCursor(int64(1)),
			poisonRowsCursor(int64(3)),
			poisonRowsCursor(poisonInt(999)),
		},
		idKey(), false, false,
	)
	defer c.Close()
	ctx := context.Background()

	// Round 0: expected push-time setup error (leg 2 vs leg 0), already
	// safe pre-fix — not the bug under test, just tolerated noise.
	if _, err := c.OnNext(ctx); err == nil {
		t.Fatal("round 0: want the push-time cross-type error while admitting leg 2, got nil")
	}
	if got := c.heap.Len(); got != 3 {
		t.Fatalf("round 0: heap.Len() = %d, want 3 (push errors never remove a leg)", got)
	}

	// Round 1: the bug's exact trigger — heapPop's own internal sift-down
	// comparison (leg 1 vs poisoned leg 2) fails while leg 0's row is
	// sitting in `chosen`, already extracted from the heap.
	if _, err := c.OnNext(ctx); err == nil {
		t.Fatal("round 1: want the pop-sift cross-type error, got nil")
	}
	if got := c.heap.Len(); got != 3 {
		t.Fatalf("round 1 (the bug's trigger point): heap.Len() = %d, want 3 — leg 0's row (id=1) must not be dropped by a comparison failure between two OTHER legs", got)
	}

	// The leg-1-vs-poisoned-leg-2 conflict can never resolve (see the
	// poisonInt doc comment), so every further call keeps erroring — assert
	// the leg count survives each one, not just the first.
	for round := 2; round < 6; round++ {
		if _, err := c.OnNext(ctx); err == nil {
			t.Fatalf("round %d: want a continued cross-type error (leg 1 and the poisoned leg can never resolve), got nil", round)
		}
		if got := c.heap.Len(); got != 3 {
			t.Fatalf("round %d: heap.Len() = %d, want 3 — a leg leaked out on a later retry", round, got)
		}
	}
}
