package client

import (
	"bytes"
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
					got := finalizeShipConflicts(snapshot, sc, added, coalesced)
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
		got = finalizeShipConflicts(in, sc, true, true)
	}); allocs != 1 {
		t.Errorf("finalization allocations = %v, want 1 owned vector", allocs)
	}
	if len(got) != 1 || string(got[0].Begin) != "a" || string(got[0].End) != "f" {
		t.Fatalf("finalization got %q, want [a,f)", got)
	}
}
