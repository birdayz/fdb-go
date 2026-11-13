package client

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestCommitConflictCoalescing(t *testing.T) {
	t.Parallel()
	r := func(begin, end string) KeyRange {
		return KeyRange{Begin: []byte(begin), End: []byte(end)}
	}
	for _, tc := range []struct {
		name   string
		in     []KeyRange
		want   []KeyRange
		withSC []KeyRange
	}{
		{name: "nil", withSC: []KeyRange{r("d", "j")}},
		{
			name: "single", in: []KeyRange{r("a", "b")},
			want: []KeyRange{r("a", "b")}, withSC: []KeyRange{r("a", "b"), r("d", "j")},
		},
		{
			name: "unordered overlapping adjacent duplicate and nested",
			in:   []KeyRange{r("h", "i"), r("c", "f"), r("a", "d"), r("b", "c"), r("a", "b"), r("f", "g"), r("a", "d")},
			want: []KeyRange{r("a", "g"), r("h", "i")}, withSC: []KeyRange{r("a", "j")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Include spare capacity: finalizing must not append into the snapshot.
			in := append(slices.Clone(tc.in), r("suffix", "untouched"))
			before := slices.Clone(in)
			for i := range before {
				before[i].Begin = bytes.Clone(before[i].Begin)
				before[i].End = bytes.Clone(before[i].End)
			}
			snapshot := in[:len(tc.in)]
			sc := r("d", "j")
			for _, coalesced := range []bool{false, true} {
				for _, added := range []bool{false, true} {
					want := slices.Clone(tc.in)
					if coalesced {
						want = slices.Clone(tc.want)
					}
					if added {
						if coalesced {
							want = tc.withSC
						} else {
							want = append(want, sc)
						}
					}
					sized := snapshot // coalesceCommitVectors's sizeConflicts
					if coalesced {
						sized = coalesceWriteConflicts(snapshot)
					}
					got := finalizeShipConflicts(sized, sc, added, coalesced)
					if !slices.EqualFunc(got, want, equalCommitConflictRange) {
						t.Fatalf("added=%v coalesced=%v: got %q, want %q", added, coalesced, got, want)
					}
					if !slices.EqualFunc(in, before, equalCommitConflictRange) {
						t.Fatalf("added=%v coalesced=%v modified snapshot: got %q, want %q", added, coalesced, in, before)
					}
				}
			}
			if got := coalesceWriteConflicts(snapshot); !slices.EqualFunc(got, tc.want, equalCommitConflictRange) {
				t.Fatalf("coalesce got %q, want %q", got, tc.want)
			}
			if !slices.EqualFunc(in, before, equalCommitConflictRange) {
				t.Fatal("coalescing modified snapshot")
			}
		})
	}
}

func equalCommitConflictRange(a, b KeyRange) bool {
	return bytes.Equal(a.Begin, b.Begin) && bytes.Equal(a.End, b.End)
}

func TestCommitConflictCoalescingAllocations(t *testing.T) {
	in := []KeyRange{
		{Begin: []byte("c"), End: []byte("e")},
		{Begin: []byte("a"), End: []byte("c")},
	}
	sc := KeyRange{Begin: []byte("d"), End: []byte("f")}
	sized := coalesceWriteConflicts(in)
	var got []KeyRange
	if allocs := testing.AllocsPerRun(100, func() {
		got = coalesceWriteConflicts(in)
	}); allocs != 1 {
		t.Errorf("coalescing allocations = %v, want 1 owned vector", allocs)
	}
	if len(got) != 1 || string(got[0].Begin) != "a" || string(got[0].End) != "e" {
		t.Fatalf("coalescing got %q, want [a,e)", got)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		got = finalizeShipConflicts(sized, sc, true, true)
	}); allocs != 1 {
		t.Errorf("finalization allocations = %v, want 1 owned vector", allocs)
	}
	if len(got) != 1 || string(got[0].Begin) != "a" || string(got[0].End) != "f" {
		t.Fatalf("finalization got %q, want [a,f)", got)
	}
}

// insertCoalesced must equal coalescing the whole vector again, and the
// presence check over coalesced vectors must equal the sorted-copy walk, for
// overlapping, touching, nested and empty ranges.
func TestInsertCoalescedMatchesCoalesce(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 11))
	key := func() []byte { return []byte{byte('a' + rng.IntN(12))} }
	ranges := func(n int) []KeyRange {
		out := make([]KeyRange, n)
		for i := range out {
			b, e := key(), key()
			if bytes.Compare(b, e) > 0 {
				b, e = e, b
			}
			out[i] = KeyRange{Begin: b, End: e}
		}
		return out
	}
	for iter := range 20000 {
		raw := ranges(rng.IntN(8))
		r := ranges(1)[0]
		sized := coalesceWriteConflicts(raw)
		want := coalesceOwnedWriteConflicts(append(slices.Clone(raw), r))
		if got := insertCoalesced(sized, r); !slices.EqualFunc(got, want, equalCommitConflictRange) {
			t.Fatalf("iter %d: insertCoalesced(%q, %q) = %q, want %q", iter, sized, r, got, want)
		}
		// Commit intersects the shipped read map: presence over the coalesced writes equals presence over the raw ones.
		reads := shipReadConflicts(ranges(rng.IntN(5)), false)
		_, want2 := intersectRanges(raw, reads)
		if got := conflictRangesIntersect(sized, reads); got != want2 {
			t.Fatalf("iter %d: intersect(%q, %q) = %v, want %v", iter, raw, reads, got, want2)
		}
	}
}
