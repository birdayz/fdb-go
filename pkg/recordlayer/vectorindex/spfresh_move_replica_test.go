package vectorindex

import (
	"slices"
	"testing"
)

func TestSPFreshMoveReplica(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mem            []int64
		want           []int64
		place, changed bool
	}{
		{"moves the replica", []int64{7, 9}, []int64{8, 9}, true, true},
		{"target already holds one: drop", []int64{7, 8}, []int64{8}, false, true},
		{"membership disclaims the parent: orphan", []int64{9}, []int64{9}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, place, changed := spfreshMoveReplica(slices.Clone(tc.mem), 7, 8)
			if !slices.Equal(got, tc.want) || place != tc.place || changed != tc.changed {
				t.Fatalf("spfreshMoveReplica(%v, 7, 8) = %v, %v, %v; want %v, %v, %v", tc.mem, got, place, changed, tc.want, tc.place, tc.changed)
			}
		})
	}
}
