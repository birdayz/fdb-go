package executor

import (
	"sync"
	"sync/atomic"
	"weak"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// recordTypeEqualMemoEntries bounds the memo; a full memo starts over.
const recordTypeEqualMemoEntries = 4096

// recordTypeEqualMemo remembers pairs of record type objects that compared
// equal. Types are immutable once built and a cursor's rows share one type
// object, so a per-row structural comparison of the same two objects (an edge
// binding's row type against its flowed type) is answered once. Keys hold
// both objects weakly: a key matches only the objects it was made from, never
// a later object at a reused address. RecordType.Equals itself is unchanged
// (it always descends, as Java's does).
var recordTypeEqualMemo struct {
	pairs atomic.Pointer[sync.Map] // recordTypePair → struct{}
	size  atomic.Int64
}

type recordTypePair struct {
	a, b weak.Pointer[values.RecordType]
}

// recordTypesEqual is a.Equals(b) for a record type b, memoised by object
// identity. A nil type equals only nil.
func recordTypesEqual(a *values.RecordType, b *values.RecordType) bool {
	if a == nil || b == nil {
		return a == b
	}
	pairs := recordTypeEqualMemo.pairs.Load()
	if pairs == nil {
		pairs = &sync.Map{}
		if !recordTypeEqualMemo.pairs.CompareAndSwap(nil, pairs) {
			pairs = recordTypeEqualMemo.pairs.Load()
		}
	}
	key := recordTypePair{weak.Make(a), weak.Make(b)}
	if _, ok := pairs.Load(key); ok {
		return true
	}
	if !a.Equals(b) {
		return false
	}
	if recordTypeEqualMemo.size.Add(1) > recordTypeEqualMemoEntries {
		recordTypeEqualMemo.pairs.Store(&sync.Map{})
		recordTypeEqualMemo.size.Store(0)
		return true
	}
	pairs.Store(key, struct{}{})
	return true
}
