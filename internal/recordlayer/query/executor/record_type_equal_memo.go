package executor

import (
	"sync"
	"sync/atomic"
	"weak"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

// recordTypeEqualMemoEntries bounds the memo; a full memo starts over.
const recordTypeEqualMemoEntries = 4096

// recordTypeEqualMemo remembers pairs of record type objects that compared
// equal. Types are immutable once built and a cursor's rows share one type
// object, so a per-row structural comparison of the same two objects (an edge
// binding's row type against its flowed type) is answered once. Keys hold
// both objects weakly: a key matches only the objects it was made from, never
// a later object at a reused address. RecordType.Equals itself is unchanged:
// unlike Java's Type.Record.equals (Type.java:2400-2402) it deliberately has
// no identity short-circuit, so a cyclic record type fails at its first
// comparison (values/proto_type_test.go, DIVERGENCES.md).
var recordTypeEqualMemo struct {
	current atomic.Pointer[memoGeneration]
	mu      sync.Mutex // serializes miss inserts and generation rollover
}

type memoGeneration struct {
	pairs sync.Map     // recordTypePair → struct{}
	size  atomic.Int64 // exact entry count in pairs; mutated only under mu
}

type recordTypePair struct {
	a, b weak.Pointer[values.RecordType]
}

func init() {
	recordTypeEqualMemo.current.Store(&memoGeneration{})
}

// recordTypesEqual is a.Equals(b) for a record type b, memoised by object
// identity. A nil type equals only nil.
func recordTypesEqual(a *values.RecordType, b *values.RecordType) bool {
	if a == nil || b == nil {
		return a == b
	}
	gen := recordTypeEqualMemo.current.Load()
	key := recordTypePair{weak.Make(a), weak.Make(b)}
	if _, ok := gen.pairs.Load(key); ok {
		return true
	}
	if !a.Equals(b) {
		return false
	}
	recordTypeEqualMemo.mu.Lock()
	defer recordTypeEqualMemo.mu.Unlock()
	gen = recordTypeEqualMemo.current.Load()
	if _, ok := gen.pairs.Load(key); ok {
		return true
	}
	if gen.size.Load() >= recordTypeEqualMemoEntries {
		// Populate the replacement generation before publishing so old
		// generations stay immutable at cap and concurrent readers never see
		// an out-of-sync counter reset.
		next := &memoGeneration{}
		next.pairs.Store(key, struct{}{})
		next.size.Store(1)
		recordTypeEqualMemo.current.Store(next)
		return true
	}
	gen.pairs.Store(key, struct{}{})
	gen.size.Add(1)
	return true
}
