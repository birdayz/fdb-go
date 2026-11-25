package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
)

func TestIndexScanCanPushSkipRangeShape(t *testing.T) {
	t.Parallel()
	for _, indexType := range []string{
		recordlayer.IndexTypeValue,
		recordlayer.IndexTypeVersion,
		recordlayer.IndexTypeRank,
		recordlayer.IndexTypeTimeWindowLeaderboard,
		recordlayer.IndexTypePermutedMin,
		recordlayer.IndexTypePermutedMax,
	} {
		t.Run(indexType, func(t *testing.T) {
			t.Parallel()
			for _, test := range []struct {
				name string
				spec scanRangeSetSpec
				want bool
			}{
				{name: "no alternatives is one leaf", want: true},
				{name: "one singleton", spec: scanRangeSetSpec{alternativeCounts: []uint32{1}}, want: true},
				{name: "multiple singleton components", spec: scanRangeSetSpec{alternativeCounts: []uint32{1, 1, 1}}, want: true},
				{name: "empty", spec: scanRangeSetSpec{empty: true}},
				{name: "empty with singleton components", spec: scanRangeSetSpec{empty: true, alternativeCounts: []uint32{1, 1}}},
				{name: "zero alternatives", spec: scanRangeSetSpec{alternativeCounts: []uint32{0}}},
				{name: "later zero alternatives", spec: scanRangeSetSpec{alternativeCounts: []uint32{1, 0}}},
				{name: "two alternatives", spec: scanRangeSetSpec{alternativeCounts: []uint32{2}}},
				{name: "later two alternatives", spec: scanRangeSetSpec{alternativeCounts: []uint32{1, 2, 1}}},
				{name: "maximum alternatives", spec: scanRangeSetSpec{alternativeCounts: []uint32{^uint32(0)}}},
				{name: "residual filter", spec: scanRangeSetSpec{keyFilter: &scanKeyFilter{}}},
				{name: "singleton with residual filter", spec: scanRangeSetSpec{alternativeCounts: []uint32{1, 1}, keyFilter: &scanKeyFilter{}}},
			} {
				t.Run(test.name, func(t *testing.T) {
					t.Parallel()
					if got := indexScanCanPushSkip(indexType, test.spec); got != test.want {
						t.Fatalf("indexScanCanPushSkip(%q, %+v) = %t, want %t", indexType, test.spec, got, test.want)
					}
				})
			}
		})
	}
}

// Keep this executor pushdown scoped to its supported index scan families.
func TestIndexScanCanPushSkipMaintainerGate(t *testing.T) {
	t.Parallel()
	for _, indexType := range []string{
		recordlayer.IndexTypeCount,
		recordlayer.IndexTypeCountNotNull,
		recordlayer.IndexTypeCountUpdates,
		recordlayer.IndexTypeSum,
		recordlayer.IndexTypeMinEver,
		recordlayer.IndexTypeMaxEver,
		recordlayer.IndexTypeMinEverLong,
		recordlayer.IndexTypeMaxEverLong,
		recordlayer.IndexTypeMinEverTuple,
		recordlayer.IndexTypeMaxEverTuple,
		recordlayer.IndexTypeMaxEverVersion,
		recordlayer.IndexTypeBitmapValue,
		recordlayer.IndexTypeText,
		recordlayer.IndexTypeMultidimensional,
		recordlayer.IndexTypeVector,
		recordlayer.IndexTypeVectorSPFresh,
		"unknown",
		"",
	} {
		t.Run(indexType, func(t *testing.T) {
			t.Parallel()
			for _, counts := range [][]uint32{nil, {1}, {1, 1}} {
				spec := scanRangeSetSpec{alternativeCounts: counts}
				if indexScanCanPushSkip(indexType, spec) {
					t.Fatalf("indexScanCanPushSkip(%q, %+v) accepted an index type outside the executor pushdown gate", indexType, spec)
				}
			}
		})
	}
}
