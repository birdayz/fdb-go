package vectorindex

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Java's SlidingWindowIndexMaintainer layout under keyspace 10, restated here
// as the tests' independent oracle rather than read from the maintainer.
const (
	slidingWindowEntriesSubspaceKey = 0
	slidingWindowMetaSubspaceKey    = 1
	slidingWindowCountKey           = 3
	slidingWindowBoundaryKey        = 4
)

// decodeSlidingWindowLong reads the window count, which Java stores as a
// packed one-element tuple; an absent key is zero.
func decodeSlidingWindowLong(b []byte) (int64, error) {
	if b == nil {
		return 0, nil
	}
	t, err := tuple.Unpack(b)
	if err != nil {
		return 0, err
	}
	if len(t) != 1 {
		return 0, fmt.Errorf("count tuple %v, want one element", t)
	}
	v, ok := t[0].(int64)
	if !ok {
		return 0, fmt.Errorf("count element %T, want int64", t[0])
	}
	return v, nil
}
