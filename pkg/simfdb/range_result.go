package simfdb

import (
	"bytes"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// readyRangeResult is a fully-materialized RangeResult over which consumption is still MODELLED
// in batches.
//
// SimFDB resolves a range read synchronously and holds the whole result, so it could hand
// everything back at once. It must not, for two reasons that are the same reason: the real
// backend fetches in batches sized by the streaming mode (fdb.BatchSize), and
//
//   - EVERY BATCH TAKES ITS OWN READ CONFLICT RANGE, clamped to what that batch returned
//     (fdb/range_result.go:245-283). A cursor abandoned after one page has therefore conflicted
//     over ONE PAGE, not over the whole requested range. Registering the full extent at
//     GetRange() call time over-conflicts every early-abandoned scan — and the record layer's
//     cursors abandon constantly, at every continuation boundary.
//   - Batch boundaries are where a continuation lands, so a sim that batched differently would
//     page differently from the client it is standing in for.
//
// GetSliceWithError is the exception and matches the client's: it asks for the whole budget in
// one fetch (goRangeResult.GetSliceWithError calls doRangeWithLimit once), so it takes one
// conflict over the full extent.
type readyRangeResult struct {
	tx       *simTxn
	snapshot bool
	kvs      []fdb.KeyValue // materialized, already limit-truncated and direction-ordered
	more     bool           // the limit was met (see GetRange)
	options  fdb.RangeOptions
	// reqBegin/reqEnd are the RESOLVED request bounds the conflict extent is derived from.
	reqBegin, reqEnd []byte
	err              error
}

func (r *readyRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	if r.err != nil {
		return nil, r.err
	}
	if !r.snapshot {
		cb, ce := rangeConflictExtent(r.reqBegin, r.reqEnd, r.kvs, r.more, r.options.Reverse)
		if bytes.Compare(cb, ce) < 0 {
			r.tx.addFilteredReadConflict(cb, ce)
		}
	}
	return r.kvs, nil
}

func (r *readyRangeResult) GetSliceOrPanic() []fdb.KeyValue {
	kvs, err := r.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return kvs
}

func (r *readyRangeResult) Iterator() fdb.RangeIterator {
	if r.err != nil {
		return &rangeIter{err: r.err, idx: -1}
	}
	// The same two validations the client's Iterator() performs, in the same order: a row limit
	// below ROW_LIMIT_UNLIMITED(-1) is range_limits_invalid(2012), and EXACT without a limit is
	// exact_mode_without_limits(2210) (libfdb_c validate_and_update_parameters).
	if r.options.Limit < -1 {
		return &rangeIter{err: fdb.Error{Code: 2012}, idx: -1}
	}
	if r.options.Mode == fdb.StreamingModeExact && r.options.Limit <= 0 {
		return &rangeIter{err: fdb.Error{Code: 2210}, idx: -1}
	}
	return &rangeIter{
		rr:        r,
		idx:       -1,
		remaining: fdb.EffectiveRowLimit(r.options.Limit),
		iteration: 1,
		batchEnd:  0,
		scanBegin: append([]byte(nil), r.reqBegin...),
		scanEnd:   append([]byte(nil), r.reqEnd...),
	}
}

// rangeIter streams the materialized slice in streaming-mode-sized batches, registering each
// batch's read conflict as the batch is fetched. Per the RangeIterator contract, Advance() moves
// to the next element and reports whether one exists; Get() returns the current element
// idempotently (no advance).
type rangeIter struct {
	rr  *readyRangeResult
	idx int
	err error

	// batchEnd is the exclusive index of the last FETCHED row; rows at or beyond it have not
	// been fetched yet and carry no conflict.
	batchEnd  int
	remaining int
	iteration int
	exhausted bool

	// scanBegin/scanEnd are the bounds the NEXT batch would be fetched over — the client's
	// ri.begin/ri.end, which advance (forward) or retreat (reverse) past each returned batch.
	scanBegin, scanEnd []byte

	traceLog func(iteration, requested, returned int, more bool, err error)
}

func (it *rangeIter) Advance() bool {
	if it.err != nil {
		return false
	}
	it.idx++
	if it.idx < it.batchEnd {
		return true
	}
	if it.exhausted || it.remaining <= 0 {
		return it.idx < len(it.rr.kvs) && it.idx < it.batchEnd
	}
	it.fetchBatch()
	return it.idx < it.batchEnd
}

// fetchBatch models one round-trip: take up to BatchSize rows from the materialized result and
// record the conflict range that fetch would have taken.
func (it *rangeIter) fetchBatch() {
	want := fdb.BatchSize(it.rr.options.Mode, it.iteration, it.remaining)
	it.iteration++
	if want <= 0 {
		it.exhausted = true
		return
	}
	got := len(it.rr.kvs) - it.batchEnd
	if got > want {
		got = want
	}
	if it.traceLog != nil {
		it.traceLog(it.iteration-2, want, got, got == want, nil)
	}
	if got == 0 {
		it.exhausted = true
		if !it.rr.snapshot {
			// The final, empty fetch still read the remaining span and found nothing there —
			// phantom protection for the tail.
			it.addConflict(nil, false)
		}
		return
	}
	batch := it.rr.kvs[it.batchEnd : it.batchEnd+got]
	it.batchEnd += got
	it.remaining -= got

	// A batch that returned exactly what it asked for cannot know whether more rows follow, so
	// it reports more — the same rule GetRange applies to the whole read (C++ getExactRange
	// sets output.more = (data.size() == limit) unconditionally at the limit).
	more := got == want
	if !more {
		it.exhausted = true
	}
	if !it.rr.snapshot {
		it.addConflict(batch, more)
	}
	// Advance the scan bounds past this batch, as the client's iterator does: forward reads
	// resume after the last key returned, reverse reads resume before the lowest.
	last := []byte(batch[len(batch)-1].Key)
	if it.rr.options.Reverse {
		it.scanEnd = append([]byte(nil), last...)
	} else {
		it.scanBegin = keyAfter(last)
	}
	if it.remaining <= 0 {
		it.exhausted = true
	}
}

// addConflict registers the read conflict for one fetched batch over the CURRENT scan bounds,
// clamped by the same rule the whole-read path uses.
func (it *rangeIter) addConflict(batch []fdb.KeyValue, more bool) {
	cb, ce := rangeConflictExtent(it.scanBegin, it.scanEnd, batch, more, it.rr.options.Reverse)
	if bytes.Compare(cb, ce) < 0 {
		it.rr.tx.addFilteredReadConflict(cb, ce)
	}
}

func (it *rangeIter) Get() (fdb.KeyValue, error) {
	if it.err != nil {
		return fdb.KeyValue{}, it.err
	}
	if it.idx < 0 || it.idx >= it.batchEnd {
		// Zero value, no error — matches the pure-Go client / Apple binding, where Get()
		// before the first Advance() or past the end returns an empty KeyValue rather than
		// erroring. The record layer's cursors rely on this (they call Advance() then Get()).
		return fdb.KeyValue{}, nil
	}
	return it.rr.kvs[it.idx], nil
}

func (it *rangeIter) MustGet() fdb.KeyValue {
	kv, err := it.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

func (it *rangeIter) SetTraceLog(f func(iteration, requested, returned int, more bool, err error)) {
	it.traceLog = f
}

// keyAfter returns the smallest key strictly greater than k: a fresh copy of k with a trailing
// 0x00. It must copy into independent storage (never append into k's backing array), matching
// the pure-Go client's keyAfter invariant.
func keyAfter(k []byte) []byte {
	result := make([]byte, len(k)+1)
	copy(result, k)
	result[len(k)] = 0
	return result
}
