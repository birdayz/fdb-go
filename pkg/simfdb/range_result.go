package simfdb

import fdb "fdb.dev/pkg/fdbgo/fdb"

// readyRangeResult is a fully-materialized RangeResult: the whole slice is known, so the
// iterator just walks it. (SimFDB reads resolve synchronously — there is no streaming.)
type readyRangeResult struct{ kvs []fdb.KeyValue }

func newReadyRangeResult(kvs []fdb.KeyValue) fdb.RangeResult { return readyRangeResult{kvs} }

func (r readyRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) { return r.kvs, nil }
func (r readyRangeResult) GetSliceOrPanic() []fdb.KeyValue            { return r.kvs }
func (r readyRangeResult) Iterator() fdb.RangeIterator                { return &rangeIter{kvs: r.kvs, idx: -1} }

// rangeIter streams a materialized slice. Per the RangeIterator contract, Advance() moves to
// the next element and reports whether one exists; Get() returns the current element
// idempotently (no advance).
type rangeIter struct {
	kvs []fdb.KeyValue
	idx int
}

func (it *rangeIter) Advance() bool {
	it.idx++
	return it.idx < len(it.kvs)
}

func (it *rangeIter) Get() (fdb.KeyValue, error) {
	if it.idx < 0 || it.idx >= len(it.kvs) {
		// Zero value, no error — matches the pure-Go client / Apple binding, where Get()
		// before the first Advance() or past the end returns an empty KeyValue rather than
		// erroring. The record layer's cursors rely on this (they call Advance() then Get()).
		return fdb.KeyValue{}, nil
	}
	return it.kvs[it.idx], nil
}

func (it *rangeIter) MustGet() fdb.KeyValue {
	kv, err := it.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

func (it *rangeIter) SetTraceLog(func(iteration, requested, returned int, more bool, err error)) {}

// keyAfter returns the smallest key strictly greater than k: a fresh copy of k with a trailing
// 0x00. It must copy into independent storage (never append into k's backing array), matching
// the pure-Go client's keyAfter invariant.
func keyAfter(k []byte) []byte {
	result := make([]byte, len(k)+1)
	copy(result, k)
	result[len(k)] = 0
	return result
}
