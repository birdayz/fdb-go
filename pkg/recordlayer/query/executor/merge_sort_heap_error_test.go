package executor

// Regression coverage for the B1 fix: a leg's pull() error must not orphan
// the rest of the admit batch. mergeSortCursor.OnNext pulls a batch of legs
// (every leg on the first call, or the legs just consumed on every call
// after) via pullAndAdmit before choosing the next row. The differential
// harness in merge_sort_heap_differential_test.go cannot exercise this axis
// at all: every leg there is a recordlayer.FromList, which only ever stops
// via SourceExhausted — it never returns a hard error from OnNext. This file
// drives a leg that DOES.
//
// Before the fix, pullAndAdmit took the whole pending batch as a local slice
// and the caller cleared m.pendingAdmit BEFORE calling it. A pull error on
// any leg but the last one in that batch then permanently dropped every
// following leg: not pushed into the heap (pullAndAdmit returned before
// reaching it), and not left in m.pendingAdmit (already nilled by the
// caller) — so a later leg's row was silently never seen again, and if every
// remaining leg in the batch ended up orphaned that way, the eventual
// SourceExhausted snapshot was not actually all-END, which NewResultNoNext's
// invariant check turns into a panic.

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

var errInjectedLegFailure = errors.New("injected leg pull failure")

// errThenOKCursor wraps a cursor and forces its OnNext to return
// errInjectedLegFailure exactly once, on the errOnCall'th invocation
// (1-indexed) — every other invocation, including the one immediately
// following the injected failure, delegates straight through to inner.
// mergeSortChildState.pull only advances s.pulled on a SUCCESSFUL OnNext, so
// a caller retrying after the error re-invokes OnNext on the SAME logical
// pull rather than skipping it — this is what lets a leg recover from a
// transient failure without losing or duplicating a row.
type errThenOKCursor struct {
	inner     recordlayer.RecordCursor[QueryResult]
	errOnCall int
	calls     int
}

func (c *errThenOKCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	c.calls++
	if c.calls == c.errOnCall {
		return recordlayer.RecordCursorResult[QueryResult]{}, errInjectedLegFailure
	}
	return c.inner.OnNext(ctx)
}

func (c *errThenOKCursor) Close() error   { return c.inner.Close() }
func (c *errThenOKCursor) IsClosed() bool { return c.inner.IsClosed() }

// driveUntilDrained repeatedly calls OnNext, tolerating exactly the errors
// injected via errThenOKCursor (retrying immediately, exactly like a caller
// that treats a transient cursor error as retryable), and returns every
// emitted row's id plus how many errors were observed. It fails the test on
// any OTHER error, on a panic (Go's test runner reports one as a failure
// automatically, but t.Fatalf below also documents the expectation), or on
// a round budget blowout (a symptom of the orphaned-leg bug: a dropped leg
// can leave the merge silently unable to make progress on that leg's rows
// while still terminating "cleanly" on the others).
func driveUntilDrained(t *testing.T, c *mergeSortCursor) (ids []int64, errCount int) {
	t.Helper()
	ctx := context.Background()
	for round := 0; round < 1000; round++ {
		r, err := c.OnNext(ctx)
		if err != nil {
			if !errors.Is(err, errInjectedLegFailure) {
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
			errCount++
			continue
		}
		if !r.HasNext() {
			return ids, errCount
		}
		ids = append(ids, fieldVal(t, r.GetValue(), "id"))
	}
	t.Fatalf("did not drain within round budget — possible stuck/orphaned leg")
	return nil, 0
}

func mkErrThenOKCursor(errOnCall int, rows ...int64) *errThenOKCursor {
	items := make([]QueryResult, len(rows))
	for i, id := range rows {
		items[i] = qr("id", id)
	}
	return &errThenOKCursor{inner: recordlayer.FromList(items), errOnCall: errOnCall}
}

func plainCursor(rows ...int64) recordlayer.RecordCursor[QueryResult] {
	items := make([]QueryResult, len(rows))
	for i, id := range rows {
		items[i] = qr("id", id)
	}
	return recordlayer.FromList(items)
}

// TestMergeSortCursor_PullErrorOnFirstInitLeg fails on the FIRST leg pulled
// during the very first admit batch (every leg is pending on OnNext's first
// call). Before the fix this permanently orphaned every OTHER leg in the
// batch, since the whole batch had already been cleared out of
// m.pendingAdmit before the failing leg was even reached.
func TestMergeSortCursor_PullErrorOnFirstInitLeg(t *testing.T) {
	t.Parallel()
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{
			mkErrThenOKCursor(1, 10), // errors on its very first pull
			plainCursor(20),
			plainCursor(30),
		},
		idKey(), false, false,
	)
	defer c.Close()

	ids, errCount := driveUntilDrained(t, c)
	if errCount != 1 {
		t.Fatalf("errCount = %d, want 1", errCount)
	}
	assertInt64Slice(t, ids, []int64{10, 20, 30})
}

// TestMergeSortCursor_PullErrorOnMiddleInitLeg fails on the SECOND of three
// legs in the init batch — the leg the pre-fix code reached only AFTER the
// first leg had already been pulled and pushed onto the heap. This is the
// case that most directly demonstrates the bug: the pre-fix code cleared
// m.pendingAdmit before pulling anything, so hitting an error on leg 1 lost
// leg 2 (never pulled, never re-queued) even though leg 0 had already made
// it into the heap successfully.
func TestMergeSortCursor_PullErrorOnMiddleInitLeg(t *testing.T) {
	t.Parallel()
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{
			plainCursor(10),
			mkErrThenOKCursor(1, 20), // errors on its very first pull
			plainCursor(30),
		},
		idKey(), false, false,
	)
	defer c.Close()

	ids, errCount := driveUntilDrained(t, c)
	if errCount != 1 {
		t.Fatalf("errCount = %d, want 1", errCount)
	}
	assertInt64Slice(t, ids, []int64{10, 20, 30})
}

// TestMergeSortCursor_PullErrorOnLaterRoundAdmit fails on a leg's SECOND
// pull — i.e., an admit batch several rounds after init, mirroring the
// review's "fail on a later round's admit" scenario. Leg A yields two rows
// (10, 40) but errors when the merge tries to re-pull it for its second row
// after its first row (10) was already chosen and emitted; leg B yields one
// row (20); leg C yields two rows (30, 50). Full merged order with no error
// would be 10, 20, 30, 40, 50 — this test asserts that sequence survives
// intact around the injected error, i.e. leg A's row 40 is NOT silently
// dropped by the failed re-pull.
func TestMergeSortCursor_PullErrorOnLaterRoundAdmit(t *testing.T) {
	t.Parallel()
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{
			mkErrThenOKCursor(2, 10, 40), // 2nd invocation (re-pull for row 40) errors once
			plainCursor(20),
			plainCursor(30, 50),
		},
		idKey(), false, false,
	)
	defer c.Close()

	ids, errCount := driveUntilDrained(t, c)
	if errCount != 1 {
		t.Fatalf("errCount = %d, want 1", errCount)
	}
	assertInt64Slice(t, ids, []int64{10, 20, 30, 40, 50})
}

func assertInt64Slice(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (mismatch at index %d)", got, want, i)
		}
	}
}
