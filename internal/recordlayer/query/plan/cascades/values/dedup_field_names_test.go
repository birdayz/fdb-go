package values

import (
	"slices"
	"testing"
)

// DedupFieldNames is the one rule by which a name-addressed record type keeps a
// repeated output name addressable, and it must never produce a name the row
// already carries. Counting occurrences alone did: [X X X_2] became
// [X X_2 X_2], and a derived outer's ordinal type then held a genuinely
// repeated name that the lateral unnest over it (`d.x_2 AS e`) could not
// address. Every authored name is reserved before a suffix is minted.
func TestDedupFieldNamesNeverProducesARepeatedName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"no repeats", []string{"A", "B"}, []string{"A", "B"}},
		{"one repeat", []string{"A", "A"}, []string{"A", "A_2"}},
		{"three of a kind", []string{"K", "K", "K"}, []string{"K", "K_2", "K_3"}},
		{"authored suffix after the repeat", []string{"X", "X", "X_2"}, []string{"X", "X_3", "X_2"}},
		{"authored suffix before the repeat", []string{"X_2", "X", "X"}, []string{"X_2", "X", "X_3"}},
		{"authored suffix beside three of a kind", []string{"X", "X", "X_3", "X"}, []string{"X", "X_2", "X_3", "X_4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DedupFieldNames(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("DedupFieldNames(%v) = %v, want %v", tc.in, got, tc.want)
			}
			seen := map[string]struct{}{}
			for _, name := range got {
				if _, dup := seen[name]; dup {
					t.Fatalf("DedupFieldNames(%v) = %v holds %q twice", tc.in, got, name)
				}
				seen[name] = struct{}{}
			}
		})
	}
}
