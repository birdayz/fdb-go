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
// streaming mode's per-fetch BYTE target (fdb.ModeTargetBytes), EVERY BATCH TAKES ITS OWN READ
// CONFLICT RANGE, and a cursor
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
// EXACT without a row budget is exact_mode_without_limits(2210) on BOTH surfaces, and the two
// codes are ordered: 2012 wins for a limit below ROW_LIMIT_UNLIMITED(-1), because libfdb_c's
// EXACT gate compares the limit against ROW_LIMIT_UNLIMITED and -7 is neither unlimited nor valid.
// The order is what enforces that here — 2012 is checked first, so by the time the EXACT test runs
// the limit is already known to be >= -1.
//
// The EXACT test is therefore spelled "0 or -1" for READABILITY, not for behaviour: with the 2012
// check above it, "<= 0" is equivalent, and mutating it to "<= 0" leaves this package green. That
// is expected and not a coverage gap. The spelling IS load-bearing in the client's GetSlice, where
// 2012 arrives later (from getRangeDir) and "<= 0" really would answer 2210 for -7; matching the
// spelling here keeps the two readable side by side. The client-side direction is pinned by
// fdb:TestRangeIterator_RowLimitUnlimitedAndInvalid and bench:TestDifferential_ExactModeWithoutLimits.
//
// This used to be modelled as Iterator-only, on the argument that GetSlice cannot reach the check
// because neither real backend forwards the streaming mode down: the pure-Go client's
// GetSliceWithError did not pass Mode at all, and Apple's binding rewrites it (Exact when a limit
// is set, WantAll otherwise). The argument was from source, not measurement, and it was wrong —
// Apple's binding issues the FIRST batch's future eagerly with the caller's mode and only rewrites
// the batches it fetches itself. TestDifferential_ExactModeWithoutLimits measured libfdb_c raising
// 2210 from GetSlice; the pure-Go client was fixed to match, and this follows it.
func validateRangeLimit(options fdb.RangeOptions) error {
	if options.Limit < -1 {
		return fdb.Error{Code: 2012}
	}
	if options.Mode == fdb.StreamingModeExact && (options.Limit == 0 || options.Limit == -1) {
		return fdb.Error{Code: 2210}
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
	// fetchRange, not rangeRows: the unreadable cap is a property of the FETCH, so it applies on
	// both consumption surfaces and it narrows the window the conflict below is taken over.
	kvs, more, begin, end, err := r.tx.fetchRange(begin, end, fdb.EffectiveRowLimit(r.options.Limit), fdb.ByteLimitUnlimited, r.options.Reverse)
	if err != nil {
		return nil, err
	}
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
	// Both option errors come from validateRangeLimit, for both surfaces. The Iterator used to
	// carry its own inline EXACT check here as well; once GetSlice started raising 2210 too, that
	// line became unreachable and was removed rather than left standing as a second guard that
	// looks independent and is not.
	if err := validateRangeLimit(r.options); err != nil {
		return &rangeIter{err: err, idx: -1}
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
	// The division is BYTE-driven, exactly as it is in the client: the streaming mode supplies a
	// per-fetch byte target (fdb.ModeTargetBytes, the single definition of the rule), the row
	// budget passed down is just what is left of the caller's limit, and the truncation below
	// stands in for the storage server's. A per-mode ROW page here would put this simulator's
	// batch boundaries somewhere the real client's are not — and a batch boundary is where a
	// read-conflict range is taken and where a mid-iteration local write becomes visible, so a
	// disagreement shows up as a differential failure on read-your-writes rather than as a
	// harmless difference in round-trip count.
	// it.iteration is already 1-based, matching the C API (a fresh iterator's first fetch is
	// iteration 1 and takes the progression's first entry, 4096).
	byteTarget, terr := fdb.ModeTargetBytes(it.rr.options.Mode, it.iteration)
	if terr != nil {
		it.err = terr
		it.exhausted = true
		return false
	}
	// Mirrors the client's request-side row clamp (C++ transformRangeLimits,
	// NativeAPI.actor.cpp:4223: req.limit = min(REPLY_BYTE_LIMIT, limits.rows)).
	want := it.remaining
	if want > replyByteLimit {
		want = replyByteLimit
	}
	it.iteration++
	if want <= 0 {
		it.exhausted = true
		return false
	}
	// The cap is recomputed per batch, as it is in the client — doRangeWithLimit funnels every
	// batch through rywCache.getRange, which caps the window it was handed. So a versionstamped
	// write issued between two batches truncates the second one and not the first.
	batch, more, capBegin, capEnd, err := it.rr.tx.fetchRange(
		it.scanBegin, it.scanEnd, want, byteTarget, it.rr.options.Reverse)
	if err != nil {
		// Surfaced through it.err, the same channel a resolve-time error uses: Advance() reports
		// no element and Get() returns the error. The client does the same — goRangeIterator's
		// fetch stores convertError(err) in ri.err and returns false (fdb/range_result.go:316-319).
		it.err = err
		it.exhausted = true
		return false
	}
	// capBegin/capEnd are used for THIS batch's conflict only; they must NOT be written back
	// into scanBegin/scanEnd. The client keeps ri.begin/ri.end at the REQUESTED bounds and lets
	// rywCache.getRange re-cap each call (fdb/range_result.go:309 passes ri.begin/ri.end
	// unmodified). Persisting the cap here would move the unreadable key onto the next window's
	// exclusive end, where unreadableScanCap no longer sees it — the following batch would find
	// no cap, return no rows and report clean exhaustion, silently swallowing the 1036 the scan
	// is supposed to raise.
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
			it.addConflict(capBegin, capEnd, nil, false)
		}
		return false
	}
	it.batch = batch
	it.idx = 0
	it.pos = 1
	it.remaining -= len(batch)
	if !it.rr.snapshot {
		it.addConflict(capBegin, capEnd, batch, more)
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

// replyByteLimit is CLIENT_KNOBS->REPLY_BYTE_LIMIT (fdbclient/ClientKnobs.cpp:66), the flat
// ceiling on any single reply. It bounds both the byte target and, as C++ transformRangeLimits
// does, the row count a single request may ask for.
const replyByteLimit = 80000

// serverRowOverheadBytes is the per-row overhead the storage server charges against a reply's
// byte budget, on top of key+value.
//
// It is MEASURED, not derived: the server's accounting is not the client's, which is the whole
// reason the byte target has to go on the REQUEST rather than being modelled client-side. The
// value 24, together with the "accumulate until the budget is REACHED, including the row that
// crosses it" rule below, reproduces every cross-client division measured against libfdb_c —
// four row shapes and ten targets:
//
//	12B key + 200B value : 256->2,  1000->5,  4096->18, 8192->35
//	13B key + 200B value : 256->2,  1000->5,  4096->18
//	17B key +   1B value : 256->7,  1000->24
//	20B key +   7B value : 80000->1569
//
// Those measurements live in libfdbc:TestLibFDBC_RangeBatchDivision,
// TestLibFDBC_BoundedRangeDivisionDifferential and TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget,
// and fdb:TestRangeIterator_DrainsPastIteratorSaturation. If this constant is ever wrong, those
// differentials are what will say so — this simulator agreeing with itself proves nothing.
const serverRowOverheadBytes = 24

// serverByteCut returns how many rows of batch a storage server would put in a reply limited to
// targetBytes. targetBytes of fdb.ByteLimitUnlimited means no truncation.
func serverByteCut(batch []fdb.KeyValue, targetBytes int) int {
	if targetBytes == fdb.ByteLimitUnlimited || len(batch) == 0 {
		return len(batch)
	}
	sum := 0
	for i, kv := range batch {
		sum += len(kv.Key) + len(kv.Value) + serverRowOverheadBytes
		// >=, not >: the row that reaches the budget is INCLUDED. A floor rule here would cut
		// one row early at every target that is not an exact multiple of the row size.
		if sum >= targetBytes {
			return i + 1
		}
	}
	return len(batch)
}

// addConflict registers the read conflict for one fetched batch over the window that batch
// ACTUALLY read — the scan bounds as the unreadable cap narrowed them, not the requested ones —
// clamped by the same rule the whole-read path uses.
func (it *rangeIter) addConflict(begin, end []byte, batch []fdb.KeyValue, more bool) {
	cb, ce := rangeConflictExtent(begin, end, batch, more, it.rr.options.Reverse)
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
