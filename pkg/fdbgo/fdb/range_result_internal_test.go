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

// The two ITERATOR batching tests that lived here — TestBatchSizeIteratorSaturates and
// TestBatchSizeIteratorPeakIsCardinalityIndependent — were removed with the row-model
// batchSize they exercised. They asserted that a per-fetch ROW budget doubled to a 1024-row
// clamp, which is not how this client divides a range any more: the budget is a per-fetch BYTE
// target taken from libfdb_c's own table.
//
// Their coverage did not go with them, and is stronger for being split across the two things
// they conflated:
//
//   - The progression's VALUES and its saturation are pinned exactly, per iteration, by
//     TestModeTargetBytes_IteratorProgression in modebytes_test.go — including that the index
//     is CLAMPED rather than running off the table (fdb_c.cpp:1019).
//   - That a real scan actually reaches saturation, and that the peak does not track the size
//     of the range, is pinned end to end against a live cluster by
//     TestRangeIterator_DrainsPastIteratorSaturation.
//   - That the first fetch uses the progression's FIRST entry — the dimension neither old test
//     could see, and where a real off-by-one hid — is pinned by
//     TestRangeIterator_FirstFetchUsesFirstProgressionEntry.
