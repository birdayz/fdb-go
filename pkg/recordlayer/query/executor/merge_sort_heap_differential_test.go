package executor

// Differential proof that mergeSortCursor's heap-based leg selection
// (executor_new_plans.go) is behaviorally IDENTICAL to the O(N)-per-row
// linear scan it replaced (Java UnionCursor.chooseStates, :101-131 — Java
// itself never uses a heap here; see the mergeSortCursor doc comment for
// why Go's InUnion legs can grow far past what Java's per-row rescan was
// designed for). referenceMergeSortOnNext below is that replaced algorithm,
// preserved ONLY as a test oracle — CLAUDE.md forbids shipping two
// mechanisms for the same concept, so it must never be called from
// production code.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// referenceMergeSortOnNext is the pre-heap selection algorithm this package
// used before the O(log N) heap: pull every leg every round, stop the whole
// merge on any non-exhaustion leg reason, else linearly rescan every leg for
// the minimum comparison key. Byte-for-byte the replaced OnNext body.
func referenceMergeSortOnNext(m *mergeSortCursor, ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if m.lastNoNext != nil {
		return *m.lastNoNext, nil
	}
	if m.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if err := ctx.Err(); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	for _, s := range m.states {
		if err := s.pull(ctx, m); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
	}

	anyHasNext := false
	for _, s := range m.states {
		if s.result.HasNext() {
			anyHasNext = true
		} else if s.result.GetNoNextReason().IsLimitReached() {
			return m.stopWith(referenceStrongestNoNextReason(m)), nil
		}
	}
	if !anyHasNext {
		return m.stopWith(recordlayer.SourceExhausted), nil
	}

	var chosen []*mergeSortChildState
	var minKey []any
	for _, s := range m.states {
		if !s.result.HasNext() {
			continue
		}
		cmp := -1
		if chosen != nil {
			var cmpErr error
			cmp, cmpErr = m.compareKeyVals(s.keyVals, minKey)
			if cmpErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, cmpErr
			}
		}
		if cmp < 0 {
			chosen = chosen[:0]
			minKey = s.keyVals
		}
		if cmp <= 0 {
			chosen = append(chosen, s)
		}
	}
	if !m.dedup {
		chosen = chosen[:1]
	}

	result := chosen[0].result.GetValue()
	for _, s := range chosen {
		s.consume()
	}
	return recordlayer.NewResultWithValue(result, m.snapshotContinuation()), nil
}

func referenceStrongestNoNextReason(m *mergeSortCursor) recordlayer.NoNextReason {
	reason := recordlayer.SourceExhausted
	found := false
	for _, s := range m.states {
		if !s.pulled || s.result.HasNext() {
			continue
		}
		childReason := s.result.GetNoNextReason()
		if !found || childReason.IsOutOfBand() || reason.IsSourceExhausted() {
			reason = childReason
			found = true
		}
	}
	return reason
}

// mergeSortStep is one OnNext outcome, reduced to what a caller can observe
// and rely on for resumption: whether a row came out, its id, the stop
// reason when it didn't, and the continuation bytes either way.
type mergeSortStep struct {
	hasNext bool
	id      int64
	reason  recordlayer.NoNextReason
	contEnd bool
	contHex string
}

func stepOf(t *testing.T, r recordlayer.RecordCursorResult[QueryResult], err error) mergeSortStep {
	t.Helper()
	if err != nil {
		t.Fatalf("OnNext error: %v", err)
	}
	cont := r.GetContinuation()
	b, cErr := cont.ToBytes()
	if cErr != nil {
		t.Fatalf("continuation ToBytes: %v", cErr)
	}
	s := mergeSortStep{hasNext: r.HasNext(), contEnd: cont.IsEnd(), contHex: fmt.Sprintf("%x", b)}
	if r.HasNext() {
		s.id = fieldVal(t, r.GetValue(), "id")
	} else {
		s.reason = r.GetNoNextReason()
	}
	return s
}

// buildLegRows generates numLegs sorted (ascending, or descending if
// reverse) int64 "id" sequences drawn from [1, maxKey] — a small maxKey
// forces heavy cross-leg AND within-leg ties, the case the replaced linear
// scan and the heap must agree on bit-for-bit (which leg "wins" a tie is
// observable: it determines whose row is returned and whose continuation
// advances).
func buildLegRows(rng *rand.Rand, numLegs, maxLegLen, maxKey int, reverse bool) [][]QueryResult {
	legs := make([][]QueryResult, numLegs)
	for i := range legs {
		n := rng.Intn(maxLegLen + 1)
		keys := make([]int, n)
		for j := range keys {
			keys[j] = 1 + rng.Intn(maxKey)
		}
		sort.Ints(keys)
		if reverse {
			sort.Sort(sort.Reverse(sort.IntSlice(keys)))
		}
		rows := make([]QueryResult, n)
		for j, k := range keys {
			rows[j] = qr("id", int64(k))
		}
		legs[i] = rows
	}
	return legs
}

func rowsToCursors(legs [][]QueryResult) []recordlayer.RecordCursor[QueryResult] {
	out := make([]recordlayer.RecordCursor[QueryResult], len(legs))
	for i, rows := range legs {
		out[i] = recordlayer.FromList(rows)
	}
	return out
}

// runDifferential drives a heap-based cursor and a reference (linear-scan)
// cursor over identical fresh copies of legs, in lockstep, and fails on the
// first observable divergence (value, stop reason, or continuation bytes).
// It returns the emitted continuations for the heap cursor's runs, so
// callers can additionally check resumption from every row.
func runDifferential(t *testing.T, legs [][]QueryResult, reverse, dedup bool) []mergeSortStep {
	t.Helper()
	ctx := context.Background()
	heapCursor := newMergeSortCursor(rowsToCursors(legs), idKey(), reverse, dedup)
	defer heapCursor.Close()
	refCursor := newMergeSortCursor(rowsToCursors(legs), idKey(), reverse, dedup)
	defer refCursor.Close()

	var steps []mergeSortStep
	for round := 0; ; round++ {
		hr, hErr := heapCursor.OnNext(ctx)
		hs := stepOf(t, hr, hErr)
		rr, rErr := referenceMergeSortOnNext(refCursor, ctx)
		rs := stepOf(t, rr, rErr)
		if hs != rs {
			t.Fatalf("round %d: heap selection = %+v, linear-scan reference = %+v (legs=%v reverse=%v dedup=%v)",
				round, hs, rs, legs, reverse, dedup)
		}
		steps = append(steps, hs)
		if !hs.hasNext {
			return steps
		}
		if round > 10_000 {
			t.Fatalf("merge did not terminate after %d rounds — possible infinite loop in the heap selection", round)
		}
	}
}

func TestMergeSortCursor_HeapMatchesLinearScan(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(0xC0FFEE))
	for trial := 0; trial < 500; trial++ {
		numLegs := 1 + rng.Intn(12)
		reverse := rng.Intn(2) == 0
		dedup := rng.Intn(2) == 0
		maxKey := 1 + rng.Intn(6) // small: forces heavy ties, exercising the tie-break loop
		legs := buildLegRows(rng, numLegs, 15, maxKey, reverse)
		runDifferential(t, legs, reverse, dedup)
	}
}

// TestMergeSortCursor_HeapMatchesLinearScan_WideNoTies uses a wide key range
// (near-zero collision probability) across many legs — the shape the O(N)
// vs O(log N) improvement targets (an InUnion over a long IN list, where
// legs rarely tie) — as a companion to the heavy-tie-biased fuzz corpus
// above.
func TestMergeSortCursor_HeapMatchesLinearScan_WideNoTies(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(0xBADF00D))
	for trial := 0; trial < 100; trial++ {
		numLegs := 20 + rng.Intn(80)
		reverse := trial%2 == 0
		dedup := trial%3 != 0
		legs := buildLegRows(rng, numLegs, 8, 100_000, reverse)
		runDifferential(t, legs, reverse, dedup)
	}
}

// TestMergeSortCursor_HeapMatchesLinearScan_Resume checks that resuming
// EVERY emitted row's continuation (both algorithms, cross-wired: a heap
// cursor resumed from a reference-produced continuation and vice versa)
// reproduces exactly the remaining suffix — the heap change must not alter
// what a continuation means.
func TestMergeSortCursor_HeapMatchesLinearScan_Resume(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(0x5EED))
	for trial := 0; trial < 150; trial++ {
		// A UnionContinuation always encodes at least 2 legs
		// (decodeUnionContinuation / Java UnionCursorContinuation.from), so
		// resumption is only exercised for merges the planner would ever
		// build via newMergeSortCursorFromFactories (>= 2 legs — the
		// single-leg case is short-circuited before a mergeSortCursor is
		// even constructed).
		numLegs := 2 + rng.Intn(7)
		reverse := rng.Intn(2) == 0
		dedup := rng.Intn(2) == 0
		maxKey := 1 + rng.Intn(5)
		legs := buildLegRows(rng, numLegs, 10, maxKey, reverse)

		steps := runDifferential(t, legs, reverse, dedup)

		factories := make([]recordlayer.CursorFactory[QueryResult], len(legs))
		for i, rows := range legs {
			factories[i] = listFactory(rows)
		}

		for i, st := range steps {
			if !st.hasNext {
				continue
			}
			contBytes := hexToBytes(t, st.contHex)

			resumedHeap, err := newMergeSortCursorFromFactories(factories, idKey(), reverse, dedup, contBytes)
			if err != nil {
				t.Fatalf("trial legs=%v round %d: resume construction: %v", legs, i, err)
			}
			wantSuffix := stepsAfter(steps, i+1)
			gotHeap := drainSteps(t, resumedHeap, func(c *mergeSortCursor, ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
				return c.OnNext(ctx)
			})
			resumedHeap.Close()
			if !sameStepSequence(gotHeap, wantSuffix) {
				t.Fatalf("trial legs=%v reverse=%v dedup=%v: heap-resume from round %d = %v, want suffix %v",
					legs, reverse, dedup, i, gotHeap, wantSuffix)
			}

			resumedRef, err := newMergeSortCursorFromFactories(factories, idKey(), reverse, dedup, contBytes)
			if err != nil {
				t.Fatalf("trial legs=%v round %d: reference resume construction: %v", legs, i, err)
			}
			gotRef := drainSteps(t, resumedRef, referenceMergeSortOnNext)
			resumedRef.Close()
			if !sameStepSequence(gotRef, wantSuffix) {
				t.Fatalf("trial legs=%v reverse=%v dedup=%v: linear-scan-resume from round %d = %v, want suffix %v",
					legs, reverse, dedup, i, gotRef, wantSuffix)
			}
		}
	}
}

func stepsAfter(steps []mergeSortStep, i int) []mergeSortStep {
	if i >= len(steps) {
		return nil
	}
	return steps[i:]
}

func drainSteps(t *testing.T, c *mergeSortCursor, next func(*mergeSortCursor, context.Context) (recordlayer.RecordCursorResult[QueryResult], error)) []mergeSortStep {
	t.Helper()
	ctx := context.Background()
	var out []mergeSortStep
	for {
		r, err := next(c, ctx)
		s := stepOf(t, r, err)
		out = append(out, s)
		if !s.hasNext {
			return out
		}
		if len(out) > 10_000 {
			t.Fatal("resumed drain did not terminate")
		}
	}
}

// sameStepSequence compares emitted ids/reasons but ignores continuation
// bytes (a resumed cursor's absolute continuation bytes need not match the
// unresumed run's suffix bytes verbatim in every cursor implementation —
// only the emitted VALUES and final reason are the resumption contract).
func sameStepSequence(got, want []mergeSortStep) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].hasNext != want[i].hasNext || got[i].id != want[i].id {
			return false
		}
		if !got[i].hasNext && got[i].reason != want[i].reason {
			return false
		}
	}
	return true
}

func hexToBytes(t *testing.T, s string) []byte {
	t.Helper()
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode %q: %v", s, err)
	}
	return b
}

// FuzzMergeSortCursorHeapMatchesLinearScan is the differential fuzz target:
// arbitrary leg counts/lengths/keys/reverse/dedup, asserting the heap
// selection and the replaced linear scan never diverge.
func FuzzMergeSortCursorHeapMatchesLinearScan(f *testing.F) {
	f.Add(uint32(1), uint8(3), uint8(0), false, false)
	f.Add(uint32(0xC0FFEE), uint8(20), uint8(2), true, true)
	f.Add(uint32(42), uint8(1), uint8(1), false, true)
	f.Add(uint32(7), uint8(50), uint8(4), true, false)

	f.Fuzz(func(t *testing.T, seed uint32, legsHint uint8, keyHint uint8, reverse, dedup bool) {
		rng := rand.New(rand.NewSource(int64(seed)))
		numLegs := 1 + int(legsHint%16)
		maxKey := 1 + int(keyHint%8)
		legs := buildLegRows(rng, numLegs, 12, maxKey, reverse)
		runDifferential(t, legs, reverse, dedup)
	})
}
