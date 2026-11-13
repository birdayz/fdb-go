package recordlayer

import (
	"math"
	"math/big"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestTextMergeKeepsPackedComparatorContrasts(t *testing.T) {
	t.Parallel()
	// Java KeyComparisons uses object Comparable order, not Tuple.compareTo.
	// The live-JVM conformance counterpart pins its unnormalized outputs.
	for _, tc := range []struct {
		name        string
		left, right tuple.Tuple
		want        int
	}{
		{"UUID signed high bit", tuple.Tuple{tuple.UUID{0x10}}, tuple.Tuple{tuple.UUID{0x90}}, -1},
		{"UTF-8 versus UTF-16", tuple.Tuple{"\U00010000"}, tuple.Tuple{"\uE000"}, 1},
		{"float NaN payloads", tuple.Tuple{math.Float32frombits(0x7fc00001)}, tuple.Tuple{math.Float32frombits(0x7fc00002)}, -1},
		{"double NaN payloads", tuple.Tuple{math.Float64frombits(0x7ff8000000000001)}, tuple.Tuple{math.Float64frombits(0x7ff8000000000002)}, -1},
		{"nested NaN payloads", tuple.Tuple{tuple.Tuple{math.Float64frombits(0x7ff8000000000001)}}, tuple.Tuple{tuple.Tuple{math.Float64frombits(0x7ff8000000000002)}}, -1},
		{"Long versus BigInteger", tuple.Tuple{int64(1)}, tuple.Tuple{new(big.Int).Lsh(big.NewInt(1), 80)}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compareKeys(tc.left, tc.right)
			if err != nil || got != tc.want {
				t.Fatalf("packed merge comparison=%d, err=%v, want %d", got, err, tc.want)
			}
		})
	}
}
