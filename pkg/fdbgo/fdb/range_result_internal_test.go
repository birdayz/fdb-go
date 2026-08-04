package fdb

import (
	"bytes"
	"testing"
)

// TestKeyAfter_NoAliasOnSpareCapacity pins the #15 fix: keyAfter must return a
// fresh copy of k + 0x00 WITHOUT scribbling or aliasing k's backing array, even
// when cap(k) > len(k).
//
// The old form `append([]byte(k), 0)` writes the trailing 0 into the shared
// backing array at index len(k) when there is spare capacity, corrupting
// whatever follows k in that buffer — and real range replies pack many keys and
// values into a single buffer. The code is only accidentally-safe today because
// the reply parser length-caps every key slice (data[pos:pos+n:pos+n]); this
// test pins the contract independent of that upstream invariant. It fails on the
// buggy append form on both the scribble axis and the alias axis.
func TestKeyAfter_NoAliasOnSpareCapacity(t *testing.T) {
	t.Parallel()

	// k is a length-slice (len 3, cap 7) over a larger array; the trailing
	// "ZZZZ" stands in for adjacent data a bare append would clobber.
	backing := []byte("keyZZZZ")
	k := backing[:3:len(backing)] // len 3, cap 7 — spare capacity, shares backing
	sentinel := append([]byte(nil), backing...)

	got := keyAfter(k)

	if want := []byte("key\x00"); !bytes.Equal(got, want) {
		t.Errorf("keyAfter(%q) = %q, want %q", k, got, want)
	}
	// The shared backing array must be byte-for-byte untouched. The buggy
	// append writes 0 at backing[3], turning "keyZZZZ" into "key\x00ZZZ".
	if !bytes.Equal(backing, sentinel) {
		t.Errorf("keyAfter scribbled k's backing array: got %q, want %q", backing, sentinel)
	}
	if !bytes.Equal(k, []byte("key")) {
		t.Errorf("keyAfter mutated k's length view: got %q, want %q", k, "key")
	}
	// The result must own independent storage: writing through it must not leak
	// into k's backing array.
	got[0] = 'X'
	if backing[0] == 'X' {
		t.Error("keyAfter result aliases k's backing array (write to result leaked into backing)")
	}
}

// TestKeyAfter_Cases covers the boundary inputs: empty key, single byte, and a
// key whose backing array is exactly full (cap == len, where even the bare
// append would have reallocated).
func TestKeyAfter_Cases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"nil", nil, []byte{0}},
		{"empty", []byte{}, []byte{0}},
		{"single", []byte("a"), []byte("a\x00")},
		{"capped", []byte("abc"), []byte("abc\x00")}, // string-literal slice: cap == len
		{"trailing_zero", []byte{0x00}, []byte{0x00, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := keyAfter(tc.in); !bytes.Equal(got, tc.want) {
				t.Errorf("keyAfter(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestBatchSizeIteratorSaturates pins that StreamingModeIterator's per-fetch budget stops
// growing, the way libfdb_c's does.
//
// libfdb_c gives ITERATOR a byte target from a FIXED table (bindings/c/fdb_c.cpp:1006,
// {4096 ... 80000, 120000}) and clamps the index into it at fdb_c.cpp:1019
// (`iteration = std::min(iteration, max_iteration)`), so a C fetch never targets more than
// 120000 bytes no matter how long the scan runs. This client's budget is a row count, and
// it used to double without any clamp: `2 << (iteration-1)`. That makes the per-fetch
// budget grow to the size of the whole remaining range, and getRangeImpl materializes a
// whole batch before the iterator yields a row — so peak memory was Θ(rows) rather than
// bounded.
//
// The assertions below are the two independent directions the clamp can be wrong in, and
// each is checked separately: the budget must be BOUNDED (never exceeds the saturation
// value), and it must actually REACH that bound (a clamp set too low, or growth removed
// altogether, is equally wrong).
func TestBatchSizeIteratorSaturates(t *testing.T) {
	t.Parallel()

	// The row-model analog of C++'s last progression entry: growth stops at the same
	// iteration index C++ stops at, so the largest budget is 2 << (iteratorMaxIteration-1).
	const wantCap = 1024 // 2 << (maxIteration-1), maxIteration = 10 (fdb_c.cpp:1011)

	// A row budget far larger than the saturation value, so `remaining` never masks the
	// clamp — an unclamped batchSize returns the doubling value here, not `remaining`.
	const remaining = 1 << 30

	var peak int
	for iteration := 1; iteration <= 64; iteration++ {
		got := batchSize(StreamingModeIterator, iteration, remaining)
		if got <= 0 {
			t.Fatalf("iteration %d: batchSize = %d, want a positive row budget "+
				"(a non-positive budget stalls the iterator)", iteration, got)
		}
		if got > wantCap {
			t.Fatalf("iteration %d: batchSize = %d, want <= %d — the ITERATOR progression "+
				"must SATURATE (fdb_c.cpp:1019 clamps the progression index), not keep "+
				"doubling; an unbounded budget makes one fetch materialize the whole "+
				"remaining range", iteration, got, wantCap)
		}
		if got > peak {
			peak = got
		}
	}
	if peak != wantCap {
		t.Fatalf("peak ITERATOR batch = %d, want exactly %d — the progression must still "+
			"GROW up to the saturation point (clamping below it, or not growing at all, "+
			"diverges from the C progression just as an unbounded one does)", peak, wantCap)
	}

	// The pre-saturation shape is unchanged: doubling from 2 up to the cap. This is what
	// keeps early batches small (a cursor that reads two rows must not pull a huge batch)
	// and is the half of the progression that already matched.
	for iteration := 1; iteration <= 10; iteration++ {
		want := 2 << (iteration - 1)
		if got := batchSize(StreamingModeIterator, iteration, remaining); got != want {
			t.Fatalf("iteration %d: batchSize = %d, want %d (doubling below saturation)",
				iteration, got, want)
		}
	}

	// Past saturation every batch is the cap — the C client likewise re-uses the last
	// table entry rather than stopping or shrinking.
	for _, iteration := range []int{11, 20, 63, 1000} {
		if got := batchSize(StreamingModeIterator, iteration, remaining); got != wantCap {
			t.Fatalf("iteration %d: batchSize = %d, want %d (saturated)",
				iteration, got, wantCap)
		}
	}

	// `remaining` still clamps the saturated budget: the last fetch of a scan asks for
	// exactly what is left, never the full cap.
	if got := batchSize(StreamingModeIterator, 30, 7); got != 7 {
		t.Fatalf("batchSize(ITERATOR, 30, 7) = %d, want 7 (remaining clamps the budget)", got)
	}
}

// TestBatchSizeIteratorPeakIsCardinalityIndependent pins the property the saturation exists
// for: the largest single fetch of a full scan does not grow with the size of the range.
//
// This is the direct regression for the measured defect — draining 2M rows held ~951k
// entries in one fetch. It drives the same loop the iterator drives (batchSize, subtract,
// repeat) at two cardinalities an order of magnitude apart and requires the peak to be
// identical. Under the unclamped doubling the peak scales with the row count, so the two
// disagree and this fails.
func TestBatchSizeIteratorPeakIsCardinalityIndependent(t *testing.T) {
	t.Parallel()

	drainPeak := func(rows int) (peak, fetches int) {
		remaining := rows
		for iteration := 1; remaining > 0; iteration++ {
			got := batchSize(StreamingModeIterator, iteration, remaining)
			if got <= 0 {
				t.Fatalf("rows=%d iteration=%d: non-positive budget %d", rows, iteration, got)
			}
			if got > peak {
				peak = got
			}
			remaining -= got
			fetches++
		}
		return peak, fetches
	}

	smallPeak, smallFetches := drainPeak(200_000)
	largePeak, largeFetches := drainPeak(2_000_000)

	if smallPeak != largePeak {
		t.Fatalf("peak fetch grew with cardinality: 200k rows peaked at %d, 2M rows peaked "+
			"at %d — ITERATOR's per-fetch budget must be bounded and cardinality-"+
			"INDEPENDENT (libfdb_c caps its target at 120000 bytes regardless of range "+
			"size, fdb_c.cpp:1006-1019)", smallPeak, largePeak)
	}
	if largePeak > 1024 {
		t.Fatalf("peak fetch = %d, want <= 1024", largePeak)
	}
	// A bounded budget necessarily means the fetch COUNT grows with cardinality — that is
	// the trade the C client makes too, and pinning it here keeps a future "optimization"
	// from restoring the unbounded batch by making fetches constant again.
	if largeFetches <= smallFetches {
		t.Fatalf("fetches for 2M rows = %d, for 200k rows = %d: a saturated budget must "+
			"take MORE fetches for a larger range", largeFetches, smallFetches)
	}
}
