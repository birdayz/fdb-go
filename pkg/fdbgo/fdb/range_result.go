package fdb

import (
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// RangeResult is the asynchronous result of a range read. It is an INTERFACE (RFC-109)
// so a non-pure-Go backend (libfdb_c) can return its own implementation; the pure-Go
// client returns goRangeResult.
type RangeResult interface {
	// GetSliceWithError materializes the whole range into a slice.
	GetSliceWithError() ([]KeyValue, error)
	// GetSliceOrPanic is GetSliceWithError, panicking on error.
	GetSliceOrPanic() []KeyValue
	// Iterator streams the range in batches per the StreamingMode.
	Iterator() RangeIterator
}

// RangeIterator streams key-value pairs from a range read. Call Advance() before each
// Get(); Get() is idempotent (returns the current element without advancing). Interface
// for the same backend-substitution reason as RangeResult (RFC-109).
type RangeIterator interface {
	Advance() bool
	Get() (KeyValue, error)
	MustGet() KeyValue
	SetTraceLog(fn func(iteration, requested, returned int, more bool, err error))
}

// goRangeResult is the pure-Go client's RangeResult implementation.
type goRangeResult struct {
	tx       *transaction
	r        Range
	options  RangeOptions
	snapshot bool
}

func newRangeResult(tx *transaction, r Range, options RangeOptions) RangeResult {
	return goRangeResult{tx: tx, r: r, options: options}
}

func newSnapshotRangeResult(tx *transaction, r Range, options RangeOptions) RangeResult {
	return goRangeResult{tx: tx, r: r, options: options, snapshot: true}
}

// keyAfter returns the smallest key strictly greater than k — a fresh copy of
// k with a trailing 0x00 byte. It MUST copy into independent storage:
// `append([]byte(k), 0)` would scribble k's own backing array (and alias it)
// whenever cap(k) > len(k). The range-reply parser currently hands out
// length-capped key slices (data[pos:pos+n:pos+n]), so that bare append happens
// to reallocate today — but relying on that invariant is a latent corruption
// bug. The exact-sized make+copy guarantees a single allocation and an exact
// cap (len(k)+1), documenting the no-alias invariant unambiguously.
func keyAfter(k []byte) []byte {
	result := make([]byte, len(k)+1)
	copy(result, k)
	result[len(k)] = 0
	return result
}

// effectiveLimit returns the limit to use for a range read.
// Apple API: Limit=0 means unlimited.
func effectiveLimit(limit int) int {
	if limit == 0 {
		return math.MaxInt32
	}
	return limit
}

func (rr goRangeResult) doRangeWithLimit(begin, end []byte, limit int) ([]client.KeyValue, bool, error) {
	if rr.snapshot {
		snap := rr.tx.inner.Snapshot()
		if rr.options.Reverse {
			return snap.GetRangeReverse(rr.tx.ctx, begin, end, limit)
		}
		return snap.GetRange(rr.tx.ctx, begin, end, limit)
	}
	if rr.options.Reverse {
		return rr.tx.inner.GetRangeReverse(rr.tx.ctx, begin, end, limit)
	}
	return rr.tx.inner.GetRange(rr.tx.ctx, begin, end, limit)
}

// GetSliceWithError returns all key-value pairs in the range as a slice.
// Always fetches all results regardless of streaming mode.
//
// WARNING: This eagerly loads all matching key-value pairs into memory.
// For large ranges this can cause excessive memory usage. Prefer
// Iterator() for streaming large result sets.
func (rr goRangeResult) GetSliceWithError() ([]KeyValue, error) {
	begin, end, err := resolveRange(rr.tx, rr.r)
	if err != nil {
		return nil, convertError(err)
	}
	limit := effectiveLimit(rr.options.Limit)
	kvs, _, err := rr.doRangeWithLimit(begin, end, limit)
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
func (rr goRangeResult) GetSliceOrPanic() []KeyValue {
	s, err := rr.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return s
}

// Iterator returns a RangeIterator for streaming through results.
// The iterator fetches data in batches according to the StreamingMode.
func (rr goRangeResult) Iterator() RangeIterator {
	begin, end, err := resolveRange(rr.tx, rr.r)
	if err != nil {
		return &goRangeIterator{err: convertError(err)}
	}
	return &goRangeIterator{
		rr:        rr,
		begin:     begin,
		end:       end,
		remaining: effectiveLimit(rr.options.Limit),
		iteration: 1,
		index:     -1,
	}
}

// batchSize returns the number of rows to fetch for a given streaming mode
// and iteration number. Matches the C client's behavior:
//   - WANT_ALL: fetch everything
//   - EXACT: fetch exact limit
//   - ITERATOR: start small, double each iteration
//   - SMALL/MEDIUM/LARGE/SERIAL: fixed sizes
func batchSize(mode StreamingMode, iteration int, remaining int) int {
	switch mode {
	case StreamingModeWantAll:
		return remaining
	case StreamingModeExact:
		return remaining
	case StreamingModeIterator:
		// C client: starts at ~256 bytes (~2 KVs), doubles each iteration.
		// We use row count since our client doesn't support limitBytes yet.
		base := 2 << (iteration - 1) // 2, 4, 8, 16, ...
		if base <= 0 || base > remaining {
			return remaining
		}
		return base
	case StreamingModeSmall:
		return min(10, remaining)
	case StreamingModeMedium:
		return min(100, remaining)
	case StreamingModeLarge:
		return min(1000, remaining)
	case StreamingModeSerial:
		return min(500, remaining)
	default:
		return remaining
	}
}

// RangeIterator returns key-value pairs one at a time from a range read.
// Fetches data lazily in batches based on the StreamingMode.
// Call Advance() before each Get(). Get() is idempotent — it returns the
// current element without advancing. Only Advance() moves forward.
type goRangeIterator struct {
	rr        goRangeResult
	begin     []byte
	end       []byte
	remaining int
	iteration int

	kvs       []KeyValue
	err       error
	pos       int
	index     int // position returned by Get(); set by Advance()
	exhausted bool

	// traceLog, when non-nil, is called after each batch fetch for debugging.
	traceLog func(iteration, requested, returned int, more bool, err error)
}

// SetTraceLog sets a callback invoked after each batch fetch. For debugging.
func (ri *goRangeIterator) SetTraceLog(fn func(iteration, requested, returned int, more bool, err error)) {
	ri.traceLog = fn
}

// Advance moves to the next key-value pair. Returns true if there is a
// value available via Get(), false at end of iteration or on error.
func (ri *goRangeIterator) Advance() bool {
	if ri.err != nil {
		return false
	}

	// If we still have buffered results, consume the next one.
	if ri.pos < len(ri.kvs) {
		ri.index = ri.pos
		ri.pos++
		return true
	}

	// No more buffered results. If the previous batch was the last, we're done.
	if ri.exhausted || ri.remaining <= 0 {
		return false
	}

	// Fetch the next batch as a serializable read (unless the whole RangeResult is a
	// snapshot — doRangeWithLimit honors rr.snapshot). EVERY batch adds its own
	// read-conflict, clamped to the extent it returned (RFC-121); since each batch reads a
	// distinct contiguous sub-range ([ri.begin, ri.end) advances below), the union of the
	// per-batch conflicts covers exactly the consumed range — matching the C client, where
	// each fdb_transaction_get_range call adds a conflict for the batch it returned. Reading
	// later batches under snapshot (no conflict) — as this did before — is UNSAFE now that
	// RFC-121 clamps the first batch's conflict to its returned prefix: the rows in later
	// batches would carry no read-conflict, so a concurrent write to one of them could let
	// this transaction commit with stale data (lost serializability).
	batch := batchSize(ri.rr.options.Mode, ri.iteration, ri.remaining)
	ri.iteration++

	kvs, more, err := ri.rr.doRangeWithLimit(ri.begin, ri.end, batch)

	// Trace: log every batch for debugging premature exhaustion.
	if ri.traceLog != nil {
		ri.traceLog(ri.iteration-1, batch, len(kvs), more, err)
	}

	if err != nil {
		ri.err = convertError(err)
		return false
	}

	if len(kvs) == 0 {
		ri.exhausted = true
		return false
	}

	// Convert to fdb.KeyValue.
	ri.kvs = make([]KeyValue, len(kvs))
	for i, kv := range kvs {
		ri.kvs[i] = KeyValue{Key: Key(kv.Key), Value: kv.Value}
	}
	ri.index = 0
	ri.pos = 1
	ri.remaining -= len(ri.kvs)

	// Update the scan boundary for the next batch.
	lastKey := ri.kvs[len(ri.kvs)-1].Key
	if ri.rr.options.Reverse {
		// Next batch: end at the last key we received (exclusive).
		ri.end = append([]byte(nil), lastKey...)
	} else {
		// Next batch: begin after the last key we received.
		ri.begin = keyAfter(lastKey)
	}

	if !more {
		ri.exhausted = true
	}

	return true
}

// Get returns the current key-value pair. Get is idempotent — calling it
// multiple times without Advance() returns the same element.
func (ri *goRangeIterator) Get() (KeyValue, error) {
	if ri.err != nil {
		return KeyValue{}, ri.err
	}
	if ri.index < 0 || ri.index >= len(ri.kvs) {
		return KeyValue{}, nil // matches Apple: zero value before first Advance()
	}
	return ri.kvs[ri.index], nil
}

// MustGet returns the current key-value pair or panics.
func (ri *goRangeIterator) MustGet() KeyValue {
	kv, err := ri.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

// isTrivialSelector returns true when the selector is FirstGreaterOrEqual
// (OrEqual=false, Offset=1) — the only form that can be resolved by just
// extracting the key bytes. Matches Apple Go binding convention.
func isTrivialSelector(ks KeySelector) bool {
	return !ks.OrEqual && ks.Offset == 1
}

// resolveSelector resolves a key selector to raw bytes. Trivial selectors
// (FirstGreaterOrEqual) are resolved locally; all others require a GetKey
// round-trip to the database.
func resolveSelector(tx *transaction, ks KeySelector) ([]byte, error) {
	if isTrivialSelector(ks) {
		// FirstGreaterOrEqual(k) — trivial, no round-trip needed.
		// Defensive copy to avoid sharing caller's backing array.
		key := ks.Key.FDBKey()
		out := make([]byte, len(key))
		copy(out, key)
		return out, nil
	}
	if ks.OrEqual && ks.Offset == 1 {
		// FirstGreaterThan(k) = FirstGreaterOrEqual(k + \x00).
		// Resolve client-side to avoid unnecessary GetKey round-trip.
		key := ks.Key.FDBKey()
		out := make([]byte, len(key)+1)
		copy(out, key)
		return out, nil
	}
	// OrEqual values match the wire convention. Pass directly.
	k, err := tx.inner.GetKey(tx.ctx, ks.Key.FDBKey(), ks.OrEqual, int32(ks.Offset))
	if err != nil {
		return nil, err
	}
	return k, nil
}

// resolveRange extracts begin/end byte slices from a Range. For ExactRange
// the keys are used directly. For non-trivial key selectors (anything other
// than FirstGreaterOrEqual) the selector is resolved via GetKey.
func resolveRange(tx *transaction, r Range) (begin, end []byte, err error) {
	if er, ok := r.(ExactRange); ok {
		b, e := er.FDBRangeKeys()
		return b.FDBKey(), e.FDBKey(), nil
	}
	bs, es := r.FDBRangeKeySelectors()
	begin, err = resolveSelector(tx, bs.FDBKeySelector())
	if err != nil {
		return nil, nil, err
	}
	end, err = resolveSelector(tx, es.FDBKeySelector())
	if err != nil {
		return nil, nil, err
	}
	return begin, end, nil
}
