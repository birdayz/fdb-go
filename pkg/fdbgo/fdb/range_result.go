package fdb

import (
	"bytes"
	"math"
	"unsafe"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/internal/rangecheck"
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
// for the same backend-substitution reason as RangeResult (RFC-109). Returned key and
// value bytes are owned by the caller and are never reused by the iterator.
type RangeIterator interface {
	Advance() bool
	Get() (KeyValue, error)
	MustGet() KeyValue
	SetTraceLog(fn func(iteration, requested, returned int, more bool, err error))
}

// goRangeResult is the pure-Go client's RangeResult implementation.
type goRangeResult struct {
	tx       *transaction
	options  RangeOptions
	snapshot bool
	first    *firstBatch
}

// firstBatch is batch 1 of a range read, issued by GetRange itself as Apple's
// binding does (bindings/go/src/fdb/transaction.go:295-296); every Iterator and
// GetSliceWithError of the RangeResult starts from it (range.go:173-176).
type firstBatch struct {
	rangeBounds     // the range batch 1 reads
	limit       int // the row budget batch 1 was issued with
	pending     *client.PendingRange
	err         error // a failure before any read was issued
}

// rangeBounds is what is left of a range read: the resolved keys while both
// selectors are trivial (firstGreaterOrEqual / firstGreaterThan), otherwise the
// selectors themselves, which the client resolves within each read as
// fdb_transaction_get_range does (ReadYourWrites.actor.cpp:592-813, 896-1123).
type rangeBounds struct {
	begin, end       []byte
	selectors        bool // read by beginSel/endSel instead of begin/end
	beginSel, endSel client.KeySelector
}

// advance moves the bounds past a batch whose last row is last, as Apple's
// fetchNextBatch does: FirstGreaterThan(last) as the forward begin,
// FirstGreaterOrEqual(last) as the reverse end, the other selector kept
// (bindings/go/src/fdb/range.go:229-233). last is the caller's and is copied.
// A selector range stays one: C++ reads every batch through getRangeValue.
func (b *rangeBounds) advance(last []byte, reverse bool) {
	switch {
	case !b.selectors && reverse:
		b.end = append([]byte(nil), last...)
	case !b.selectors:
		b.begin = keyAfter(last)
	case reverse:
		b.endSel = client.KeySelector{Key: bytes.Clone(last), Offset: 1}
	default:
		b.beginSel = client.KeySelector{Key: bytes.Clone(last), OrEqual: true, Offset: 1}
	}
}

func newRangeResult(tx *transaction, r Range, options RangeOptions) RangeResult {
	return startRangeResult(goRangeResult{tx: tx, options: options}, r)
}

func newSnapshotRangeResult(tx *transaction, r Range, options RangeOptions) RangeResult {
	return startRangeResult(goRangeResult{tx: tx, options: options, snapshot: true}, r)
}

func startRangeResult(rr goRangeResult, r Range) goRangeResult {
	f := &firstBatch{}
	rr.first = f
	// Capture custom range/key callbacks before validating options, in call order.
	exact, isExact := r.(ExactRange)
	if isExact {
		f.begin, f.end = cloneRangeKeys(exact)
	} else {
		r = cloneSelectorRange(r)
	}
	// EXACT without a limit (2210) and an invalid mode fail before the RYW checks and any I/O
	// (fdb_c.cpp:996-1024); the client then raises cancelled, 2004 and 2012 in C++ order.
	if rr.options.Mode == StreamingModeExact && (rr.options.Limit == 0 || rr.options.Limit == -1) {
		f.err = Error{Code: 2210}
		return rr
	}
	byteTarget, err := modeTargetBytes(rr.options.Mode, 1)
	if err != nil {
		f.err = err
		return rr
	}
	if !isExact {
		bs, es := r.FDBRangeKeySelectors()
		bsel, esel := bs.FDBKeySelector(), es.FDBKeySelector()
		if !isTrivialSelector(bsel) || !isTrivialSelector(esel) {
			// The client checks, resolves and reads in one getRange (RYW:1711-1770).
			f.selectors = true
			f.beginSel, f.endSel = clientSelector(bsel), clientSelector(esel)
			f.limit = effectiveLimit(rr.options.Limit)
			f.pending = rr.startSelectors(f.rangeBounds, f.limit, byteTarget)
			return rr
		}
		// Both selectors are byte keys: C++ rejects first on the raw selector keys.
		b, e := bsel.Key.FDBKey(), esel.Key.FDBKey()
		if err := rangecheck.Check(rr.tx.ctx, rr.tx.inner, b, e, rr.options.Limit); err != nil {
			f.err = convertError(err)
			return rr
		}
		f.begin, f.end = resolveRange(r)
	}
	f.limit = effectiveLimit(rr.options.Limit) // a limit < -1 stays as is, for the client's 2012
	if rr.snapshot {
		f.pending = rr.tx.inner.Snapshot().StartGetRange(rr.tx.ctx, f.begin, f.end, f.limit, byteTarget, rr.options.Reverse)
	} else {
		f.pending = rr.tx.inner.StartGetRange(rr.tx.ctx, f.begin, f.end, f.limit, byteTarget, rr.options.Reverse)
	}
	return rr
}

func cloneRangeKeys(r ExactRange) ([]byte, []byte) {
	begin, end := r.FDBRangeKeys()
	return bytes.Clone(begin.FDBKey()), bytes.Clone(end.FDBKey())
}

func cloneSelectorRange(r Range) Range {
	begin, end := r.FDBRangeKeySelectors()
	b, e := begin.FDBKeySelector(), end.FDBKeySelector()
	b.Key = Key(bytes.Clone(b.Key.FDBKey()))
	e.Key = Key(bytes.Clone(e.Key.FDBKey()))
	return SelectorRange{Begin: b, End: e}
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

// effectiveLimit returns the row budget to use for a range read. Apple API: Limit=0 means
// unlimited; ROW_LIMIT_UNLIMITED=-1 ALSO means unlimited (FDBTypes.h:728, isReached()==false for
// -1). A row limit < -1 is range_limits_invalid (2012), raised by the client's issueRange; it is
// never normalized here.
func effectiveLimit(limit int) int {
	if limit == 0 || limit == -1 {
		return math.MaxInt32
	}
	return limit
}

// selectorLimit is the row limit a selector read passes the client: the caller's
// unlimited 0 or -1 stays unlimited, as fdb_c passes it (fdb_c.cpp:991-994), so
// the RYW layer sizes its storage reads as C++ does; otherwise what is left.
func (rr goRangeResult) selectorLimit(remaining int) int {
	if rr.options.Limit == 0 || rr.options.Limit == -1 {
		return 0
	}
	return remaining
}

// startSelectors issues a selector read, as GetRange issues batch 1.
//
// The selector keys are the range result's own copies (cloneSelectorRange, advance),
// never modified, so the client keeps them rather than copying them again.
func (rr goRangeResult) startSelectors(b rangeBounds, limit, byteTarget int) *client.PendingRange {
	limit = rr.selectorLimit(limit)
	if rr.snapshot {
		return rr.tx.inner.Snapshot().StartGetRangeOwnedSelectors(rr.tx.ctx, b.beginSel, b.endSel, limit, byteTarget, rr.options.Reverse)
	}
	return rr.tx.inner.StartGetRangeOwnedSelectors(rr.tx.ctx, b.beginSel, b.endSel, limit, byteTarget, rr.options.Reverse)
}

// readBounds reads one batch of what is left of the range.
func (rr goRangeResult) readBounds(b rangeBounds, limit, byteTarget int) ([]client.KeyValue, bool, error) {
	if !b.selectors {
		return rr.doRange(b.begin, b.end, limit, byteTarget)
	}
	limit = rr.selectorLimit(limit) // the selector keys are owned, as in startSelectors
	if rr.snapshot {
		return rr.tx.inner.Snapshot().GetRangeOwnedSelectors(rr.tx.ctx, b.beginSel, b.endSel, limit, byteTarget, rr.options.Reverse)
	}
	return rr.tx.inner.GetRangeOwnedSelectors(rr.tx.ctx, b.beginSel, b.endSel, limit, byteTarget, rr.options.Reverse)
}

// doRange issues one range read carrying BOTH dimensions: a row budget and a per-fetch byte
// target. byteTarget is client.ByteLimitUnlimited for callers that have no streaming mode to
// derive one from (GetSliceWithError, which drains the whole range) and the mode's own target
// for the batching iterator.
func (rr goRangeResult) doRange(begin, end []byte, limit, byteTarget int) ([]client.KeyValue, bool, error) {
	if rr.snapshot {
		snap := rr.tx.inner.Snapshot()
		return snap.GetRangeWithByteTarget(rr.tx.ctx, begin, end, limit, byteTarget, rr.options.Reverse)
	}
	return rr.tx.inner.GetRangeWithByteTarget(rr.tx.ctx, begin, end, limit, byteTarget, rr.options.Reverse)
}

// GetSliceWithError returns all key-value pairs in the range as a slice: batch 1 as GetRange
// issued it (in the streaming mode), then the rest in one unlimited-byte read, as Apple does.
//
// WARNING: This eagerly loads all matching key-value pairs into memory.
// For large ranges this can cause excessive memory usage. Prefer
// Iterator() for streaming large result sets.
func (rr goRangeResult) GetSliceWithError() ([]KeyValue, error) {
	f := rr.first
	if f.err != nil {
		return nil, f.err
	}
	kvs, more, err := f.pending.Get()
	if err != nil {
		return nil, convertError(err)
	}
	// Apple reads the rest in one more pass (range.go:139-153); an unlimited byte target
	// lets the client drain it in as few requests as the server allows.
	bounds, remaining := f.rangeBounds, f.limit-len(kvs)
	for more && remaining > 0 && len(kvs) > 0 {
		bounds.advance(kvs[len(kvs)-1].Key, rr.options.Reverse)
		var rest []client.KeyValue
		rest, more, err = rr.readBounds(bounds, remaining, client.ByteLimitUnlimited)
		if err != nil {
			return nil, convertError(err)
		}
		if len(rest) == 0 {
			break
		}
		kvs = append(kvs, rest...)
		remaining -= len(rest)
	}
	return ownedKeyValues(kvs), nil
}

// KeyValue and client.KeyValue are both {[]byte, []byte}: equal sizes and
// Value offsets, and both fields convert to []byte without a conversion.
const (
	_ = unsafe.Sizeof(KeyValue{}) - unsafe.Sizeof(client.KeyValue{})
	_ = unsafe.Sizeof(client.KeyValue{}) - unsafe.Sizeof(KeyValue{})
	_ = unsafe.Offsetof(KeyValue{}.Value) - unsafe.Offsetof(client.KeyValue{}.Value)
	_ = unsafe.Offsetof(client.KeyValue{}.Value) - unsafe.Offsetof(KeyValue{}.Value)
)

var _ = func(a KeyValue, b client.KeyValue) [4][]byte { return [4][]byte{a.Key, a.Value, b.Key, b.Value} }

// ownedKeyValues reinterprets rows the caller exclusively owns as the public
// type without copying. PendingRange.Get returns a fresh copy per call and
// append into it reallocates, so neither the cache nor another result aliases kvs.
func ownedKeyValues(kvs []client.KeyValue) []KeyValue {
	if len(kvs) == 0 {
		return make([]KeyValue, 0)
	}
	return unsafe.Slice((*KeyValue)(unsafe.Pointer(unsafe.SliceData(kvs))), len(kvs))
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
	f := rr.first
	if f.err != nil {
		return &goRangeIterator{err: f.err}
	}
	return &goRangeIterator{
		rr:          rr,
		rangeBounds: f.rangeBounds,
		remaining:   f.limit,
		iteration:   1,
		index:       -1,
		first:       f.pending,
	}
}

// EffectiveRowLimit returns the row budget for a range read: Limit 0 and -1 both mean
// unlimited. Exported because the SimFDB backend (pkg/simfdb) is a third implementation of this
// interface and has to interpret a row budget identically.
//
// Its former companion BatchSize is deliberately gone rather than left unused. It returned a
// per-mode ROW page, which is how this client divided a range before the division became
// byte-driven; an exported function still handing out the old rule is an unwatched revival —
// a future caller would reintroduce row batching while every division test kept passing,
// because nothing would be calling the byte path. The batching rule now has exactly one
// exported form, ModeTargetBytes.
func EffectiveRowLimit(limit int) int { return effectiveLimit(limit) }

// goRangeIterator yields key-value pairs one at a time from a range read.
// Batch 1 is issued by GetRange; later batches are fetched by Advance, sized by the StreamingMode.
// Call Advance() before each Get(). Get() is idempotent — it returns the
// current element without advancing. Only Advance() moves forward.
type goRangeIterator struct {
	rr goRangeResult
	rangeBounds
	remaining int
	iteration int

	kvs       []KeyValue
	err       error
	pos       int
	index     int // position returned by Get(); set by Advance()
	exhausted bool
	first     *client.PendingRange // batch 1, issued by GetRange; nil once taken

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
	// Batch 1 is taken before the budget check, so its error (2012 included) surfaces.
	if ri.first == nil && (ri.exhausted || ri.remaining <= 0) {
		return false
	}

	// Fetch the next batch as a serializable read (unless the whole RangeResult is a
	// snapshot — readBounds honors rr.snapshot). EVERY batch adds its own
	// read-conflict, clamped to the extent it returned (RFC-121); since each batch reads a
	// distinct contiguous sub-range ([ri.begin, ri.end) advances below), the union of the
	// per-batch conflicts covers exactly the consumed range — matching the C client, where
	// each fdb_transaction_get_range call adds a conflict for the batch it returned. Reading
	// later batches under snapshot (no conflict) — as this did before — is UNSAFE now that
	// RFC-121 clamps the first batch's conflict to its returned prefix: the rows in later
	// batches would carry no read-conflict, so a concurrent write to one of them could let
	// this transaction commit with stale data (lost serializability).
	// libfdb_c derives a per-fetch BYTE target from the streaming mode and iteration
	// (fdb_c.cpp:1011-1029) and passes the caller's ROW limit through unchanged. The division
	// into batches then comes from the byte dimension: the target goes on the request, the
	// storage server truncates against it, and the client's soft byte limit ends the call at
	// that reply. So the row budget here is simply what is left of the user's limit — a
	// per-mode ROW batch would be a second, disagreeing divider.
	//
	// EXACT resolves to ByteLimitUnlimited (mode_bytes_array[EXACT]), so it keeps no byte
	// target, the soft byte limit cannot fire, and the read stays a single row-bounded batch.
	// That is agreement with C, not a shortfall.
	// ri.iteration is already 1-based (initialised to 1), and the C API's iteration is 1-based
	// too — bindings/java/.../RangeQuery.java:121 holds `iteration = 0` and passes `++iteration`
	// at :225, so a fresh iterator's FIRST fetch is iteration 1 and takes
	// iterationProgression[0] = 4096. Adding one here instead made the first fetch target 6144
	// and made the table's first entry unreachable by any real scan, while the trace below —
	// which logs the pre-increment value — still called that fetch iteration 1.
	batch := ri.remaining
	var kvs []client.KeyValue
	var more bool
	var err error
	if ri.first != nil {
		kvs, more, err = ri.first.Get()
		ri.first = nil
	} else {
		var byteTarget int
		if byteTarget, err = modeTargetBytes(ri.rr.options.Mode, ri.iteration); err != nil {
			ri.err = err
			return false
		}
		kvs, more, err = ri.rr.readBounds(ri.rangeBounds, batch, byteTarget)
	}
	ri.iteration++

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
	ri.advance(ri.kvs[len(ri.kvs)-1].Key, ri.rr.options.Reverse)

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

// isTrivialSelector reports whether the selector names a byte key without
// reading: firstGreaterOrEqual(k) is k and firstGreaterThan(k) is k+\x00, which is
// what removeOrEqual makes of it (FDBTypes.h:645-650). Every other selector is
// resolved by the client within the range read.
func isTrivialSelector(ks KeySelector) bool {
	return ks.Offset == 1
}

// clientSelector converts an owned (already cloned) selector. The offset is a C
// int, as Apple's binding passes C.int(Offset).
func clientSelector(ks KeySelector) client.KeySelector {
	return client.KeySelector{Key: ks.Key.FDBKey(), OrEqual: ks.OrEqual, Offset: int32(ks.Offset)}
}

// resolveSelector returns the byte key of a trivial selector, a fresh copy.
func resolveSelector(ks KeySelector) []byte {
	key := ks.Key.FDBKey()
	if ks.OrEqual {
		// FirstGreaterThan(k) = FirstGreaterOrEqual(k + \x00).
		out := make([]byte, len(key)+1)
		copy(out, key)
		return out
	}
	out := make([]byte, len(key))
	copy(out, key)
	return out
}

// resolveRange returns the byte range of a selector range whose selectors are
// both trivial.
func resolveRange(r Range) (begin, end []byte) {
	bs, es := r.FDBRangeKeySelectors()
	return resolveSelector(bs.FDBKeySelector()), resolveSelector(es.FDBKeySelector())
}
