package simfdb

import (
	"bytes"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// simRangeResult is SimFDB's RangeResult: a LAZY handle over a range request, holding nothing
// but the request itself — the same shape as the pure-Go client's goRangeResult
// (fdb/range_result.go:32-37), which stores only {tx, r, options, snapshot} and does every bit
// of its work at consumption.
//
// The laziness is load-bearing twice over, and both halves are about a write issued BETWEEN the
// GetRange() call and the consumption of its result — the record layer's scan-and-update shape:
//
//   - ROWS. Each fetch resolves against the read-your-writes view AS IT IS AT FETCH TIME. A
//     write landing mid-scan is therefore visible in every batch not yet fetched, and invisible
//     in the ones already returned. Materializing the whole result at GetRange() time answers
//     from a stale view and hides the write entirely.
//   - CONFLICTS. Every batch takes its own read conflict range, clamped to what that batch
//     returned (fdb/range_result.go:245-283) and filtered through the write map at consumption
//     (addFilteredReadConflict). The filter SUBTRACTS spans the transaction's own buffer
//     answered — which is only sound if the rows were actually resolved from that buffer. An
//     eagerly-materialized result returns the STORED value while the filter subtracts the span
//     as locally-satisfied: the read depended on storage and records no conflict for it, so a
//     concurrent writer of that key does not abort the reader. That is a lost update certified
//     by the simulator, which is worse than no simulator.
//
// The batching itself is modelled, not incidental: the real backend fetches pages sized by the
// streaming mode (fdb.BatchSize), EVERY BATCH TAKES ITS OWN READ CONFLICT RANGE, and a cursor
// abandoned after one page has therefore conflicted over ONE PAGE. Registering the full extent
// at GetRange() call time over-conflicts every early-abandoned scan — and the record layer's
// cursors abandon constantly, at every continuation boundary. Batch boundaries are also where a
// continuation lands, so a sim that batched differently would page differently from the client
// it stands in for.
//
// GetSliceWithError is the exception and matches the client's: it asks for the whole budget in
// one fetch (goRangeResult.GetSliceWithError calls doRangeWithLimit once), so it takes one
// conflict over the full extent.
type simRangeResult struct {
	tx       *simTxn
	r        fdb.Range
	options  fdb.RangeOptions
	snapshot bool
}

// validateRangeLimit is the row-limit check the client applies on BOTH consumption paths. A row
// limit below ROW_LIMIT_UNLIMITED(-1) is range_limits_invalid(2012): GetSlice reaches it through
// getRangeDir (client/transaction.go:1265-1268, which every doRangeWithLimit call funnels
// through) and Iterator() performs it inline (fdb/range_result.go:122-125). Without it here a
// Limit of -2 silently returned every row — a wrong answer no real backend gives, on the path
// most callers use.
//
// The Mode check (EXACT without a limit is exact_mode_without_limits, 2210) is deliberately NOT
// here: it belongs to Iterator alone. GetSlice ignores the streaming mode in both real backends
// — the pure-Go client's GetSliceWithError never passes Mode to getRangeDir, and Apple's binding
// OVERRIDES it (Exact when a limit is set, WantAll otherwise) before the C layer can validate it
// — so a sim that raised 2210 from GetSlice would be inventing an error neither backend returns.
func validateRangeLimit(options fdb.RangeOptions) error {
	if options.Limit < -1 {
		return fdb.Error{Code: 2012}
	}
	return nil
}

func (r *simRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	// Bound resolution first, validation second — the client's order: GetSliceWithError calls
	// resolveRange (which may issue conflict-taking GetKeys) before doRangeWithLimit reaches
	// getRangeDir's limit check.
	begin, end, err := r.tx.resolveRangeForRead(r.r, r.snapshot)
	if err != nil {
		return nil, err
	}
	if err := validateRangeLimit(r.options); err != nil {
		return nil, err
	}
	kvs, more := r.tx.rangeRows(begin, end, fdb.EffectiveRowLimit(r.options.Limit), r.options.Reverse)
	if !r.snapshot {
		cb, ce := rangeConflictExtent(begin, end, kvs, more, r.options.Reverse)
		if bytes.Compare(cb, ce) < 0 {
			r.tx.addFilteredReadConflict(cb, ce)
		}
	}
	return kvs, nil
}

func (r *simRangeResult) GetSliceOrPanic() []fdb.KeyValue {
	kvs, err := r.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return kvs
}

func (r *simRangeResult) Iterator() fdb.RangeIterator {
	begin, end, err := r.tx.resolveRangeForRead(r.r, r.snapshot)
	if err != nil {
		return &rangeIter{err: err, idx: -1}
	}
	if err := validateRangeLimit(r.options); err != nil {
		return &rangeIter{err: err, idx: -1}
	}
	// EXACT without a row limit is exact_mode_without_limits(2210) (libfdb_c
	// validate_and_update_parameters); only the explicit-Exact Iterator path can reach it. See
	// validateRangeLimit for why GetSlice does not.
	if r.options.Mode == fdb.StreamingModeExact && r.options.Limit <= 0 {
		return &rangeIter{err: fdb.Error{Code: 2210}, idx: -1}
	}
	return &rangeIter{
		rr:        r,
		idx:       -1,
		remaining: fdb.EffectiveRowLimit(r.options.Limit),
		iteration: 1,
		scanBegin: begin,
		scanEnd:   end,
	}
}

// rangeIter streams a range in streaming-mode-sized batches, RESOLVING each batch's rows when
// that batch is fetched and registering its read conflict then. Field for field it is the
// client's goRangeIterator (fdb/range_result.go:196-217): batch/idx/pos are its kvs/index/pos,
// scanBegin/scanEnd its begin/end.
//
// Per the RangeIterator contract, Advance() moves to the next element and reports whether one
// exists; Get() returns the current element idempotently (no advance).
type rangeIter struct {
	rr  *simRangeResult
	err error

	// batch is the rows the most recent fetch returned; idx is the element Get() hands back
	// and pos the next one Advance() will. Rows beyond the current batch have not been fetched
	// and carry no conflict.
	batch []fdb.KeyValue
	idx   int
	pos   int

	remaining int
	iteration int
	exhausted bool

	// scanBegin/scanEnd are the bounds the NEXT batch is fetched over — the client's
	// ri.begin/ri.end, which advance (forward) or retreat (reverse) past each returned batch.
	scanBegin, scanEnd []byte

	traceLog func(iteration, requested, returned int, more bool, err error)
}

func (it *rangeIter) Advance() bool {
	if it.err != nil {
		return false
	}
	if it.pos < len(it.batch) {
		it.idx = it.pos
		it.pos++
		return true
	}
	if it.exhausted || it.remaining <= 0 {
		return false
	}
	return it.fetchBatch()
}

// fetchBatch models one round-trip: resolve up to a streaming-mode-sized page from the store
// merged with the transaction's write buffer AS OF NOW, and record the conflict range that
// fetch would have taken.
func (it *rangeIter) fetchBatch() bool {
	want := fdb.BatchSize(it.rr.options.Mode, it.iteration, it.remaining)
	it.iteration++
	if want <= 0 {
		it.exhausted = true
		return false
	}
	batch, more := it.rr.tx.rangeRows(it.scanBegin, it.scanEnd, want, it.rr.options.Reverse)
	if it.traceLog != nil {
		// iteration-1 is the 1-based number of the iteration this batch WAS, matching the
		// client's own trace call (fdb/range_result.go:255-257).
		it.traceLog(it.iteration-1, want, len(batch), more, nil)
	}
	if len(batch) == 0 {
		it.exhausted = true
		if !it.rr.snapshot {
			// The final, empty fetch still read the remaining span and found nothing there —
			// phantom protection for the tail. The client takes it too: getRangeDir registers
			// the clamped extent on every call, and an empty result clamps to the full
			// [begin,end) it scanned. The previous batch stays in it.batch, exactly as the
			// client leaves ri.kvs untouched on an empty fetch.
			it.addConflict(nil, false)
		}
		return false
	}
	it.batch = batch
	it.idx = 0
	it.pos = 1
	it.remaining -= len(batch)
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
	if !more || it.remaining <= 0 {
		it.exhausted = true
	}
	return true
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
	if it.idx < 0 || it.idx >= len(it.batch) {
		// Zero value, no error — matches the pure-Go client, where Get() before the first
		// Advance() returns an empty KeyValue rather than erroring. The record layer's cursors
		// rely on this (they call Advance() then Get()).
		return fdb.KeyValue{}, nil
	}
	return it.batch[it.idx], nil
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
