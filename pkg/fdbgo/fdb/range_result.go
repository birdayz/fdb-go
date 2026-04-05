package fdb

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// RangeResult is the asynchronous result of a range read.
type RangeResult struct {
	tx       *transaction
	r        Range
	options  RangeOptions
	snapshot bool
}

func newRangeResult(tx *transaction, r Range, options RangeOptions) RangeResult {
	return RangeResult{tx: tx, r: r, options: options}
}

func newSnapshotRangeResult(tx *transaction, r Range, options RangeOptions) RangeResult {
	return RangeResult{tx: tx, r: r, options: options, snapshot: true}
}

func (rr RangeResult) doRange() ([]client.KeyValue, error) {
	begin, end := resolveRange(rr.r)
	limit := rr.options.Limit
	if limit == 0 {
		limit = 1<<31 - 1 // Apple API: 0 means unlimited
	}

	if rr.snapshot {
		snap := rr.tx.inner.Snapshot()
		if rr.options.Reverse {
			kvs, _, err := snap.GetRangeReverse(rr.tx.ctx, begin, end, limit)
			return kvs, err
		}
		kvs, _, err := snap.GetRange(rr.tx.ctx, begin, end, limit)
		return kvs, err
	}
	if rr.options.Reverse {
		kvs, _, err := rr.tx.inner.GetRangeReverse(rr.tx.ctx, begin, end, limit)
		return kvs, err
	}
	kvs, _, err := rr.tx.inner.GetRange(rr.tx.ctx, begin, end, limit)
	return kvs, err
}

// GetSliceWithError returns all key-value pairs in the range as a slice.
func (rr RangeResult) GetSliceWithError() ([]KeyValue, error) {
	kvs, err := rr.doRange()
	if err != nil {
		return nil, convertError(err)
	}
	result := make([]KeyValue, len(kvs))
	for i, kv := range kvs {
		result[i] = KeyValue{Key: Key(kv.Key), Value: kv.Value}
	}
	return result, nil
}

// GetSliceOrPanic returns all key-value pairs or panics on error.
func (rr RangeResult) GetSliceOrPanic() []KeyValue {
	s, err := rr.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return s
}

// Iterator returns a RangeIterator for streaming through the results.
func (rr RangeResult) Iterator() *RangeIterator {
	return &RangeIterator{rr: rr}
}

// RangeIterator returns key-value pairs one at a time from a range read.
// Call Advance() before each Get().
type RangeIterator struct {
	rr RangeResult

	once sync.Once
	kvs  []KeyValue
	err  error
	pos  int
}

// Advance moves to the next key-value pair. Returns true if there is a
// value available via Get(), false if the iteration is complete or an
// error occurred.
func (ri *RangeIterator) Advance() bool {
	ri.once.Do(func() {
		ri.kvs, ri.err = ri.rr.GetSliceWithError()
	})
	if ri.err != nil || ri.pos >= len(ri.kvs) {
		return false
	}
	return true
}

// Get returns the current key-value pair. Must be called after Advance()
// returns true.
func (ri *RangeIterator) Get() (KeyValue, error) {
	if ri.err != nil {
		return KeyValue{}, ri.err
	}
	if ri.pos >= len(ri.kvs) {
		return KeyValue{}, Error{Code: 2000}
	}
	kv := ri.kvs[ri.pos]
	ri.pos++
	return kv, nil
}

// MustGet returns the current key-value pair or panics.
func (ri *RangeIterator) MustGet() KeyValue {
	kv, err := ri.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

// resolveRange extracts begin/end byte slices from a Range.
func resolveRange(r Range) (begin, end []byte) {
	if er, ok := r.(ExactRange); ok {
		b, e := er.FDBRangeKeys()
		return b.FDBKey(), e.FDBKey()
	}
	bs, es := r.FDBRangeKeySelectors()
	return bs.FDBKeySelector().Key.FDBKey(), es.FDBKeySelector().Key.FDBKey()
}
