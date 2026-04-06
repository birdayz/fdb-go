package fdb

import (
	"math"
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
	begin, end, err := resolveRange(rr.tx, rr.r)
	if err != nil {
		return nil, err
	}
	limit := rr.options.Limit
	if limit == 0 {
		// Apple API: Limit=0 means unlimited. The underlying client uses
		// limit>0 as a loop condition, so we pass MaxInt32 (matching the
		// FDB wire protocol's 32-bit limit field).
		limit = math.MaxInt32
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
// WARNING: loads all results into memory in a single round-trip. For large
// ranges without a Limit, this may exceed FDB's 5-second transaction limit
// or cause OOM. Set RangeOptions.Limit for large scans.
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
	return &RangeIterator{rr: rr, index: -1}
}

// RangeIterator returns key-value pairs one at a time from a range read.
// Call Advance() before each Get().
//
// NOTE: This implementation eagerly loads all results on the first Advance()
// call. StreamingMode is accepted for API compatibility but does not affect
// fetching behavior. For large ranges, set an explicit Limit to avoid OOM.
// Lazy paging with streaming mode support is implemented in a later PR.
type RangeIterator struct {
	rr RangeResult

	once  sync.Once
	kvs   []KeyValue
	err   error
	pos   int // next position to advance to
	index int // current element (set by Advance)
}

// Advance moves the cursor forward and returns true if there is a value
// available via Get(). Matches Apple binding: Advance() moves, Get() reads
// the current element without advancing.
func (ri *RangeIterator) Advance() bool {
	ri.once.Do(func() {
		ri.kvs, ri.err = ri.rr.GetSliceWithError()
	})
	if ri.err != nil || ri.pos >= len(ri.kvs) {
		return false
	}
	ri.index = ri.pos
	ri.pos++
	return true
}

// Get returns the current key-value pair. Idempotent — multiple calls
// after a single Advance() return the same element.
func (ri *RangeIterator) Get() (KeyValue, error) {
	if ri.err != nil {
		return KeyValue{}, ri.err
	}
	if ri.index < 0 || ri.index >= len(ri.kvs) {
		return KeyValue{}, Error{Code: 2000}
	}
	return ri.kvs[ri.index], nil
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
// For ExactRange, uses keys directly. For SelectorRange, resolves
// key selectors via GetKey if they have non-trivial OrEqual/Offset.
func resolveRange(tx *transaction, r Range) (begin, end []byte, err error) {
	if er, ok := r.(ExactRange); ok {
		b, e := er.FDBRangeKeys()
		return b.FDBKey(), e.FDBKey(), nil
	}
	bs, es := r.FDBRangeKeySelectors()
	bks := bs.FDBKeySelector()
	eks := es.FDBKeySelector()

	// FirstGreaterOrEqual(k) is the trivial case (OrEqual=true, Offset=1).
	// Anything else requires a GetKey round-trip to resolve.
	beginKey, err := resolveSelector(tx, bks)
	if err != nil {
		return nil, nil, err
	}
	endKey, err := resolveSelector(tx, eks)
	if err != nil {
		return nil, nil, err
	}
	return beginKey, endKey, nil
}

func resolveSelector(tx *transaction, ks KeySelector) ([]byte, error) {
	if ks.OrEqual && ks.Offset == 1 {
		// FirstGreaterOrEqual(k) — trivial, no round-trip needed.
		return ks.Key.FDBKey(), nil
	}
	if !ks.OrEqual && ks.Offset == 1 {
		// FirstGreaterThan(k) = FirstGreaterOrEqual(k + \x00).
		// Resolve client-side to avoid unnecessary GetKey round-trip.
		key := ks.Key.FDBKey()
		// Copy to avoid mutating caller's backing array.
		out := make([]byte, len(key)+1)
		copy(out, key)
		return out, nil
	}
	// Non-trivial selector — resolve via GetKey.
	k, err := tx.inner.GetKey(tx.ctx, ks.Key.FDBKey(), ks.OrEqual, int32(ks.Offset))
	if err != nil {
		return nil, err
	}
	return k, nil
}
