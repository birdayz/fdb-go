package client

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/internal/fdbgo/diag"
	"fdb.dev/internal/fdbwire/types"
)

// grvBatcherIndex maps GRV priority bits to a batcher array index.
// C++ uses separate batchers for BATCH, DEFAULT, and SYSTEM_IMMEDIATE.
const (
	grvBatcherBatch           = 0
	grvBatcherDefault         = 1
	grvBatcherSystemImmediate = 2
)

// grvBatcherIndex returns the array index for the given priority flags.
func grvBatcherIndex(flags uint32) int {
	switch flags & grvPriorityMask {
	case grvPriorityBatch:
		return grvBatcherBatch
	case grvPrioritySystemImmediate:
		return grvBatcherSystemImmediate
	default:
		return grvBatcherDefault
	}
}

// grvBatcher batches GetReadVersion calls with identical priority and option
// flags, matching C++ DatabaseContext::VersionBatcher and readVersionBatcher.
//
// Methods receive *database as argument — no stored back-pointer.
type grvBatcher struct {
	mu         sync.Mutex
	pending    []grvRequest
	spare      []grvRequest // a flushed batch's cleared storage, for the next batch
	inFlight   int
	batchTime  time.Duration
	priority   uint32 // fixed priority bits for this batcher
	extraFlags uint32 // immutable option bits; variants never share a queue
	variants   map[uint32]*grvBatcher

	// The batch window (RFC-259, grv_window.go). window is nil until the
	// first window opens; newWindow (nil: newWindowLocked) and now (nil:
	// time.Now) are test seams. overlapped and pendingIdle feed the serial
	// fast path: overlapped records evidence that GRVs overlap, pendingIdle
	// that the pending batch's first request found nothing in flight.
	window         batchWindow
	windowArmed    bool
	windowDeadline time.Time
	overlapped     bool
	pendingIdle    bool
	hooks          *grvBatcherTestHooks

	refreshOnce sync.Once
	// refresherStarted is a deterministic test seam (RFC-104): true once the
	// background refresher has been launched (on the first opted-in request —
	// hit OR miss, matching C++ which starts the updater inside the opt-in gate
	// before the freshness check). Lets a test assert "the refresher never
	// started for a cache-off process" without a flaky goroutine count.
	refresherStarted atomic.Bool

	// lastPanicLog rate-limits recoverFlush's diagnostic log across overlapping
	// flushes (UnixNano; atomic — a new batch can flush while an earlier flush's
	// RPC is still in flight). RFC-110.
	lastPanicLog atomic.Int64
}

type grvBatcherTestHooks struct {
	yield     func() // nil: runtime.Gosched; test seam for broadcast cadence
	newWindow func(*database) batchWindow
	now       func() time.Time
}

func (b *grvBatcher) testHooks() *grvBatcherTestHooks {
	if b.hooks == nil {
		b.hooks = &grvBatcherTestHooks{}
	}
	return b.hooks
}

type grvRequest struct {
	reply chan grvResult
	// spanContext is the requesting transaction's span. flush() folds the batch's
	// span contexts into the readVersionBatcher span (batchGRVSpanContext) to stamp
	// the GetReadVersionRequest — 1:1 with C++ addLink (NativeAPI.actor.cpp:7345).
	spanContext types.SpanContext
	// tags is the requesting transaction's tag set. flush() folds the batch's
	// tags into a per-tag occurrence count, matching C++ readVersionBatcher's
	// TransactionTagMap<uint32_t> accumulation (NativeAPI.actor.cpp:7347-7349):
	// the count is how many transactions in this batch carried the tag, which is
	// what the ratekeeper needs to attribute GRV load.
	tags []string
}

// aggregateBatchTags folds per-transaction tag sets into the per-tag occurrence
// counts the GRV request carries. Entries are sorted by tag: C++ accumulates
// into an unordered_map and the proxy re-reads it into one, so element order is
// not part of the wire contract — sorting only makes Go's output deterministic.
func aggregateBatchTags(batch []grvRequest) []types.TransactionTagCount {
	var counts map[string]uint32
	for _, r := range batch {
		for _, t := range r.tags {
			if counts == nil {
				counts = make(map[string]uint32)
			}
			counts[t]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	names := make([]string, 0, len(counts))
	for t := range counts {
		names = append(names, t)
	}
	sort.Strings(names)
	out := make([]types.TransactionTagCount, 0, len(names))
	for _, t := range names {
		out = append(out, types.TransactionTagCount{Tag: []byte(t), Count: counts[t]})
	}
	return out
}

type grvResult struct {
	version int64
	locked  bool // database-locked flag from the GRV reply (RFC-096)
	// instant is when this version's MVCC window opened, as nearly as the
	// client can know it: the PRE-RPC request time, never the reply time. It
	// is a lower bound — the proxy assigned the version at or after we asked —
	// and erring early is the safe direction, because a budget built on it
	// then expires sooner than FDB's window rather than later.
	instant time.Time
	err     error
}

var grvReplyPool = sync.Pool{New: func() any { return make(chan grvResult, 1) }}

// getReadVersion returns a read version, using the cache if fresh. The
// second return is the database-locked flag from the reply (or from the
// cache entry) — the per-transaction lock check happens at the consumption
// site (the C++ extractReadVersion analog), NOT here: one batched reply
// fans out to transactions with different lock-awareness.
func (b *grvBatcher) getReadVersion(db *database, ctx context.Context, flags uint32, span types.SpanContext, tags []string, useGrvCache, skipGrvCache bool) (int64, bool, time.Time, error) {
	// Fast path: serve from cache ONLY when the transaction opted in
	// (USE_GRV_CACHE, default off — RFC-104). C++ gate NativeAPI.actor.cpp:7503-7517:
	//   (debugChance || useGrvCache) && rkThrottlingCooledDown(cx, priority)
	// The gate does NOT special-case SYSTEM_IMMEDIATE: rkThrottlingCooledDown(IMMEDIATE)
	// returns true (:7485-7486 — IMMEDIATE bypasses ratekeeper), so an opted-in IMMEDIATE
	// transaction is served from the cache exactly like DEFAULT (#16 — IMMEDIATE priority is
	// a ratekeeper concern, orthogonal to cache freshness; the app opted into USE_GRV_CACHE).
	// The cached path FAIL-OPENS on locked (returns locked=false): C++ does not
	// lock-check the cached path (:7514-7516 returns the version with zero lock
	// inspection; lockAware appears only on the fresh fall-through, :7425). The
	// real locked check is at the consumption site (ensureReadVersion), which
	// every DEFAULT (cache-off) transaction reaches.
	//
	// NOTE (separate documented divergence): the C++ gate skips the WHOLE cache block under
	// active ratekeeper throttling (rkThrottlingCooledDown false for DEFAULT/BATCH); Go's gate
	// omits that at-gate condition, so under throttle Go starts the refresher where C++ would
	// not. tryCache still rechecks the DEFAULT/BATCH throttle on the serve path, so this only
	// affects WHEN the background updater launches, never correctness. IMMEDIATE is unaffected —
	// it is never throttled.
	if useGrvCache && !skipGrvCache {
		// Start the background refresher on the FIRST opted-in request, BEFORE the
		// freshness check — matching C++ getReadVersion (NativeAPI.actor.cpp:7507-7509),
		// which launches backgroundGrvUpdater inside the opt-in gate regardless of
		// whether the cached version is usable ("Upon our first request to use cached
		// RVs, start the background updater"). Starting it only on a cache HIT (the
		// prior Go behavior) left a cold/stale cache un-warmed: every opted-in read with
		// lag > MAX_VERSION_CACHE_LAG fell through to a real GRV and the cache never
		// caught up, defeating the opt-in entirely for sparse workloads.
		//
		// EXCEPT for SYSTEM_IMMEDIATE: Go's refresher is PER-BATCHER and issues its periodic
		// GRVs at b.priority, so starting the IMMEDIATE batcher's refresher would emit a long-lived stream
		// of ratekeeper-bypassing PRIORITY_SYSTEM_IMMEDIATE GRVs from a single opted-in read — a real
		// throttling bug. C++ instead runs ONE cx-level updater at DEFAULT priority (backgroundGrvUpdater,
		// NativeAPI.actor.cpp:1283 — its refreshTransaction uses a default-priority getReadVersion), so
		// IMMEDIATE never drives ratekeeper-bypassing background traffic. IMMEDIATE still SERVES the shared
		// cache below when it is warm (the #16 fix); the cache is kept warm by DEFAULT/BATCH refreshers and
		// every commit/GRV reply, and a pure-IMMEDIATE read-only workload simply falls through to a real
		// GRV once it ages out — never a background IMMEDIATE flood.
		if b.priority != grvPrioritySystemImmediate {
			b.refreshOnce.Do(func() {
				// Reserve the refresher's wg slot under the close barrier. If Close() has begun,
				// registerBackgroundGoroutine returns false and we skip: starting a background goroutine +
				// db.wg.Add(1) now would race Close's db.wg.Wait() at a zero counter (the topology monitor has
				// Done'd) → "WaitGroup misuse: Add concurrently with Wait" panic. refreshOnce still marks this
				// done (no retry) and tryCache below misses → the caller falls through to a real GRV, the
				// right behavior during shutdown.
				if !db.registerBackgroundGoroutine() {
					return
				}
				b.refresherStarted.Store(true)
				go b.backgroundRefresher(db) // does its own db.wg.Done() on exit
			})
		}
		if v, at, ok := db.grvCache.tryCache(b.priority); ok {
			db.metrics.countGRVCacheHit()
			return v, false, at, nil
		}
	}

	// Slow path: batch request to proxy.
	replyCh := grvReplyPool.Get().(chan grvResult)
	req := grvRequest{reply: replyCh, spanContext: span, tags: tags}

	b = b.forFlags(flags)
	switch batch := b.admit(db, req); len(batch) {
	case 0:
	case 1:
		// The serial fast path detached only this request: a goroutine would just
		// hand the reply back, so run the RPC here, bounded by this caller's ctx too.
		// Accepted divergence: C++ runs every batch to its reply (NativeAPI.actor.cpp
		// :7266-7287, :7382), so a cancelled lone caller here also skips tag throttles,
		// the minAcceptable floor and one batch-window sample.
		b.flushWithin(db, ctx, batch)
		result := <-replyCh
		grvReplyPool.Put(replyCh)
		return result.version, result.locked, result.instant, result.err
	default:
		go b.flush(db, batch)
	}

	select {
	case result := <-replyCh:
		grvReplyPool.Put(replyCh)
		return result.version, result.locked, result.instant, result.err
	case <-ctx.Done():
		return 0, false, time.Time{}, ctx.Err()
	}
}

const (
	grvMaxBatchSize       = 1000
	grvBroadcastBatchSize = 20 // ClientKnobs.cpp BROADCAST_BATCH_SIZE
	grvBatchTimeout       = 5 * time.Millisecond
)

// NativeAPI.actor.cpp indexes versionBatcher by full flags, not just priority:
// a causal-risky request must never weaken an ordinary request in the same batch.
func (b *grvBatcher) forFlags(flags uint32) *grvBatcher {
	extra := flags &^ grvPriorityMask
	if extra == b.extraFlags {
		return b
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if child := b.variants[extra]; child != nil {
		return child
	}
	if b.variants == nil {
		b.variants = make(map[uint32]*grvBatcher)
	}
	child := &grvBatcher{priority: b.priority, extraFlags: extra}
	b.variants[extra] = child
	return child
}

// admit queues req. It returns the batch to send now: the serial fast path's
// lone request or a full batch. Otherwise req waits for the batch window, which
// the first request of a batch opens, as readVersionBatcher does
// (NativeAPI.actor.cpp:7351-7356).
func (b *grvBatcher) admit(db *database, req grvRequest) []grvRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inFlight > 0 {
		b.overlapped = true
	}
	if b.pending == nil {
		b.pending, b.spare = b.spare, nil
	}
	b.pending = append(b.pending, req)
	if len(b.pending) == grvMaxBatchSize {
		return b.detachLocked()
	}
	if len(b.pending) > 1 {
		return nil // the window is already open
	}
	b.pendingIdle = b.inFlight == 0
	// Serial fast path (accepted divergence, RFC-259): skip the window while
	// the evidence says GRVs come one at a time: nothing is in flight, and no
	// batch since the last window batch of one has overlapped another. C++
	// waits for every first request.
	if b.inFlight == 0 && !b.overlapped {
		return b.detachLocked()
	}
	deadline := b.clock().Add(b.batchTime)
	if b.window == nil {
		if b.hooks != nil && b.hooks.newWindow != nil {
			b.window = b.hooks.newWindow(db)
		} else {
			b.window = b.newWindowLocked(db)
		}
	}
	// Arm what is left of the window: creating the timer may have taken time.
	if b.window == nil || !b.window.arm(max(deadline.Sub(b.clock()), 0)) {
		return b.detachLocked() // no timer (Close has begun): send now
	}
	b.windowArmed = true
	b.windowDeadline = deadline
	return nil
}

// detachLocked takes the pending batch and closes its window. A batch of one
// whose request found nothing in flight is evidence of serial use, unless a
// request arrives during its flight (admit); anything else is evidence of
// overlap.
func (b *grvBatcher) detachLocked() []grvRequest {
	batch := b.pending
	b.pending = nil
	b.inFlight++
	b.overlapped = len(batch) > 1 || !b.pendingIdle
	if b.windowArmed {
		b.windowArmed = false
		b.window.stop()
	}
	return batch
}

// expiredBatch detaches the pending batch if its window is due. A timer can
// fire for a window that a full batch already closed, or before a newer
// window's deadline; both detach nothing.
func (b *grvBatcher) expiredBatch() []grvRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.windowArmed || b.clock().Before(b.windowDeadline) {
		return nil
	}
	return b.detachLocked()
}

// recycleBatch keeps a flushed batch's storage (at most grvMaxBatchSize
// requests) for the next batch, cleared so it retains no waiter.
func (b *grvBatcher) recycleBatch(batch []grvRequest) {
	clear(batch)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spare == nil {
		b.spare = batch[:0]
	}
}

func (b *grvBatcher) finishBatch() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight--
}

func (b *grvBatcher) clock() time.Time {
	if b.hooks != nil && b.hooks.now != nil {
		return b.hooks.now()
	}
	return time.Now()
}

func (b *grvBatcher) observeLatency(elapsed time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// C++ readVersionBatcher starts at zero and imposes no lower bound.
	b.batchTime = min(time.Duration(0.1*float64(elapsed)/2+0.9*float64(b.batchTime)), grvBatchTimeout)
}

// flush sends a detached batch without holding mu, allowing overlapping RPCs.
// Callers cancel their own waits; only Close cancels a shared batch's RPC.
func (b *grvBatcher) flush(db *database, batch []grvRequest) {
	b.flushWithin(db, nil, batch)
}

// flushWithin runs the batch's RPC under ctx; Close also ends it. A nil ctx
// means db.ctx, read only once the panic backstop below is armed.
func (b *grvBatcher) flushWithin(db *database, ctx context.Context, batch []grvRequest) {
	// RFC-110: once the batch is popped, b.pending no longer references
	// it — a panic anywhere below (sendGRVRequest decode, applyGRVReply, the
	// adaptive-window math) would orphan it, and every waiter blocked on its
	// req.reply with a non-canceling ctx would hang forever. recoverFlush is the
	// fail-the-batch backstop: it delivers an error to every popped waiter and
	// keeps the process alive (the libfdb_c analog: a failed proxy GRV future
	// resolves with error, not a hung future). This is a one-shot per-batch
	// callback (not a standing loop) — there is nothing to re-arm, and the next
	// queued request arms a fresh timer; the log is rate-limited, the metric
	// counts every occurrence.
	defer b.recycleBatch(batch) // after the backstop: nothing reads batch then
	published := 0
	defer b.recoverFlush(db, batch, &published)
	retired := false
	retire := func() {
		if !retired {
			b.finishBatch()
			retired = true
		}
	}
	defer retire() // retire before the panic backstop publishes errors
	if ctx == nil {
		ctx = db.ctx
	}

	// Collect per-tx span contexts so GetReadVersionRequest carries the
	// readVersionBatcher span (C++
	// NativeAPI.actor.cpp:7345 addLink + :7385 getConsistentReadVersion child).
	spans := make([]types.SpanContext, len(batch))
	for i, r := range batch {
		spans[i] = r.spanContext
	}
	flags := b.priority | b.extraFlags

	// The generation is captured with the request time, BEFORE dispatch: a
	// reply may install its version only if no invalidation happened while it
	// was in flight. Capturing it at reply-processing time instead would leave
	// the whole round trip as a window in which InvalidateGRVCache is defeated
	// by a reply it was meant to retire.
	tok := db.grvCache.token()
	requestTime := time.Now()
	sampleStart := requestTime
	if b.hooks != nil && b.hooks.now != nil {
		sampleStart = b.clock()
	}
	version, locked, rkDefault, rkBatch, tagThrottleInfoEntries, _, attemptEpoch, err := b.sendGRVRequest(db, ctx, flags, uint32(len(batch)), batchGRVSpanContext(spans), aggregateBatchTags(batch))
	elapsed := time.Since(requestTime)

	if err == nil {
		// Unconditional, even when locked: C++ updates the shared cache
		// BEFORE the per-transaction locked throw (NativeAPI.actor.cpp:7409
		// precedes :7425). `locked` is returned to waiters below but no longer
		// rides the cache (RFC-104).
		b.applyGRVReply(db, tok.withEpoch(attemptEpoch), requestTime, version, rkDefault, rkBatch, tagThrottleInfoEntries)
		// C++ counts per-transaction in extractReadVersion (:7428-7440) — one
		// batched reply serves len(batch) transactions. Cache hits never reach
		// here (C++ parity: its cached path returns before the counters); the
		// background refresher has no waiters and adds nothing. Two accepted
		// edge divergences: waiters whose ctx expired mid-batch are still
		// counted, except a lone inline one (C++ cancels abandoned futures before its counter), and
		// under a LOCKED database non-lock-aware waiters are counted here
		// while C++'s database_locked throw (:7425-7426) precedes its
		// counters — fixing either would need per-waiter counting at the
		// consumption site, machinery a counter doesn't justify. RFC-097.
		db.metrics.countGRVBatchCompleted(b.priority, len(batch))
		// RFC-114: GRV round-trip latency (C++ GRVLatencies, NativeAPI.actor.cpp:7417).
		// Divergence (documented in RFC-114), on TWO axes: (1) count — Go samples
		// once per GRV BATCH, so Count is proxy round-trips, whereas C++ samples
		// per-transaction in extractReadVersion (Count == read-versions-completed);
		// (2) semantics — Go measures only the proxy RPC round-trip, whereas C++'s
		// latency = replyTime − startTime also folds in the per-transaction
		// batch-window queueing. Go's is the cleaner RPC-latency SLI. Cache hits
		// never reach here (they return before the flush — C++ parity, as above).
		db.metrics.observeGRVLatency(elapsed)
	}

	retire() // a waiter can start its next transaction as soon as it receives
	result := grvResult{version: version, locked: locked, instant: requestTime, err: err}
	b.publishReply(batch, result, sampleStart, &published)
}

func (b *grvBatcher) yieldReady() {
	if b.hooks != nil && b.hooks.yield != nil {
		b.hooks.yield()
	} else {
		runtime.Gosched()
	}
}

func (b *grvBatcher) publishReply(batch []grvRequest, result grvResult, started time.Time, published *int) {
	for i, req := range batch {
		req.reply <- result
		*published = i + 1
		if (i+1)%grvBroadcastBatchSize == 0 {
			b.yieldReady()
		}
	}
	if result.err == nil {
		// C++ times the last broadcast promise, then yields once before sampling
		// (NativeAPI.actor.cpp:7380–7387; genericactors.actor.h timeReply).
		b.yieldReady()
		b.observeLatency(b.clock().Sub(started))
	}
}

// recoverFlush is flush's deferred RFC-110 backstop. On a recovered panic it
// fails the unpublished suffix of the batch, counts the panic and logs it.
// Already-completed waiters keep their results. The batch reply channels are
// cap-1; the publication frontier excludes waiters already sent a result, so
// recovery cannot overwrite success or block on a second send. A waiter that
// already took its ctx.Done() branch still has room. On the normal path
// recover() is nil → no-op. Flushes are independent one-shots (not a standing loop), so this is fail-the-batch at
// request rate, not backoff-bounded.
func (b *grvBatcher) recoverFlush(db *database, batch []grvRequest, published *int) {
	r := recover()
	if r == nil {
		return
	}
	if db != nil {
		db.metrics.countRecoveredPanic(1)
	}
	b.logFlushPanic(r)
	result := grvResult{err: fmt.Errorf("fdbgo: panic in GRV flush: %v", r)}
	for _, req := range batch[*published:] {
		req.reply <- result
		*published += 1
	}
}

// logFlushPanic rate-limits recoverFlush's ERROR log to one per panicLogInterval
// (atomic CAS so overlapping flushes don't all log). The metric counts every
// panic; the log is the human breadcrumb, not the storm signal.
func (b *grvBatcher) logFlushPanic(r any) {
	now := time.Now().UnixNano()
	last := b.lastPanicLog.Load()
	if now-last < int64(panicLogInterval) || !b.lastPanicLog.CompareAndSwap(last, now) {
		return
	}
	diag.Recovered(
		"fdbgo: recovered panic in client goroutine",
		"goroutine", "grvFlush",
		"err", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()),
	)
}
