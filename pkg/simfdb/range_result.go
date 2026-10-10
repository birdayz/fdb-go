package simfdb

import (
	"bytes"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// simRangeResult is SimFDB's RangeResult. Like the pure-Go client's goRangeResult and Apple's
// binding (bindings/go/src/fdb/transaction.go:295-296), GetRange fetches BATCH 1 at the call;
// every later batch is fetched when an iterator reaches it.
//
// Both halves of that split are observable through a write issued between the GetRange() call
// and the consumption of its result — the record layer's scan-and-update shape:
//
//   - ROWS. Each fetch resolves against the read-your-writes view AS IT IS WHEN THE FETCH IS
//     ISSUED (C++ ReadYourWrites.actor.cpp:382). Batch 1 ignores a write made after GetRange();
//     a later batch sees it.
//   - CONFLICTS. Every batch takes its own read conflict range, clamped to what that batch
//     returned and filtered through the write map at that batch's issue (addFilteredReadConflict;
//     C++ :388). Batch 1's conflict counts even if the result is never consumed: commit waits
//     for every issued read (:1359-1360).
//
// The batching itself is modelled, not incidental: the real backend fetches pages sized by the
// streaming mode's per-fetch BYTE target (fdb.ModeTargetBytes), EVERY BATCH TAKES ITS OWN READ
// CONFLICT RANGE, and a cursor abandoned after one page has therefore conflicted over ONE PAGE.
// Batch boundaries are also where a continuation lands, so a sim that batched differently would
// page differently from the client it stands in for.
//
// GetSliceWithError matches the client's: batch 1, then the rest in one unlimited-byte fetch,
// each taking its own conflict.
type simRangeResult struct {
	tx       *simTxn
	r        fdb.Range
	options  fdb.RangeOptions
	snapshot bool
	first    *simFirstBatch
}

// simFirstBatch is batch 1, fetched by GetRange.
type simFirstBatch struct {
	begin, end []byte // the resolved range
	limit      int
	batch      []fdb.KeyValue
	more       bool
	err        error
}

func (r *simRangeResult) issueFirst() {
	f := &simFirstBatch{}
	r.first = f
	begin, end, err := r.tx.resolveRangeForRead(r.r, r.snapshot)
	if err != nil {
		f.err = err
		return
	}
	f.begin, f.end = begin, end
	if err := validateRangeLimit(r.options); err != nil {
		f.err = err
		return
	}
	byteTarget, err := fdb.ModeTargetBytes(r.options.Mode, 1)
	if err != nil {
		f.err = err
		return
	}
	f.limit = fdb.EffectiveRowLimit(r.options.Limit)
	// The client's request-side row clamp (C++ transformRangeLimits, NativeAPI.actor.cpp:4223).
	want := min(f.limit, replyByteLimit)
	batch, more, capBegin, capEnd, err := r.tx.fetchRange(begin, end, want, byteTarget, r.options.Reverse)
	if err != nil {
		f.err = err
		return
	}
	f.batch, f.more = batch, more
	if !r.snapshot {
		r.addConflict(capBegin, capEnd, batch, more)
	}
}

// take returns a consumer's own copy of batch 1, as each Get of a range future copies. A
// Cancel since GetRange fails it: the storage reply cannot have beaten a local cancel, and C++
// fails the in-flight read through resetPromise (ReadYourWrites.actor.cpp:391).
func (f *simFirstBatch) take(tx *simTxn) ([]fdb.KeyValue, error) {
	if tx.cancelled.Load() {
		return nil, fdb.Error{Code: 1025}
	}
	return f.rows(), nil
}

func (f *simFirstBatch) rows() []fdb.KeyValue {
	out := make([]fdb.KeyValue, len(f.batch))
	for i, kv := range f.batch {
		out[i] = fdb.KeyValue{Key: bytes.Clone(kv.Key), Value: bytes.Clone(kv.Value)}
	}
	return out
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
	f := r.first
	if f.err != nil {
		return nil, f.err
	}
	kvs, err := f.take(r.tx)
	if err != nil {
		return nil, err
	}
	more := f.more
	begin, end, remaining := f.begin, f.end, f.limit-len(kvs)
	for more && remaining > 0 && len(kvs) > 0 {
		last := []byte(kvs[len(kvs)-1].Key)
		if r.options.Reverse {
			end = bytes.Clone(last)
		} else {
			begin = keyAfter(last)
		}
		// fetchRange, not rangeRows: the unreadable cap is a property of the FETCH, so it
		// narrows the window the conflict below is taken over.
		rest, restMore, capBegin, capEnd, err := r.tx.fetchRange(begin, end, remaining, fdb.ByteLimitUnlimited, r.options.Reverse)
		if err != nil {
			return nil, err
		}
		if !r.snapshot {
			r.addConflict(capBegin, capEnd, rest, restMore)
		}
		if len(rest) == 0 {
			break
		}
		kvs = append(kvs, rest...)
		remaining -= len(rest)
		more = restMore
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
	f := r.first
	if f.err != nil {
		return &rangeIter{err: f.err, idx: -1}
	}
	return &rangeIter{
		rr:        r,
		idx:       -1,
		remaining: f.limit,
		iteration: 1,
		scanBegin: f.begin,
		scanEnd:   f.end,
		first:     f,
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
	// first is batch 1, fetched by GetRange; nil once this iterator has taken it.
	first *simFirstBatch

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
	if f := it.first; f != nil {
		it.first = nil
		it.iteration++
		if it.traceLog != nil {
			it.traceLog(1, it.remaining, len(f.batch), f.more, nil)
		}
		rows, err := f.take(it.rr.tx)
		if err != nil {
			it.err = err
			it.exhausted = true
			return false
		}
		return it.takeBatch(rows, f.more)
	}
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
	if !it.rr.snapshot {
		it.addConflict(capBegin, capEnd, batch, more)
	}
	return it.takeBatch(batch, more)
}

// takeBatch makes a fetched, non-empty batch current and moves the scan bounds past it.
func (it *rangeIter) takeBatch(batch []fdb.KeyValue, more bool) bool {
	if len(batch) == 0 {
		it.exhausted = true
		return false
	}
	it.batch = batch
	it.idx = 0
	it.pos = 1
	it.remaining -= len(batch)
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

// serverRowOverheadBytes is an EFFECTIVE, END-TO-END per-row overhead: the constant that, in
// this simulator, reproduces where a real cluster actually cuts a reply. It is deliberately NOT
// described as "what the storage server charges", because it is not equal to that, and the
// difference is the kind that gets "fixed" by the next reader who opens the C++ source.
//
// The two named per-row charges in 7.3.77 are both something else:
//
//   - The STORAGE SERVER charges key+value+32 — `*pLimitBytes -= sizeof(KeyValueRef) +
//     i->expectedSize()` (fdbserver/storageserver.actor.cpp:4262, and the same shape at the
//     merge sites and in the SQLite and RocksDB stores; Redwood charges key+value only).
//   - The CLIENT's own decrement nets key+value+8, because VectorRef<T>::expectedSize()
//     ALREADY includes sizeof(T)*n (flow/include/flow/Arena.h:1136-1140), so the
//     `- (8 - sizeof(KeyValueRef)) * data.size()` term in GetRangeLimits::decrement cancels 24
//     of that 32.
//
// 24 matches NEITHER. Reconciling it to 32 would be a regression: the pure server-charge model
// predicts 17 rows at LARGE's 4096-byte target for 200-byte rows where 18 is what libfdb_c and
// a real cluster produce. So this constant stands on measurement, not derivation — which is
// also why the real client never needs it. The client sends the target and lets the real server
// apply whatever its true accounting is; only a simulator with no storage server has to guess,
// and the differentials are what keep the guess honest.
//
// The value 24, together with the "accumulate until the budget is REACHED, including the row
// that crosses it" rule below, reproduces every cross-client division measured against
// libfdb_c — four row shapes and ten targets:
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
	it.rr.addConflict(begin, end, batch, more)
}

func (r *simRangeResult) addConflict(begin, end []byte, batch []fdb.KeyValue, more bool) {
	cb, ce := rangeConflictExtent(begin, end, batch, more, r.options.Reverse)
	if bytes.Compare(cb, ce) < 0 {
		r.tx.addFilteredReadConflict(cb, ce)
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
