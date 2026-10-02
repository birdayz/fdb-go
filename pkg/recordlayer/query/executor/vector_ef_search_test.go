package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// Without a query option a VECTOR scan searches with Java's default,
// HnswVectorIndexEngine.efSearch: min(max(4k, 64), max(k, 400)) over the scan
// limit. An explicit option is used as given, raised to k in top-k mode.
// SPFresh keeps 0, its maintainer's own default.
func TestVectorEfSearchIsJavas(t *testing.T) {
	t.Parallel()
	five, threeHundred := 5, 300
	for _, c := range []struct {
		name         string
		indexType    string
		explicit     *int
		limit        int
		selfLimiting bool
		want         int
	}{
		{"k 1", recordlayer.IndexTypeVector, nil, 1, true, 64},
		{"k 10", recordlayer.IndexTypeVector, nil, 10, true, 64},
		{"k 50", recordlayer.IndexTypeVector, nil, 50, true, 200},
		{"k 100", recordlayer.IndexTypeVector, nil, 100, true, 400},
		{"k 500", recordlayer.IndexTypeVector, nil, 500, true, 500},
		{"ordered horizon 200", recordlayer.IndexTypeVector, nil, 200, false, 400},
		{"explicit below k, top-k", recordlayer.IndexTypeVector, &five, 10, true, 10},
		{"explicit below k, ordered", recordlayer.IndexTypeVector, &five, 10, false, 5},
		{"explicit above k", recordlayer.IndexTypeVector, &threeHundred, 10, true, 300},
		{"SPFresh default", recordlayer.IndexTypeVectorSPFresh, nil, 10, true, 0},
		{"SPFresh explicit", recordlayer.IndexTypeVectorSPFresh, &threeHundred, 10, true, 300},
	} {
		if got := vectorEfSearch(c.indexType, c.explicit, c.limit, c.selfLimiting); got != c.want {
			t.Errorf("%s: efSearch %d, want %d", c.name, got, c.want)
		}
	}
}
