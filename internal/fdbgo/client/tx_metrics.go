package client

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/internal/fdbwire/types"
)

// regenerateSpan refreshes the transaction's trace span — a fresh span per
// transaction/attempt, matching C++ generateSpanID at cloneAndReset
// (NativeAPI.actor.cpp:3458). With an injected SPAN_PARENT the parent's traceID +
// flags are inherited and only the spanID is fresh (a child span on the caller's
// trace); otherwise a brand-new random span, sampled per the DB's sample rate
// (default unsampled). RFC-115 §4.
func (tx *Transaction) regenerateSpan() {
	if tx.db != nil && tx.db.tracer != nil {
		tx.traceMu.Lock()
		defer tx.traceMu.Unlock()
	}
	tx.regenerateSpanLocked()
}

// regenerateSpanLocked requires traceMu when export is enabled.
func (tx *Transaction) regenerateSpanLocked() {
	tx.traceClosed = false
	if parent := tx.spanParent.Load(); parent != nil {
		sc := childSpanContext(*parent)
		tx.spanContext.Store(&sc)
		tx.spanNeedsRegen.Store(false)
		return
	}
	// tx.db is nil for directly-constructed Transactions in some unit tests (which call
	// reset()/postCommitReset() without a Database) — default to the unsampled rate.
	var rate float64
	if tx.db != nil {
		rate = tx.db.tracingSampleRate
	}
	sc := newSpanContext(rate)
	tx.spanContext.Store(&sc)
	tx.spanNeedsRegen.Store(false)
}

// currentSpan loads the transaction's span context atomically, acquiring traceMu
// only when post-commit reset deferred regeneration (callers must not hold traceMu).
func (tx *Transaction) currentSpan() types.SpanContext {
	if p := tx.spanContext.Load(); p != nil {
		return *p
	}
	if tx.spanNeedsRegen.Load() {
		tx.traceMu.Lock()
		if tx.spanNeedsRegen.Load() {
			tx.regenerateSpanLocked()
		}
		tx.traceMu.Unlock()
	}
	if p := tx.spanContext.Load(); p != nil {
		return *p
	}
	return types.SpanContext{}
}

// ensureTxSpan starts one span per attempt, independently of metricStart, which
// survives retries. A pending start is published before calling application code.
func (tx *Transaction) ensureTxSpan(ctx context.Context) {
	if tx.db == nil || tx.db.tracer == nil || !isSampled(tx.currentSpan()) {
		return
	}
	if tx.readEntryError(ctx) != nil {
		return
	}
	for {
		tx.traceMu.Lock()
		if tx.txSpan != nil || tx.traceClosed {
			tx.traceMu.Unlock()
			return
		}
		if pending := tx.traceStarting; pending != nil {
			tx.traceMu.Unlock()
			if tx.hooks != nil && tx.hooks.beforeTraceWait != nil {
				tx.hooks.beforeTraceWait(ctx)
			}
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return
			}
		}
		wireSpan := tx.spanContext.Load()
		if wireSpan == nil || !isSampled(*wireSpan) {
			tx.traceMu.Unlock()
			return
		}
		pending := make(chan struct{})
		tx.traceStarting = pending
		tx.traceMu.Unlock()
		tx.startTxSpan(wireSpan, pending)
		return
	}
}

func (tx *Transaction) startTxSpan(wireSpan *types.SpanContext, pending chan struct{}) {
	// Also release queued starts if an application tracer panics.
	defer func() {
		tx.traceMu.Lock()
		tx.traceStarting = nil
		close(pending)
		tx.traceMu.Unlock()
	}()
	parent := oteltrace.ContextWithSpanContext(context.Background(), otelSpanContext(*wireSpan))
	ctx, span := tx.db.tracer.Start(parent, "Transaction", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	tx.traceMu.Lock()
	publish := !tx.traceClosed && tx.spanContext.Load() == wireSpan
	if publish {
		tx.traceCtx, tx.txSpan = ctx, span
	}
	tx.traceMu.Unlock()
	if !publish {
		endSpan(span)
	}
}

// startOpSpan starts a per-operation child span under the "Transaction" span (the C++
// NAPI:* children, e.g. NativeAPI.actor.cpp:3623). Returns nil when the tx is unsampled
// or the Transaction span hasn't started yet — callers use:
//
//	if sp := tx.startOpSpan("fdbgo.getValue"); sp != nil { defer sp.End() }
//
// The per-op otel span carries an otel-minted spanID under the shared traceID; the wire
// SpanContext is generated independently by Layer 1 (single ID authority).
func (tx *Transaction) startOpSpan(name string) oteltrace.Span {
	if tx.db == nil || tx.db.tracer == nil || !isSampled(tx.currentSpan()) {
		return nil
	}
	tx.traceMu.Lock()
	parent := tx.traceCtx
	tx.traceMu.Unlock()
	if parent == nil {
		return nil
	}
	_, span := tx.db.tracer.Start(parent, name, oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	return span
}

// detachTxSpan closes the attempt before invoking synchronous SDK processors.
// Callers holding admission or turnover must release it before ending this span.
func (tx *Transaction) detachTxSpan() oteltrace.Span {
	if tx.db == nil || tx.db.tracer == nil {
		return nil
	}
	tx.traceMu.Lock()
	span := tx.txSpan
	tx.txSpan, tx.traceCtx = nil, nil
	tx.traceClosed = true
	tx.traceMu.Unlock()
	return span
}

func endSpan(span oteltrace.Span) {
	if span != nil {
		span.End()
	}
}

// endTxSpan is also deferred by managed helpers, including read-only and error exits.
// Raw handles still require explicit commit, Reset, or Cancel.
func (tx *Transaction) endTxSpan() { endSpan(tx.detachTxSpan()) }

func (tx *Transaction) stateSetSpanParent(b []byte) error {
	parent, err := parseSpanParent(b)
	if err != nil {
		return err
	}
	tx.traceMu.Lock()
	defer tx.traceMu.Unlock()
	tx.spanParent.Store(&parent)
	if tx.spanNeedsRegen.Load() {
		tx.regenerateSpanLocked()
		return nil
	}
	sc := childSpanContext(parent)
	tx.spanContext.Store(&sc)
	return nil
}

// C++ sizeof constants for approximate size calculation. flow/Arena.h:370 wraps StringRef in
// `#pragma pack(push, 4)`, so StringRef is 12 bytes (8-byte data pointer + 4-byte length, NO tail
// padding up to 16) — NOT 16. Therefore, under that 4-byte packing:
//
//	sizeof(KeyRangeRef) = 2×StringRef          = 24   (FDBTypes.h:315 — {const KeyRef begin, end})
//	sizeof(MutationRef) = uint8 type + 2×StringRef(12) + Optional<uint32>(8) + Optional<uint16>(4)
//	                      + bool, all 4-aligned  = 44   (CommitTransaction.h:67)
//
// The earlier 48/32 assumed natural 8-byte StringRef alignment and missed the pack(4); both
// over-counted. Verified byte-exact against libfdb_c by bench TestDifferential_ApproximateSize.
const (
	sizeofMutationRef = 44
	sizeofKeyRangeRef = 24
)

func (tx *Transaction) stateGetApproximateSize() (int64, error) {
	// C++ gates getApproximateSize on the deferred error and NOTHING else
	// (ThreadSafeTransaction.cpp:715-721: checkDeferredError, then the plain
	// RYW counter getter — no resetPromise race, so a cancelled-but-unpoisoned
	// txn still returns its size; differential_cancel_test pins that parity).
	if e := tx.deferredErr.Load(); e != nil {
		return 0, e
	}
	// Iterate the buffers UNDER conflictMu, not a released snapshot. This is a
	// public method callers may poll concurrently with Commit on another
	// goroutine, and Commit's auto-reset (postCommitReset) reuses the buffer
	// backing arrays via [:0] — a released snapshot would then read memory a
	// post-reset Set overwrites. Holding the lock across the iteration prevents
	// reset/append from running concurrently with the read. The body is pure CPU
	// (no I/O), so the critical section is short.
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	var size int64
	for _, m := range tx.mutations {
		size += int64(len(m.Key)) + int64(len(m.Value)) + sizeofMutationRef
	}
	for _, r := range tx.readConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	for _, r := range tx.writeConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	// C++ charges a single-key clear's MUTATION part sizeof(KeyRangeRef), not sizeof(MutationRef)
	// (ReadYourWrites.actor.cpp:2431 — it is modeled as a range entry in the write map). The mutation
	// loop above charged every MutClearRange sizeofMutationRef; a single-key clear is shape-
	// indistinguishable from a ClearRange(k, k+\x00), so correct the overcharge here from the Clear()-
	// call count (the write-conflict half already matched: both charge sizeof(KeyRangeRef)).
	size -= int64(tx.singleKeyClearCount) * (sizeofMutationRef - sizeofKeyRangeRef)
	size += tx.rywConflictSize // the RYW conflicts, held in the read map and the write map's nodes
	return size, nil
}

// approximateCommitSize sizes the request that WILL be committed: the validated mutation
// snapshot `muts` (the exact set Commit marshals — NOT the live tx.mutations) plus the current
// conflict ranges. The commit-time size check (transaction_too_large, 2101) must use this, not
// GetApproximateSize(): under the concurrent-use contract a Set racing Commit on another
// goroutine appends to tx.mutations BEYOND `muts`, so GetApproximateSize() (which reads the live
// buffer) could fail a small commit for a mutation that is never shipped. `muts` is an
// append-only snapshot (elements never mutated in place) captured by Commit before this runs, so
// iterating it needs no lock. The conflict ranges are the vectors Commit froze with `muts` and
// ships, so the size counts exactly what is sent.
func (tx *Transaction) approximateCommitSize(muts []Mutation, readConflicts, writeConflicts []KeyRange) int64 {
	// The transaction_too_large (2101) check uses the NATIVE commit accounting: each mutation charged
	// sizeof(MutationRef) and each conflict range sizeof(KeyRangeRef) (the 44/24 under pack(4)). This
	// deliberately does NOT apply GetApproximateSize's single-key-clear adjustment: in the native
	// commit a single-key clear is a ClearRange mutation charged sizeof(MutationRef) [44], not the RYW
	// sizeof(KeyRangeRef) [24]. Verified byte-exact against libfdb_c's 2101 boundary (Set AND
	// single-key-clear workloads) by bench TestDifferential_TransactionSizeLimit — go and cgo reject at
	// the identical limit. (The old 48/32 over-counted here too, rejecting large txns slightly earlier
	// than libfdb_c; the 44/24 fix closed it.)
	var size int64
	for _, m := range muts {
		size += int64(len(m.Key)) + int64(len(m.Value)) + sizeofMutationRef
	}
	// readConflicts is the vector the commit ships (coalesced with RYW), as for writes.
	for _, r := range readConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	// writeConflicts is the caller-supplied snapshot (coalesced for a RYW commit, #28) so the 2101 gate
	// sizes exactly the write-conflict-range set that ships — not the uncoalesced live tx.writeConflicts.
	for _, r := range writeConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	return size
}

// GetLocations returns all shard location entries overlapping [begin, end).
func (tx *Transaction) GetLocations(parentCtx context.Context, begin, end []byte, limit int) ([]LocationResult, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if err := tx.readEntryError(ctx); err != nil {
		return nil, err
	}
	locs, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, limit, false, tx.tenantId, tx.currentSpan())
	for i := range locs {
		locs[i].Servers = slices.Clone(locs[i].Servers)
		locs[i].ShardBegin = bytes.Clone(locs[i].ShardBegin)
		locs[i].ShardEnd = bytes.Clone(locs[i].ShardEnd)
	}
	return locs, tx.mapReadError(ctx, err)
}

// GetAddressesForKey returns the addresses of storage servers responsible for
// the given key. Uses the location cache (queries cluster on miss).
func (tx *Transaction) GetAddressesForKey(parentCtx context.Context, key []byte) ([]string, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if err := tx.readEntryError(ctx); err != nil {
		return nil, err
	}
	// A cancelled txn returns transaction_cancelled (1025) — C++ getAddressesForKey races
	// resetPromise at op entry (ReadYourWrites.actor.cpp:1837); this path bypasses
	// ensureReadVersion, so gate explicitly (RFC-068).
	if err := tx.checkCancelled(); err != nil {
		return nil, err
	}
	// The captured signal also bounds the locate (C++ resetPromise,
	// ReadYourWrites.actor.cpp:1843-1848).
	loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId, tx.currentSpan(), false)
	if err != nil {
		// Tracked (C++ ryw->reading): getAddressesForKey is reading.add'd
		// (ReadYourWrites.actor.cpp:1849), so its failure poisons commit too.
		return nil, tx.trackReadOperation(ctx, tx.mapReadError(ctx, fmt.Errorf("locate key: %w", err)))
	}
	addrs := make([]string, len(loc.Servers))
	for i, s := range loc.Servers {
		addrs[i] = s.Address
		if s.TLS {
			// C++ getAddressesForKey returns address().toString(), which appends ":tls"
			// (NativeAPI.actor.cpp:5747 → flow/network.cpp:215). Address stays clean for dialing.
			addrs[i] += ":tls"
		}
	}
	return addrs, nil
}

func (tx *Transaction) stateGetTagThrottledDuration() float64 {
	return tx.proxyTagThrottledDuration
}
