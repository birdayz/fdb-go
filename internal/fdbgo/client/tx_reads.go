package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
)

// Snapshot returns a snapshot view of this transaction.
// Snapshot reads do not add read conflict ranges, so they don't cause
// conflicts with concurrent writers. Same read version, same connection.
func (tx *Transaction) Snapshot() Snapshot {
	return Snapshot{tx: tx}
}

// Snapshot wraps a Transaction for conflict-free reads.
// All reads go through the same transaction (same read version, same
// connection pool) but do not add read conflict ranges.
type Snapshot struct {
	tx *Transaction
}

// Get reads a key without adding a read conflict range.
// Snapshot reads retain the storage cache; pending writes are included unless
// snapshot RYW is net-disabled (snapshotRYWDisableCount > 0).
func (s Snapshot) Get(ctx context.Context, key []byte) ([]byte, error) {
	return s.tx.issueGet(ctx, key, true, false).wait()
}

// StartGet is the snapshot form of Transaction.StartGet.
func (s Snapshot) StartGet(ctx context.Context, key []byte) *PendingValue {
	return s.tx.startGet(ctx, key, true)
}

// GetKey resolves a key selector without adding a read conflict range.
// Snapshot reads go through the snapshot cache, and — by default — SEE the txn's own
// pending writes (matching Snapshot.Get/GetRange and libfdb_c, where snapshot RYW is
// enabled unless net-disabled via SetSnapshotRYWDisable). When net-disabled
// (snapshotRYWDisableCount > 0) the write map is bypassed (snapshot cache only).
func (s Snapshot) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	return s.tx.issueKey(ctx, KeySelector{Key: selectorKey, OrEqual: orEqual, Offset: offset}, true).run()
}

// GetRange reads a range without adding a read conflict range.
// Snapshot reads retain the storage cache; pending writes are included unless
// snapshot RYW is net-disabled (snapshotRYWDisableCount > 0).
func (s Snapshot) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	return s.getRangeDir(ctx, begin, end, limit, ByteLimitUnlimited, false)
}

// GetRangeWithByteTarget is GetRange carrying a per-fetch BYTE target, the dimension libfdb_c
// derives from the streaming mode (fdb_c.cpp:1002/1006). ByteLimitUnlimited means "no target",
// which is what the plain GetRange passes and what EXACT resolves to.
func (s Snapshot) GetRangeWithByteTarget(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return s.getRangeDir(ctx, begin, end, limit, byteTarget, reverse)
}

func (s Snapshot) getRangeDir(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return s.tx.issueRange(ctx, begin, end, limit, byteTarget, reverse, true).run(true)
}

// GetRangeReverse reads a range in reverse without adding a read conflict range.
// Snapshot reads retain the storage cache; pending writes are included unless
// snapshot RYW is net-disabled (snapshotRYWDisableCount > 0).
func (s Snapshot) GetRangeReverse(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	return s.getRangeDir(ctx, begin, end, limit, ByteLimitUnlimited, true)
}

// GetReadVersion returns the read version for this transaction via its snapshot view.
func (s Snapshot) GetReadVersion(ctx context.Context) (int64, error) {
	return s.tx.GetReadVersion(ctx)
}

// Local RYW reads still observe deferred errors and reset/timeout, but do not
// enter NativeAPI's GRV path unless their answer needs database state.
func (tx *Transaction) checkReadState(ctx context.Context) error {
	if err := tx.readEntryError(ctx); err != nil {
		return err
	}
	if err := tx.checkCancelled(); err != nil {
		return err
	}
	if txState(tx.state.Load()) != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	return nil
}

func (tx *Transaction) ensureReadVersion(ctx context.Context) error {
	return tx.ensureReadVersionFlags(ctx, 0)
}

// ensureReadVersionFlags adds extraFlags to the read-version GRV, if it issues one.
func (tx *Transaction) ensureReadVersionFlags(ctx context.Context, extraFlags uint32) error {
	ctx, release := tx.opContext(ctx)
	defer release()
	if _, err := tx.readVersionForOperation(ctx, extraFlags, true); err != nil {
		return err
	}
	return tx.readEntryError(ctx)
}

// operationReadVersion is ensureReadVersion returning the version: C++ getRange and
// getKey start the transaction and validate its version once (NativeAPI.actor.cpp:4617-4618),
// and every request the operation sends carries that version.
func (tx *Transaction) operationReadVersion(ctx context.Context) (int64, error) {
	ctx, release := tx.opContext(ctx)
	defer release()
	readVersion, err := tx.readVersionForOperation(ctx, 0, true)
	if err != nil {
		return 0, err
	}
	return readVersion, tx.readEntryError(ctx)
}

// readVersionForOperation returns the GRV captured by this operation, not a
// later read of the mutable handle. C++ RYWImpl::getReadVersion returns the
// value from its completed future even if the handle is subsequently reset.
func (tx *Transaction) readVersionForOperation(parentCtx context.Context, extraFlags uint32, validateExplicit bool) (int64, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if err := tx.checkReadState(ctx); err != nil {
		return 0, err
	}
	if tx.hooks != nil && tx.hooks.beforeReadVersionLock != nil {
		tx.hooks.beforeReadVersionLock()
	}
	rv, userSet, err := tx.cachedOrFetchReadVersion(ctx, extraFlags)
	if err != nil {
		return 0, err
	}
	if validateExplicit && userSet {
		if err := validateExplicitVersion(ctx, tx.db, rv, tx.readVersionRequest); err != nil {
			return 0, err
		}
	}
	if tx.hooks != nil && tx.hooks.afterReadVersion != nil {
		tx.hooks.afterReadVersion()
	}
	return rv, nil
}

// Get reads a single key. Returns nil if the key doesn't exist.
func (tx *Transaction) Get(ctx context.Context, key []byte) ([]byte, error) {
	return tx.issueGet(ctx, key, false, false).wait()
}

// StartGet issues a Get in the call, as fdb_transaction_get does (ThreadSafeTransaction.cpp:426-434);
// Commit waits for its server read whether or not Get is called (RYW:1690-1691, 1359-1360).
func (tx *Transaction) StartGet(ctx context.Context, key []byte) *PendingValue {
	return tx.startGet(ctx, key, false)
}

// PendingValue is a Get issued in the call; Get waits for its value.
type PendingValue struct {
	done  chan struct{} // closed once value and err are set
	value []byte
	err   error
}

// Ready reports a value already decided: Get does not wait.
func (p *PendingValue) Ready() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Get returns the value; every call returns the same result.
func (p *PendingValue) Get() ([]byte, error) {
	<-p.done
	return p.value, p.err
}

// startGet runs the server read on its own goroutine, as C++ runs the read's actor unawaited,
// so its lease and Commit's barrier never wait for the caller.
func (tx *Transaction) startGet(parent context.Context, key []byte, snapshot bool) *PendingValue {
	g := tx.issueGet(parent, key, snapshot, true)
	if g.finish == nil {
		return &PendingValue{done: closedDone, value: g.value, err: g.err}
	}
	p := &PendingValue{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer g.release()
		// RFC-110: a panic fails the read, recorded before release lets Commit past its wait.
		if tx.rangeReadBackstop().run(func() { p.value, p.err = g.finish() }) {
			p.value, p.err = nil, tx.trackReadOperation(g.ctx, &wire.FDBError{Code: ErrInternalError})
		}
	}()
	return p
}

// issuedGet is a Get past its issue: its value, or the server read and its release.
type issuedGet struct {
	value   []byte
	err     error
	ctx     context.Context
	finish  func() ([]byte, error) // nil when the value was decided at issue
	release func()
}

func (g issuedGet) wait() ([]byte, error) {
	if g.finish == nil {
		return g.value, g.err
	}
	defer g.release()
	return g.finish()
}

// issueGet is RYW get (ReadYourWrites.actor.cpp:1631-1693) split at its wait; a server read
// counts in Commit's barrier (`reading`, :1690-1691). copyKey keeps the key for a later finish.
func (tx *Transaction) issueGet(parent context.Context, key []byte, snapshot, copyKey bool) issuedGet {
	if tx.routesSpecialKey(key) { // RYW:1634-1638
		v, err := tx.getSpecialKey(key)
		return issuedGet{value: v, err: err}
	}
	ctx, cancel, counted := tx.opContextFor(parent, true)
	handedOff := false
	defer func() { // also on a panic, which would otherwise leak a counted lease
		if !handedOff {
			cancel()
		}
	}()
	if err := tx.checkReadState(ctx); err != nil {
		return issuedGet{err: tx.trackReadOperation(ctx, err)}
	}
	// C++ RYW::getValue: if (key >= getMaxReadKey() && key != metadataVersionKey)
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return issuedGet{err: &wire.FDBError{Code: 2004}} // key_outside_legal_range
	}
	tx.hadRead.Store(true)
	// Special keys (\xff\xff prefix) don't add read conflicts — C++ resolves
	// them internally without going through the resolver conflict map. The conflict
	// is routed through the RYW filter so a read served by a local independent write
	// adds no read-conflict, matching libfdb_c (RFC-121 D2).
	if !snapshot && !isSpecialKey(key) {
		tx.addReadConflictForKeyRYW(key)
	}
	if !tx.rywDisabled {
		// Disabling snapshot RYW ignores writes, not the snapshot cache. Only
		// disabling all RYW selects NativeAPI read-through (C++ RYW :400-403).
		var r pointRead
		if snapshot && tx.snapshotRYWDisableCount > 0 {
			r = tx.ryw.issueGetSnapshot(key)
		} else {
			r = tx.ryw.issueGet(key)
		}
		if r.known {
			return issuedGet{value: bytes.Clone(r.value), err: tx.trackReadOperation(ctx, r.err)}
		}
		if copyKey {
			r.key = bytes.Clone(r.key)
		}
		release := tx.enterIssuedReadRelease(ctx, cancel, counted)
		handedOff = true
		return tx.finishPointRead(ctx, release, r)
	}
	if copyKey {
		key = bytes.Clone(key)
	}
	release := tx.enterIssuedReadRelease(ctx, cancel, counted)
	handedOff = true
	return tx.finishGetValue(ctx, release, key)
}

// enterIssuedReadRelease counts a server read in Commit's barrier and returns what ends it: a
// borrowed lease does not count, so the read enters on its own, as range reads do at issue.
func (tx *Transaction) enterIssuedReadRelease(ctx context.Context, cancel context.CancelFunc, counted bool) func() {
	if counted {
		return cancel
	}
	inc := tx.enterIssuedRead(ctx)
	return func() {
		tx.leaveIssuedRead(inc)
		cancel()
	}
}

// finishGetValue is a Get without RYW past its issue. The closures live in their
// own functions so that a Get answered at issue moves nothing to the heap.
func (tx *Transaction) finishGetValue(ctx context.Context, release func(), key []byte) issuedGet {
	return issuedGet{ctx: ctx, release: release, finish: func() ([]byte, error) {
		v, err := tx.pointServerRead()(ctx, key)
		return v, tx.trackReadOperation(ctx, err)
	}}
}

// finishPointRead is a RYW Get past its issue (see finishGetValue).
func (tx *Transaction) finishPointRead(ctx context.Context, release func(), r pointRead) issuedGet {
	return issuedGet{ctx: ctx, release: release, finish: func() ([]byte, error) {
		v, err := r.finish(ctx, tx.pointServerRead())
		return bytes.Clone(v), tx.trackReadOperation(ctx, err)
	}}
}

// pointServerRead is NativeAPI getValue, or its test seam.
func (tx *Transaction) pointServerRead() func(context.Context, []byte) ([]byte, error) {
	if tx.hooks != nil && tx.hooks.nativeGet != nil {
		return tx.hooks.nativeGet
	}
	return tx.getValue
}

// GetPipelined sends a GetValue request and returns a PendingGet that can be
// resolved later. This enables true pipelining: send N requests without
// waiting, then collect all N responses. Matches C++ client pipelining.
//
// Returns (nil, nil, nil) for RYW cache hits (value is returned in val).
// Returns (nil, pending, nil) for server requests (call pending.Resolve() to get value).
// Returns (nil, nil, err) for errors during send.
func (tx *Transaction) GetPipelined(ctx context.Context, key []byte) (val []byte, pending *PendingGet, err error) {
	if tx.routesSpecialKey(key) { // RYW:1634-1638
		val, err = tx.getSpecialKey(key)
		return val, nil, err
	}
	ctx, opCancel, owned := tx.opContextWithLease(ctx, false)
	op := tx.readOperation(ctx)
	defer owned.release() // only this invocation's send-phase lease
	defer func() {
		if pending == nil {
			opCancel.release()
		}
	}()
	if err := tx.checkReadState(ctx); err != nil {
		return nil, nil, tx.trackReadOperation(ctx, err)
	}
	// Legal key range check BEFORE sending — matches Transaction.Get. The illegal
	// key must be rejected at enqueue, not after the frame is on the wire. RFC-010 #3.
	// C++ RYW::getValue: if (key >= getMaxReadKey() && key != metadataVersionKey)
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	tx.hadRead.Store(true)
	// Routed through the RYW filter (RFC-121 D2) — a read served by a local independent
	// write adds no read-conflict, matching libfdb_c updateConflictMap.
	if !isSpecialKey(key) {
		tx.addReadConflictForKeyRYW(key)
	}

	if !tx.rywDisabled {
		tx.ryw.mu.Lock()
		w := tx.ryw.liveLocked()
		value, known, readErr := tx.ryw.localPointLocked(w, key)
		entry, _ := w.entryOf(key)
		tx.ryw.mu.Unlock()
		if known {
			return bytes.Clone(value), nil, tx.trackReadOperation(ctx, readErr)
		}
		if entry.hasAtomics {
			return nil, nil, ErrNeedFullRYW
		}
	}

	// GRV belongs to the remote read future, so its failure poisons Commit too.
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, nil, tx.trackReadOperation(ctx, err)
	}

	// Retain the captured incarnation through locate, send and deferred Resolve.
	// The send phase must not release the context that the reply still needs.

	// Capture under readVersionMu (RFC-175 E1): pipelined reads run concurrently with
	// Commit/Reset, whose resets write readVersion under the mutex. Same capture
	// convention as the read path (readpath.go sendGetValue/sendGetKey).
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()

	// Locate shard.
	loc, locErr := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId, tx.currentSpan(), false)
	if locErr != nil {
		return nil, nil, tx.mapReadError(ctx, fmt.Errorf("locate key: %w", locErr))
	}
	if len(loc.Servers) == 0 {
		return nil, nil, fmt.Errorf("no storage servers for key")
	}

	// Skip replicas whose endpoint is gone; with none left, send anyway and let
	// the reply's failure route the read through the full path.
	servers, ok := tx.db.failMon.liveServers(loc.Servers, EndpointGetValue)
	if !ok {
		servers = loc.Servers
	}
	return tx.sendPipelinedGet(ctx, op, opCancel, key, readVersion, servers)
}

func (tx *Transaction) sendPipelinedGet(ctx context.Context, op *readOperation, opCancel opRelease, key []byte, readVersion int64, servers []ServerInfo) ([]byte, *PendingGet, error) {
	// One GetValueRequest for every server tried (NativeAPI.actor.cpp:3681).
	tags := tx.sampleReadTags()
	// Send request without waiting for response.
	var replyCleanup replyGuard
	defer replyCleanup.close()
	var sendCleanup sendGuard
	defer sendCleanup.close()
	for _, server := range servers {
		conn, permit, dialErr := tx.db.getOrDialForSend(ctx, server.Address)
		if dialErr != nil {
			tx.db.handleDialError(ctx, server.Address)
			continue
		}
		sendCleanup.permit = permit
		replyToken, replyCh, replyHandle := conn.PrepareReplyTo(server.Token)
		replyCleanup.handle = replyHandle
		body, poolBuf := buildGetValueRequest(key, readVersion, tx.lockAware || tx.readLockAware, tx.tenantId, childSpanContext(tx.currentSpan()), tags, replyToken, server.Token)
		// RFC-114: stamp sentAt BEFORE sending. The frame may be written, and its reply
		// delivered (stamping resp.RecvAt), before the send returns — RecvAt−sentAt
		// would go negative and be silently dropped by the sketch.
		sentAt := time.Now()
		// First read of a burst uses eager send; subsequent reads use deferred send
		// (coalesced by writeLoop on WithLegacyWriter; both enqueue on default D-NG).
		tx.readErrMu.Lock()
		first := !tx.hasPendingReadsLocked()
		tx.readErrMu.Unlock()
		var sendErr error
		if first {
			_, sendErr = conn.SendFrameEagerContextOrdered(ctx, server.Token, body, permit)
		} else {
			_, sendErr = conn.SendFrameDeferredContextOrdered(ctx, server.Token, body, permit)
		}
		getValueBufPool.Put(poolBuf)
		if sendErr != nil {
			replyHandle.Cancel()
			replyCleanup.release()
			tx.db.handleReadConnError(server.Address, conn, sendErr)
			continue
		}
		// Timeout delivery ends this wait through ctx (RYW:361-391); the per-RPC
		// reply timer is acquired lazily in PendingGet.resolve if replyCh is not ready.
		p := &PendingGet{tx: tx, addr: server.Address, tenantID: tx.tenantId, replyCh: replyCh, replyHandle: replyHandle, conn: conn, ctx: ctx, cancel: opCancel.afterLease(), sentAt: sentAt, gen: op.inc.gen, tags: tags}
		if len(key) <= len(p.keyBuf) {
			p.key = p.keyBuf[:len(key):len(key)]
			copy(p.key, key)
		} else {
			p.key = bytes.Clone(key)
		}
		if tx.hooks != nil && tx.hooks.afterPendingSend != nil {
			tx.hooks.afterPendingSend(p)
		}
		// Register under the current read incarnation: Commit drains
		// outstanding pipelined reads (the C++ wait(reading) completion
		// barrier) and a post-reset late Resolve must not poison the next
		// incarnation.
		tx.readErrMu.Lock()
		if tx.readLife != op.inc || op.inc.cause != nil {
			tx.readErrMu.Unlock()
			// Failure can detach the registry before this send is registered.
			// Retire it here, preserving any already-published reply without
			// requiring the caller to Resolve just to release its resources.
			replyCleanup.transfer()
			p.retireLocked()
			return p.memoVal, nil, p.memoErr
		}
		tx.addPendingReadLocked(p)
		tx.readErrMu.Unlock()
		replyCleanup.transfer()
		return nil, p, nil
	}
	// If every dial above failed because the SetTimeout deadline expired (ctx
	// cancelled), surface transaction_timed_out (1031) rather than the
	// non-retryable all_alternatives_failed (1006) — a cold-dial expiry is still a
	// timeout, matching C++ (the timebomb wins the loadBalance race). RFC-112.
	if err := ctx.Err(); err != nil {
		return nil, nil, tx.mapReadError(ctx, err)
	}
	return nil, nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

// PendingGet represents a GetValue request that has been sent but not yet resolved.
type PendingGet struct {
	key           []byte
	keyBuf        [64]byte
	tx            *Transaction
	tenantID      int64
	replyConsumed bool
	addr          string // storage server the deferred frame was sent to
	replyCh       <-chan transport.Response
	replyHandle   *transport.ReplyHandle
	conn          *transport.Conn
	ctx           context.Context
	cancel        context.CancelFunc
	timer         *time.Timer
	hooks         *pendingGetTestHooks // nil in production
	sentAt        time.Time            // RFC-114: send time, for the pipelined read-latency sample
	flushed       bool
	gen           uint64        // read incarnation at issue (see Transaction.readGen)
	tags          readTagSample // the request's draw, kept by a fallback loadBalance would retry itself

	// Resolve is idempotent: the first caller (the future's .Get(), or
	// Commit's drain — whichever runs first) does the work; later callers get
	// the memo. mu also serializes the reply-channel consumption.
	mu      sync.Mutex
	done    bool
	memoVal []byte
	memoErr error
	// settledFlag mirrors done for waiters that must not block on mu; settled
	// is closed when done, made on demand by such a waiter.
	settledFlag atomic.Bool
	settled     atomic.Pointer[chan struct{}]
}

type pendingGetTestHooks struct {
	onReplyTimer func(*time.Timer) // optional test hook run when timer is lazily acquired
}

func (p *PendingGet) testHooks() *pendingGetTestHooks {
	if p.hooks == nil {
		p.hooks = &pendingGetTestHooks{}
	}
	return p.hooks
}

// GetKey resolves a key selector to the actual key in the database: C++ RYW getKey
// (ReadYourWrites.actor.cpp:1695-1709), which walks the pending writes and the
// snapshot cache and reads storage only for what they do not answer (RFC-262 D6).
func (tx *Transaction) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	return tx.issueKey(ctx, KeySelector{Key: selectorKey, OrEqual: orEqual, Offset: offset}, false).run()
}

// normalizeByteTarget applies the C API's boundary normalization for a per-fetch byte target,
// porting fdb_c.cpp:993:
//
//	/* Zero at the C API maps to "infinity" at lower levels */
//	if (!target_bytes) target_bytes = GetRangeLimits::BYTE_LIMIT_UNLIMITED;
//
// It matters because 0 does NOT mean "no target" further down: C++ asserts a positive byte
// limit on every range request it builds (ASSERT(req.limitBytes > 0), NativeAPI.actor.cpp:4300
// and :4682), so a zero forwarded to the wire is a request for a reply that can hold nothing.
// The normalization has to happen at the exported boundary, which is where the C client does
// it — GetRangeWithByteTarget is callable by anyone.
//
// Anything below ByteLimitUnlimited is rejected rather than normalized, matching how a row
// limit < -1 is range_limits_invalid instead of being clamped: GetRangeLimits::isValid
// (FDBTypes.h) accepts bytes >= 0 or exactly BYTE_LIMIT_UNLIMITED, and nothing else.
func normalizeByteTarget(byteTarget int) (int, error) {
	if byteTarget == 0 {
		return ByteLimitUnlimited, nil
	}
	if byteTarget < ByteLimitUnlimited {
		return 0, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	return byteTarget, nil
}

// maxReadKey returns the maximum readable key for this transaction.
// Without readSystemKeys/writeSystemKeys: \xff (user keys only).
// With: \xff\xff (system keys allowed, special keys still rejected).
// Matches C++ ReadYourWritesTransaction::getMaxReadKey().
func (tx *Transaction) maxReadKey() []byte {
	if tx.readSystemKeys || tx.writeSystemKeys {
		return []byte{0xff, 0xff}
	}
	return []byte{0xff}
}

// isSpecialKey returns true for keys with the \xff\xff prefix (FDB special key
// space). Note this is the special-key module range (\xff\xff/...), NOT the
// system keyspace (single \xff/... prefix); the name reflects what the byte
// check actually tests.
func isSpecialKey(key []byte) bool {
	return len(key) >= 2 && key[0] == 0xff && key[1] == 0xff
}

// GetRange reads a range of keys [begin, end) in forward order.
func (tx *Transaction) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	return tx.getRangeDir(ctx, begin, end, limit, ByteLimitUnlimited, false)
}

// GetRangeReverse reads a range of keys [begin, end) in reverse order.
// Matches C++ where negative limit = reverse scan.
func (tx *Transaction) GetRangeReverse(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	return tx.getRangeDir(ctx, begin, end, limit, ByteLimitUnlimited, true)
}

// GetRangeWithByteTarget is GetRange carrying a per-fetch BYTE target — the dimension libfdb_c
// derives from the streaming mode and puts on the request, where the storage server truncates
// against it. That truncation, not any client-side accounting, is what divides a scan into
// batches; see rangeScanImpl's soft byte limit for the half that ends the call.
// ByteLimitUnlimited means "no target", which is what plain GetRange passes and what EXACT
// resolves to.
func (tx *Transaction) GetRangeWithByteTarget(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return tx.getRangeDir(ctx, begin, end, limit, byteTarget, reverse)
}

func (tx *Transaction) getRangeDir(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	return tx.issueRange(ctx, begin, end, limit, byteTarget, reverse, false).run(true)
}

// GetReadVersion returns the read version for this transaction, fetching it
// from a GRV proxy if not already set. Matches C++ fdb_transaction_get_read_version.
func (tx *Transaction) GetReadVersion(ctx context.Context) (int64, error) {
	ctx, cancel := tx.opContext(ctx)
	defer cancel()
	rv, err := tx.readVersionForOperation(ctx, 0, false)
	if err != nil {
		return 0, err
	}
	return rv, nil
}
