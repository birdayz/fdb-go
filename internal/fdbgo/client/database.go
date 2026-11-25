// Package client implements the FDB client transaction lifecycle.
// This is the Go equivalent of NativeAPI.actor.cpp.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/internal/fdbgo/sendqueue"
	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
)

// DBInfo holds the current cluster topology.
// Received from coordinators via OpenDatabaseCoordRequest.
type DBInfo struct {
	ID            transport.UID
	GRVProxies    []ProxyInfo
	CommitProxies []ProxyInfo
	ClusterID     transport.UID
	// Forward, when non-empty, is the serialized new ClusterConnectionString the
	// coordinators handed back instead of proxies — the C++ ClientDBInfo.forward
	// field (CommitProxyInterface.h:132). Set during a `coordinators auto`/`change`
	// rotation; the proxies on a forward reply are ignored (RFC-111 Path A).
	Forward string

	// Epoch is the cluster epoch these proxies belong to. It rides the SNAPSHOT
	// rather than living beside it, so a caller that loads the proxies gets the
	// epoch of the very set it is about to talk to — the epoch a request binds
	// to cannot describe a proxy set the request did not use, because there is
	// no second load in which the two could disagree.
	//
	// Set exclusively by installProxySet, which also owns the ordering against
	// grvCache's fence word. See the coherence rule documented there.
	Epoch int64
}

// ProxyInfo holds addressing info for a proxy.
type ProxyInfo struct {
	Address string // "host:port"
	Token   transport.UID
}

// database is the per-database state container (unexported).
// Matches C++ DatabaseContext in fdbclient/DatabaseContext.h.
// All per-database state lives here: topology, caches, connections, batchers.
//
// Safe for concurrent use by multiple goroutines after creation.
type database struct {
	// connRecord owns the mutable active connection string (coordinator set +
	// cluster key) and its on-disk persistence — the C++ IClusterConnectionRecord
	// analog. Coordinator rotations are followed here (RFC-111): a forwarded
	// connection string (Path A) or an externally-rewritten cluster file (Path B)
	// swaps the active set so a `coordinators auto`/`change` no longer strands us.
	connRecord *connRecord
	// forwardHops bounds a pathological coordinator-forward cycle (Go-only
	// divergence — C++ has no hop bound; RFC-111 §5). Written only by the single
	// active follow path: bootstrap first, then exclusively the topology-monitor
	// goroutine (the two never run concurrently, so no atomic is needed). Reset to
	// 0 on every successful non-forward connect.
	forwardHops int
	// bootstrapCoordinator answered bootstrap; the monitor long-polls it first so
	// a lagging coordinator cannot reinstall older info. Set before the monitor starts.
	bootstrapCoordinator string
	dialFn               transport.DialFunc // nil = net.DialTimeout
	dialOpts             []transport.DialOption
	// tlsConfig is the single source of truth for transport security: non-nil =>
	// every connection (coordinators, proxies, storage) is dialed over TLS with
	// this *tls.Config; nil => plaintext. Set at open from WithTLSConfig (which
	// wins) or the cluster ":tls" suffix + FDB_TLS_* resolution.
	tlsConfig *tls.Config

	// Topology: atomically swapped on coordinator refresh.
	// C++: clientInfo (AsyncVar<ClientDBInfo>)
	dbInfo atomic.Pointer[DBInfo]
	// installMu serialises installProxySet's read-modify-write of dbInfo. The
	// sequence loads dbInfo, derives the next epoch from it, possibly bumps the
	// fence, and stores — and atomic.Pointer gives atomicity of each step, never
	// of the sequence. A lost update here is not a stale read that self-corrects
	// on the next poll: the losing writer publishes DBInfo.Epoch=N after the fence
	// moved to N+1 with clusterSwitchPending ALREADY consumed, so nothing will
	// ever bump DBInfo.Epoch again. Every request from then on binds an epoch the
	// fence has passed, every publication is refused, and the refresher pins at
	// its 1ms clamp — permanently, on a handle that merely polled topology twice.
	//
	// Production has one writer today (bootstrap completes before topologyMonitor
	// starts), but that is an emergent property of the startup order rather than
	// anything this function can rely on, and tests already call it from a second
	// goroutine while the monitor is live.
	installMu sync.Mutex
	hooks     atomic.Pointer[dbTestHooks]
	// Broadcast: closed when proxy list changes, then replaced with a fresh channel.
	// Commit monitors this to detect mid-commit proxy changes (C++ onProxiesChanged).
	proxiesChangedMu sync.Mutex
	proxiesChanged   chan struct{}

	// Connection pool. C++ uses FlowTransport; we need explicit pool.
	connMu        sync.RWMutex
	connPool      map[string]*transport.Conn
	hasConnLinger bool
	lastMinDelay  time.Duration
	lastMaxDelay  time.Duration
	lastAdaptive  time.Duration
	lastWaits     bool
	// dialing coalesces concurrent dials to the same address (singleflight): the
	// first miss dials, later misses to the same address wait on its result rather
	// than each running a redundant TCP/TLS/ConnectPacket handshake. Guarded by
	// connMu. Mirrors C++ FlowTransport, where one Peer (one connectionKeeper) owns
	// the single dial per NetworkAddress.
	dialing map[string]*dialCall

	// Location cache. C++: CoalescedKeyRangeMap<Reference<LocationInfo>>.
	locCache locationCache

	// Per-endpoint health tracking. Wakes GRV backoff on recovery.
	failMon *failureMonitor

	// Load balancing: QueueModel for storage servers, round-robin for proxies.
	queueModel *QueueModel
	proxyRR    proxyRoundRobin

	// GRV cache + per-priority batchers. C++: cachedReadVersion, versionBatcher.
	// One batcher per priority level: [BATCH, DEFAULT, SYSTEM_IMMEDIATE].
	grvCache    grvCache
	grvBatchers [3]*grvBatcher

	// minAcceptableReadVersion is the observed floor for storage/commit validation,
	// not an MVCC boundary or a check on the explicit-pin setter/getter.
	//
	// It carries the EPOCH it was observed in, in the same value, for the reason
	// grvCacheEntry does: a floor is a claim about a VERSION SPACE, and the two
	// clusters' spaces are unrelated. Stamped, a value written just before a
	// handoff becomes inert the moment the fence moves — a reader validates the
	// epoch in the same load that produced the version, so there is no window in
	// which the two disagree, and no reliance on anyone remembering to clear it.
	minAcceptableReadVersion atomic.Pointer[minAcceptableStamp]

	// clusterSwitchPending is set when a coordinator set is adopted and cleared
	// at the proxy handoff that completes it. See onCoordinatorSetAdopted.
	clusterSwitchPending atomic.Bool

	// Tag throttle state — updated from GRV reply tagThrottleInfo.
	// Maps priority -> (tag -> throttle limits). Matches C++ cx->throttledTags.
	tagThrottles tagThrottleState

	// Transaction defaults — applied to every new transaction.
	// Matches C++ DatabaseContext::transactionDefaults.
	//
	// Published as an IMMUTABLE snapshot. C++ does NOT achieve this by thread
	// confinement — fdb_database_set_option and fdb_database_create_transaction both
	// run on the caller's thread — but by same-priority FIFO onto the network thread
	// plus a bulk, non-yielding copy of the whole option list when a transaction is
	// built. See the fuller citation chain on internalDB.txDefaults in
	// pkg/fdbgo/fdb/database.go.
	//
	// Go has no equivalent, and the SetTransaction*/SetDefault* setters below are
	// exported, so any goroutine may set a default while another creates a
	// transaction. Swapping the whole snapshot (rather than per-field atomics) is what
	// keeps the set coherent: a transaction must not pick up the new timeout alongside
	// the old retry limit.
	//
	// nil means "nothing ever set" and reads as the zero TransactionDefaults.
	txDefaults atomic.Pointer[TransactionDefaults]
	// txDefaultsMu serializes the read-modify-write in the setters.
	txDefaultsMu sync.Mutex

	// Operational counters — the C++ DatabaseContext CounterCollection subset.
	// Exposed via Database.Metrics(). RFC-097.
	metrics ClientMetrics

	// Per-handle operational-event logger (WithLogger; nil-resolved to
	// slog.Default() at open). RFC-097 / P1.2.
	logger *slog.Logger

	// Speculative second request (hedge) control: when true, a read may send a hedge
	// to a second server, timed and paid for by queueModel as libfdb_c does.
	hedgeEnabled atomic.Bool

	// Opt-in GetRange materialization ceiling in bytes (WithRangeByteCeiling,
	// RFC-115 §2). 0 = unlimited (default, oracle-matching). Read-only after open
	// (set once in the constructor), so no atomic needed.
	rangeByteCeiling int64

	// Distributed-trace sample rate (WithTracingSampleRate, RFC-115 §4). 0.0 =
	// unsampled (default, matches C++ TRACING_SAMPLE_RATE). Read-only after open.
	tracingSampleRate float64

	// Read tag sampling (sampleReadTags): READ_TAG_SAMPLE_RATE unless a test
	// overrides it. The zero value never samples. Read-only after open.
	readTags readTagSampling

	// A nil tracer disables export without constructing OTel contexts or spans.
	// Wire propagation and sampling remain independent of the export backend.
	tracer oteltrace.Tracer

	// Selected FDB API version (WithAPIVersion, RFC-149) — the C++
	// DatabaseContext::apiVersion analog. Mandatory-set: OpenDatabase rejects an
	// unset (0) version, so apiVersionAtLeast can never silently no-op. Gates
	// version-dependent wire behaviour (e.g. the Min→MinV2/And→AndV2 atomic upgrade
	// at >=510). Read-only after open.
	apiVersion int

	// outstandingWatches/maxWatches: per-Database cap on concurrently-outstanding (not-yet-fired,
	// not-yet-cancelled) watches — C++ DatabaseContext::increaseWatchCounter (NativeAPI.actor.cpp:5694)
	// throws too_many_watches (1032) once outstandingWatches >= maxWatches. maxWatches defaults to
	// DEFAULT_MAX_OUTSTANDING_WATCHES (10000, ClientKnobs.cpp:120) and is lowerable via the
	// max_watches database option (SetMaxWatches). 0 means unlimited.
	outstandingWatches atomic.Int64

	// rangeReadPanics is the RFC-110 backstop shared by this database's range reads.
	rangeReadPanics panicBackstop
	// resolveKeyPanics is the getRange fallback resolveKey goroutines' backstop.
	resolveKeyPanics panicBackstop
	maxWatches       atomic.Int64

	// Lifecycle.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	connected chan struct{}
	// timeouts delivers its transactions' TIMEOUT expiries; Close leaves it running.
	timeouts *timeoutQueue
	wg       sync.WaitGroup
	// GRV refreshers enroll under closeMu; dials and connections enroll under connMu.
	// Close seals both paths before wg.Wait, including zero-count shutdown.
	closeMu sync.Mutex
	closed  bool
}

type dbTestHooks struct {
	// beforeCommitProxySelect runs at the top of Transaction.commit, immediately
	// before it binds to a commit proxy. A deterministic seam for the
	// handoff-inside-the-commit-window case, which is a few instructions wide and
	// therefore untestable by racing. Always nil in production.
	beforeCommitProxySelect atomic.Pointer[func()]
	// duringProxyInstall runs inside installProxySet, immediately before the dbInfo
	// publication. It exists so a test can hold an installer inside the critical
	// section (see installMu) and observe the fence at publication. Always nil in
	// production.
	//
	// IT RUNS UNDER installMu, which is not reentrant: a hook that calls back into
	// installProxySet deadlocks the calling goroutine against itself. Hooks here
	// may observe and block; they may not install.
	duringProxyInstall atomic.Pointer[func()]
	// afterLocationSend observes a sent location query before its reply wait (test seam).
	afterLocationSend atomic.Pointer[func()]
	// beforeConnFailureMark runs in handleConnError just before it marks the address
	// failed (test seam). Nil in production.
	beforeConnFailureMark atomic.Pointer[func()]
	// Test seams observe dial publication, shutdown joining and disconnect/reply races.
	afterDialBeforePool          atomic.Pointer[func()]
	beforeCloseJoin              atomic.Pointer[func()]
	beforeMetricsDisconnectReply atomic.Pointer[func()]
	beforeMetricsReplyWait       atomic.Pointer[func()]
	afterMetricsShardResult      atomic.Pointer[func(int)]
	// beforeAltFailureWait runs when a request starts waiting for an excluded replica (tests).
	beforeAltFailureWait atomic.Pointer[func()]
	// onReadBackoff observes Get/Watch retry accounting before the delay (tests).
	onReadBackoff atomic.Pointer[func(attempts int, delay time.Duration)]
	// Test seams: set the delay before dispatch; observe actual-live recovery checks.
	altFailureDelayOverride atomic.Int64
	afterAltRecoveryCheck   atomic.Pointer[func(bool)]
}

func (db *database) testHooks() *dbTestHooks {
	if h := db.hooks.Load(); h != nil {
		return h
	}
	h := &dbTestHooks{}
	if db.hooks.CompareAndSwap(nil, h) {
		return h
	}
	return db.hooks.Load()
}

func (db *database) readBackoffObserver() *func(attempts int, delay time.Duration) {
	if h := db.hooks.Load(); h != nil {
		return h.onReadBackoff.Load()
	}
	return nil
}

func (db *database) metricsShardResultHook() *func(int) {
	if h := db.hooks.Load(); h != nil {
		return h.afterMetricsShardResult.Load()
	}
	return nil
}

// defaultMaxOutstandingWatches mirrors CLIENT_KNOBS->DEFAULT_MAX_OUTSTANDING_WATCHES (ClientKnobs.cpp:120).
const defaultMaxOutstandingWatches = 10000

// absoluteMaxWatches mirrors CLIENT_KNOBS->ABSOLUTE_MAX_WATCHES (ClientKnobs.cpp:121) — the upper
// bound MAX_WATCHES accepts; a higher (or negative) value is rejected with invalid_option_value.
const absoluteMaxWatches = 1_000_000

// tryAcquireWatch reserves an outstanding-watch slot, returning too_many_watches (1032) when the
// cap is exceeded — C++ increaseWatchCounter (NativeAPI.actor.cpp:2175-2179, called from
// Transaction::watch :5694). Each successful acquire MUST be matched by exactly one releaseWatch.
func (db *database) tryAcquireWatch() error {
	// C++ DatabaseContext::increaseWatchCounter (NativeAPI.actor.cpp:2175-2177) throws when
	// `outstandingWatches >= maxOutstandingWatches`. n is the post-increment count
	// (outstandingWatches+1), so `n > max` is exactly `outstandingWatches >= max`. maxWatches is
	// NOT an unlimited sentinel: MAX_WATCHES=0 (and any negative, which SetMaxWatches/C++
	// extractIntOption clamp to 0 — NativeAPI:2139) is a HARD 0-cap, so the FIRST watch fails 1032.
	n := db.outstandingWatches.Add(1)
	if n > db.maxWatches.Load() {
		db.outstandingWatches.Add(-1)
		return &wire.FDBError{Code: ErrTooManyWatches}
	}
	return nil
}

// releaseWatch frees an outstanding-watch slot (C++ decreaseWatchCounter).
func (db *database) releaseWatch() { db.outstandingWatches.Add(-1) }

// apiVersionAtLeast reports whether the selected API version is >= v. Mirrors
// C++ DatabaseContext::apiVersionAtLeast (Transaction::apiVersionAtLeast →
// trState->cx->apiVersionAtLeast). Gates version-dependent wire behaviour.
func (db *database) apiVersionAtLeast(v int) bool { return db.apiVersion >= v }

func (db *database) getOrDial(ctx context.Context, addr string) (*transport.Conn, error) {
	conn, _, err := db.getOrDialConn(ctx, addr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// handleDialError reacts to a getOrDial error. A context cancellation is the
// caller giving up, not the endpoint failing, so only a dial failure with a live
// ctx is reported; it has no connection to name.
func (db *database) handleDialError(ctx context.Context, addr string) {
	if ctx.Err() == nil {
		db.handleConnError(addr, nil)
	}
}

// dialCall is one in-flight singleflight dial. Waiters block on done, then read
// conn/err — written by the dialer before close(done), so the channel close
// establishes the happens-before.
type dialCall struct {
	done  chan struct{}
	conn  *transport.Conn
	err   error
	sends sendqueue.Queue
}

// getOrDialConn returns a pooled or freshly-dialed connection.
// dialed is true when THIS call is the cache miss that started the dial (the
// "owner"); callers that coalesced onto an in-flight dial get dialed=false.
//
// The dial runs WITHOUT holding connMu — holding the pool lock across the dial
// (TCP connect + TLS upgrade + ConnectPacket handshake) is a deadlock amplifier:
// one stalled dial would block every goroutine acquiring ANY connection and wedge
// the whole client. And concurrent misses to the SAME address are coalesced
// (singleflight), so a burst to one cold proxy opens ONE socket, not O(requests).
// Both mirror C++ FlowTransport: one Peer per NetworkAddress, its single
// connectionKeeper owning the dial, no global dial lock.
//
// The dial itself runs on a goroutine bound to db.ctx (+ RPC timeout), NOT to any
// one caller's context, so a caller whose ctx cancels merely abandons its own wait
// — it never aborts the dial that the other waiters share (matching C++, where the
// connectionKeeper's dial outlives any single request).
func (db *database) getOrDialConn(ctx context.Context, addr string) (conn *transport.Conn, dialed bool, err error) {
	conn, dialed, _, err = db.getOrDialConnFor(ctx, addr, false)
	return conn, dialed, err
}

// dialAndPool runs the single shared dial for addr and publishes the result to all
// waiters via call. It is bound to db.ctx (not a caller ctx); on database close
// (db.ctx canceled) the dial aborts and any connection that nonetheless completed
// is discarded rather than pooled past Close()'s pool drain.
//
// TODO: C++ FlowTransport deduplicates bidirectional connections via ConnectionID
// exchange in ConnectPacket. We don't need this as a pure client (we never accept
// incoming connections), but should implement it if we ever add server-side
// functionality.
func (db *database) dialAndPool(addr string, call *dialCall) {
	// RFC-110: a panic in transport.Dial (or the pooling section)
	// skips the normal delete(db.dialing)+close(call.done) below, leaving the
	// singleflight entry for addr in db.dialing with call.done NEVER closed —
	// every later caller in getOrDialConn coalesces onto it and blocks until its
	// own ctx expires, and no fresh dial ever starts: a permanently poisoned
	// entry. The backstop publishes the failure so waiters wake and the next
	// caller re-dials. The connMu section below is closure-scoped so a panic in
	// it unwinds the lock (else this recover's Lock would deadlock).
	pb := &panicBackstop{name: "dialAndPool", db: db}
	var c *transport.Conn // owned here until the result is published
	defer func() {
		r := recover()
		if !pb.recovered(r) {
			return
		}
		closeConn := false
		func() {
			db.connMu.Lock()
			defer db.connMu.Unlock()
			select {
			case <-call.done:
				return
			default:
			}
			if db.dialing[addr] == call {
				delete(db.dialing, addr)
			}
			if c != nil {
				if db.connPool[addr] == c {
					delete(db.connPool, addr)
				}
				closeConn = true
			}
			call.err = fmt.Errorf("fdbgo: panic dialing %s: %v", addr, r)
			call.sends.Seal()
			close(call.done)
		}()
		if closeConn {
			_ = c.Close()
		}
	}()

	dialCtx, cancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
	defer cancel() // RFC-110: release the dial timer even if transport.Dial panics
	dialOpts := append([]transport.DialOption{}, db.dialOpts...)
	dialOpts = append(dialOpts, transport.WithSendOrder(&call.sends))
	var dialErr error
	c, dialErr = transport.Dial(dialCtx, addr, db.tlsConfig, db.dialFn, dialOpts...)
	if h := db.hooks.Load(); h != nil {
		if hook := h.afterDialBeforePool.Load(); hook != nil {
			(*hook)()
		}
	}

	discard := false
	func() {
		db.connMu.Lock()
		defer db.connMu.Unlock()
		switch {
		case dialErr != nil && db.ctx.Err() != nil:
			call.err = db.ctx.Err() // the database closed; no peer failed
		case dialErr != nil:
			// A failed connect, including the dial ctx's expiry, is C++'s peer disconnect:
			// request_maybe_delivered, not the raw dial error (FlowTransport.actor.cpp:852-869).
			call.err = transport.NewConnectFailedError(addr, dialErr)
		case db.ctx.Err() != nil:
			// The database closed while we dialed: Close() drains the pool under connMu,
			// so a conn pooled now would never be reaped (leaking its read/write/monitor
			// goroutines). Discard it and fail the call.
			call.err = db.ctx.Err()
			discard = true
		default:
			c.SetEndpointNotFoundHandler(func(token transport.UID) { db.failMon.markEndpointNotFound(addr, token) })
			db.connPool[addr] = c
			db.lastMinDelay, db.lastMaxDelay, db.lastWaits = c.CoalesceDelay()
			db.lastAdaptive = c.AdaptiveLinger()
			db.hasConnLinger = true
			// Enroll before publication, while Close's pool-drain barrier is
			// excluded. Ownership survives eviction, leases and stale cohorts.
			db.wg.Add(1)
			go func(conn *transport.Conn) {
				defer db.wg.Done()
				<-conn.Done()
				_ = conn.Close()
			}(c)
			call.conn = c
			// Clear the failed state on a successful dial HERE — not in a caller (so a
			// reconnect wakes failure-monitor recovery even if every caller abandoned
			// its wait), and BEFORE the connection becomes visible (connMu unlock +
			// close(call.done)). Marking after exposure would race: a waiter could grab
			// the conn, fail its first RPC and markFailed, and this stale markAlive would
			// then overwrite that real failure. failureMonitor uses only its own lock
			// (it never reaches connMu/db), so nesting it here is lock-order-safe.
			db.failMon.markAlive(addr)
		}
		call.sends.Seal() // no arrival after dial completion can extend this cohort
		drained := !call.sends.Pending()
		if call.err != nil || drained {
			if db.dialing[addr] == call {
				delete(db.dialing, addr)
			}
			if call.conn != nil && drained {
				call.conn.ClearSendOrder(&call.sends)
			}
		}
		close(call.done) // membership and result become visible atomically under connMu
	}()
	if discard {
		_ = c.Close()
	}
}

// tryAllCoordinators races all coordinators in parallel, returning the first
// successful response.
//
// First-reply-wins is C++-FAITHFUL, not a divergence. The libfdb_c client adopts
// cluster topology on the FIRST successful coordinator reply, NOT on a majority
// quorum: monitorProxiesOneGeneration probes coordinators round-robin and adopts
// the first successful OpenDatabaseCoordRequest reply (MonitorLeader.actor.cpp:919-937);
// the `majority` bool that getLeader() computes (:578, `bestCount >= nominees.size()/2+1`)
// is SERVER-SIDE leader-election metadata and does NOT gate the client's topology
// adoption (monitorLeaderOneGeneration calls getLeader() on whatever nominees have
// arrived, with no quorum wait, :604/:634). Adding a coordinator quorum here would
// make Go STRICTER than libfdb_c — a conformance violation, not a robustness fix
// (RFC-115 §3, FDB-C-dev verified). The ONLY Go-vs-C++ difference is the probing
// shape: Go races the set in parallel where C++ goes sequential round-robin — benign
// (identical first-success outcome, lower latency) and never contacts more than the
// coordinator set. (Cluster-file re-read stays failure-gated to match C++'s
// allConnectionsFailed path, :888-900 — RFC-111; do not add a periodic timer.)
func (db *database) tryAllCoordinators(ctx context.Context, snap *ClusterFile) (*DBInfo, string, error) {
	if snap == nil || len(snap.Coordinators) == 0 {
		// Defensive — production cluster-file parsing rejects empty
		// coordinator lists, but a hand-constructed *database in tests can
		// reach this path. Without the guard, the for-loop below returns
		// (nil, nil) and bootstrap nil-derefs the info.
		return nil, "", fmt.Errorf("no coordinators configured")
	}
	if len(snap.Coordinators) == 1 {
		// Fast path: single coordinator, no goroutine overhead.
		info, err := db.tryOneCoordinator(ctx, snap, snap.Coordinators[0])
		return info, snap.Coordinators[0], err
	}

	type result struct {
		info *DBInfo
		addr string
		err  error
	}
	ch := make(chan result, len(snap.Coordinators))
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, addr := range snap.Coordinators {
		go func(addr string) {
			info, err := db.tryOneCoordinator(raceCtx, snap, addr)
			ch <- result{info, addr, err}
		}(addr)
	}

	var lastErr error
	for range snap.Coordinators {
		r := <-ch
		if r.err == nil {
			cancel() // cancel remaining attempts
			return r.info, r.addr, nil
		}
		lastErr = r.err
	}
	return nil, "", lastErr
}

func (db *database) tryOneCoordinator(ctx context.Context, snap *ClusterFile, addr string) (info *DBInfo, err error) {
	// RFC-110: recover on the WORKER. tryAllCoordinators calls this
	// both from the parallel fan-out goroutines AND directly on the caller's
	// goroutine for a single coordinator (the common test/dev shape), so a panic
	// in the dial / openDatabaseCoord decode must become a returned error here to
	// cover both paths — a recover only on the fan-out closure would miss the
	// single-coordinator fast path. The fan-out leg then forwards this error like
	// a failed quorum(ok,1) leg; the direct call returns it to its caller.
	pb := &panicBackstop{name: "tryOneCoordinator", db: db}
	defer func() {
		if r := recover(); pb.recovered(r) {
			info, err = nil, fmt.Errorf("fdbgo: panic contacting coordinator %s: %v", addr, r)
		}
	}()
	conn, permit, dialErr := db.getOrDialForSend(ctx, addr)
	if dialErr != nil {
		return nil, dialErr
	}
	defer permit.Release()
	info, err = db.openDatabaseCoord(ctx, conn, snap, transport.UID{}, CoordinatorTimeout, permit)
	if err != nil {
		return nil, fmt.Errorf("coordinator %s: %w", addr, err)
	}
	return info, nil
}
