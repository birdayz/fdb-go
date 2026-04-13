package client

import (
	"testing"
)

func TestIntersectConflictRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		writes []KeyRange
		reads  []KeyRange
		want   string // expected begin key as string
	}{
		{
			name:   "exact_overlap",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			reads:  []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			want:   "a",
		},
		{
			name:   "write_contains_read",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("z")}},
			reads:  []KeyRange{{Begin: []byte("m"), End: []byte("p")}},
			want:   "m", // max(a, m) = m
		},
		{
			name:   "read_contains_write",
			writes: []KeyRange{{Begin: []byte("m"), End: []byte("p")}},
			reads:  []KeyRange{{Begin: []byte("a"), End: []byte("z")}},
			want:   "m", // max(m, a) = m
		},
		{
			name:   "partial_overlap_write_first",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("m")}},
			reads:  []KeyRange{{Begin: []byte("g"), End: []byte("z")}},
			want:   "g", // max(a, g) = g
		},
		{
			name:   "partial_overlap_read_first",
			writes: []KeyRange{{Begin: []byte("g"), End: []byte("z")}},
			reads:  []KeyRange{{Begin: []byte("a"), End: []byte("m")}},
			want:   "g", // max(g, a) = g
		},
		{
			name:   "disjoint_fallback",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			reads:  []KeyRange{{Begin: []byte("d"), End: []byte("f")}},
			want:   "a", // no overlap → fallback to writes[0].Begin
		},
		{
			name:   "adjacent_not_overlapping",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			reads:  []KeyRange{{Begin: []byte("c"), End: []byte("f")}},
			want:   "a", // [a,c) and [c,f) don't overlap → fallback
		},
		{
			name:   "second_write_overlaps",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("b")}, {Begin: []byte("x"), End: []byte("z")}},
			reads:  []KeyRange{{Begin: []byte("y"), End: []byte("z")}},
			want:   "y", // second write [x,z) overlaps [y,z) → max(x, y) = y
		},
		{
			name:   "empty_reads_fallback",
			writes: []KeyRange{{Begin: []byte("a"), End: []byte("c")}},
			reads:  nil,
			want:   "a", // no reads → fallback
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := intersectConflictRanges(tt.writes, tt.reads)
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
