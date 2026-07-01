// Package client implements the FDB client transaction lifecycle.
// This is the Go equivalent of NativeAPI.actor.cpp.
package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// ClusterFile represents an fdb.cluster file.
// Format: "<description>:<id>@<host>:<port>[,<host>:<port>...]"
type ClusterFile struct {
	Description  string
	ID           string
	Coordinators []string // "host:port" addresses for TCP connection (":tls" suffix stripped)
	// UseTLS is true when the coordinators carry the ":tls" suffix (FDB
	// FLAG_TLS). A real cluster is uniformly TLS, so this is a single flag;
	// mixed TLS/non-TLS coordinators are rejected at parse time.
	UseTLS bool
}

// ParseClusterFile reads and parses an fdb.cluster file.
func ParseClusterFile(path string) (*ClusterFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cluster file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		return ParseClusterString(line)
	}
	return nil, fmt.Errorf("empty cluster file: %s", path)
}

// trimConnString strips all whitespace and `#…`-to-end-of-line comments from a connection
// string, matching C++ ClusterConnectionString's trim() (MonitorLeader.actor.cpp:34-50), which
// runs before parsing. This makes "desc : id @ host:port" and inline `# comments` parse
// identically to libfdb_c/Java instead of being rejected.
func trimConnString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '#' {
			// Skip the comment up to and including the next newline (the outer i++ consumes it).
			i++
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		} else if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ParseClusterString parses a cluster connection string.
func ParseClusterString(s string) (*ClusterFile, error) {
	// Format: "description:id@host1:port1,host2:port2,host3:port3"
	s = trimConnString(s) // C++ trim() before parsing (MonitorLeader.actor.cpp:34)
	atIdx := strings.LastIndex(s, "@")
	if atIdx < 0 {
		return nil, fmt.Errorf("invalid cluster string: missing '@': %q", s)
	}

	prefix := s[:atIdx]
	addrs := s[atIdx+1:]
	if addrs == "" {
		return nil, fmt.Errorf("no coordinators in cluster string: %q", s)
	}

	colonIdx := strings.Index(prefix, ":")
	if colonIdx < 0 {
		return nil, fmt.Errorf("invalid cluster string: missing ':' in prefix: %q", s)
	}

	cf := &ClusterFile{
		Description: prefix[:colonIdx],
		ID:          prefix[colonIdx+1:],
	}

	// Validate the description:id key to C++'s parseKey acceptance set so a
	// persisted cluster key is always parseable by a C++/Java client (RFC-111 §8).
	if !validClusterKeyPart(cf.Description, true) || !validClusterKeyPart(cf.ID, false) {
		return nil, fmt.Errorf("invalid cluster key %q: description must be [a-zA-Z0-9_], id must be [a-zA-Z0-9]", prefix)
	}

	tlsCount := 0
	seenCoords := make(map[string]bool)
	for _, addr := range strings.Split(addrs, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			// C++ NetworkAddress::parse("") throws connection_string_invalid on an empty
			// coordinator segment — a leading/trailing/double comma, or a comma left dangling
			// after trim() removed an inline comment (e.g. "h:4500, # disabled" → "h:4500,").
			// (MonitorLeader.actor.cpp:92.) Do NOT silently skip it — that accepts strings C++
			// rejects, breaking cross-tool cluster-file compatibility.
			return nil, fmt.Errorf("empty coordinator in cluster string: %q", s)
		}
		// Faithful to C++ NetworkAddress::parse (flow/network.cpp): strip a
		// trailing "(fromHostname)" marker, then a trailing ":tls" — but only
		// when the string is longer than 4 chars (a bare ":tls" is not a flag,
		// it's an invalid address). The remaining text is host:port.
		addr = strings.TrimSuffix(addr, "(fromHostname)")
		isTLS := false
		if len(addr) > 4 && strings.HasSuffix(addr, ":tls") {
			isTLS = true
			addr = addr[:len(addr)-len(":tls")]
		}
		// Validate the token to C++'s acceptance set: a coordinator is valid iff
		// isHostname(tok) || NetworkAddress::parse(tok) (RFC-111 §8). This is
		// stricter than a bare net.SplitHostPort (which accepts foo:abc, :1234,
		// 1.2.3.4.5:port, ...): the cluster file is cross-tool shared state, and the
		// re-watch persist path must never write a token a C++/Java client can't
		// parse.
		if !isHostnameToken(addr) && !isNetworkAddressToken(addr) {
			return nil, fmt.Errorf("invalid coordinator address %q", addr)
		}
		// Reject duplicate coordinators — C++ ClusterConnectionString throws
		// connection_string_invalid on a duplicate address/hostname
		// (MonitorLeader.actor.cpp:109/117). Dedup on the CANONICAL parsed form
		// (coordDedupKey) so normalized duplicates (leading-zero port,
		// compressed-vs-expanded IPv6) collide the way C++'s NetworkAddress set
		// does — keeping Go-accept a subset of C++-accept so the persist path never
		// writes a dup-coordinator file C++/Java can't read.
		dedupKey := coordDedupKey(addr)
		if seenCoords[dedupKey] {
			return nil, fmt.Errorf("duplicate coordinator address %q", addr)
		}
		seenCoords[dedupKey] = true
		cf.Coordinators = append(cf.Coordinators, addr)
		if isTLS {
			tlsCount++
		}
	}

	// A real cluster is uniformly TLS or uniformly plaintext. A database-level
	// flag can't represent a mix, so reject it rather than silently dialing some
	// coordinators plaintext.
	if tlsCount > 0 && tlsCount != len(cf.Coordinators) {
		return nil, fmt.Errorf("mixed TLS and non-TLS coordinators not supported: %q", s)
	}
	cf.UseTLS = tlsCount > 0

	return cf, nil
}

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
}

// ProxyInfo holds addressing info for a proxy.
type ProxyInfo struct {
	Address string // "host:port"
	Token   transport.UID
}

// TransactionDefaults holds database-level defaults applied to every new
// transaction. Matches C++ DatabaseContext::transactionDefaults.
type TransactionDefaults struct {
	Timeout        int64 // FDB_DB_OPTION_TRANSACTION_TIMEOUT (ms), 0 = disabled
	RetryLimit     int   // FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT
	MaxRetryDelay  int64 // FDB_DB_OPTION_TRANSACTION_MAX_RETRY_DELAY (ms), 0 = use default
	SizeLimit      int64 // FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT; 0/unset → 10 MB default (C++ has no "disabled" state)
	HasRetryLimit  bool
	ReadSystemKeys bool // read \xff/* keys by default
	AccessSysKeys  bool // read+write \xff/* keys by default
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
	dialFn      transport.DialFunc // nil = net.DialTimeout
	// tlsConfig is the single source of truth for transport security: non-nil =>
	// every connection (coordinators, proxies, storage) is dialed over TLS with
	// this *tls.Config; nil => plaintext. Set at open from WithTLSConfig (which
	// wins) or the cluster ":tls" suffix + FDB_TLS_* resolution.
	tlsConfig *tls.Config

	// Topology: atomically swapped on coordinator refresh.
	// C++: clientInfo (AsyncVar<ClientDBInfo>)
	dbInfo atomic.Pointer[DBInfo]
	// Kick this channel to trigger an immediate topology refresh.
	topologyKick chan struct{} // buffered(1), non-blocking send
	// Broadcast: closed when proxy list changes, then replaced with a fresh channel.
	// Commit monitors this to detect mid-commit proxy changes (C++ onProxiesChanged).
	proxiesChangedMu sync.Mutex
	proxiesChanged   chan struct{}

	// Connection pool. C++ uses FlowTransport; we need explicit pool.
	connMu   sync.RWMutex
	connPool map[string]*transport.Conn
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

	// minAcceptableReadVersion tracks the minimum version this client has seen
	// from the cluster. SetReadVersion below this throws transaction_too_old
	// client-side, matching C++ DatabaseContext::validateVersion().
	minAcceptableReadVersion atomic.Int64

	// Tag throttle state — updated from GRV reply tagThrottleInfo.
	// Maps priority -> (tag -> throttle limits). Matches C++ cx->throttledTags.
	tagThrottles tagThrottleState

	// Transaction defaults — applied to every new transaction.
	// Matches C++ DatabaseContext::transactionDefaults.
	txDefaults TransactionDefaults

	// Operational counters — the C++ DatabaseContext CounterCollection subset.
	// Exposed via Database.Metrics(). RFC-097.
	metrics ClientMetrics

	// Per-handle operational-event logger (WithLogger; nil-resolved to
	// slog.Default() at open). RFC-097 / P1.2.
	logger *slog.Logger

	// Speculative second request (hedge) control.
	// When true, read RPCs send a hedge request to a second server after
	// secondDelay if the primary is slow. Matches C++ BACKUP_REQUEST_DELAY.
	// Default: true (always on, same as C++).
	hedgeEnabled atomic.Bool

	// Opt-in GetRange materialization ceiling in bytes (WithRangeByteCeiling,
	// RFC-115 §2). 0 = unlimited (default, oracle-matching). Read-only after open
	// (set once in the constructor), so no atomic needed.
	rangeByteCeiling int64

	// Distributed-trace sample rate (WithTracingSampleRate, RFC-115 §4). 0.0 =
	// unsampled (default, matches C++ TRACING_SAMPLE_RATE). Read-only after open.
	tracingSampleRate float64

	// OpenTelemetry export backend (WithTracer, RFC-115 §4 Layer 2). Never nil after
	// open — defaulted to a no-op tracer (C++ NoopTracer analog) so the span-emission
	// paths are unconditional and allocation-free when unset. Read-only after open.
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
	maxWatches         atomic.Int64

	// Lifecycle.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	connected chan struct{}
	wg        sync.WaitGroup
	// closeMu guards `closed`, the barrier that stops the lazy GRV-cache background refresher from
	// registering a wg slot once Close() has begun. Without it, the refresher's one-shot db.wg.Add(1)
	// (grv.go) can race Close's db.wg.Wait() at a zero counter → "sync: WaitGroup misuse: Add called
	// concurrently with Wait" panic. Close sets closed under this lock BEFORE cancel()/Wait(); the
	// refresher launch checks it under the same lock, so the check-and-Add is atomic w.r.t. the store.
	closeMu sync.Mutex
	closed  bool
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

// getGRVProxy returns a GRV proxy address using round-robin selection.
func (db *database) getGRVProxy() (*ProxyInfo, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.GRVProxies) == 0 {
		return nil, fmt.Errorf("no GRV proxies available")
	}
	idx := db.proxyRR.nextGRV(len(info.GRVProxies))
	return &info.GRVProxies[idx], nil
}

// getCommitProxy returns a commit proxy address using round-robin selection.
func (db *database) getCommitProxy() (*ProxyInfo, error) {
	info := db.dbInfo.Load()
	if info == nil || len(info.CommitProxies) == 0 {
		return nil, fmt.Errorf("no commit proxies available")
	}
	idx := db.proxyRR.nextCommit(len(info.CommitProxies))
	return &info.CommitProxies[idx], nil
}

// getGRVProxies returns all GRV proxies from the current topology.
func (db *database) getGRVProxies() []ProxyInfo {
	info := db.dbInfo.Load()
	if info == nil {
		return nil
	}
	return info.GRVProxies
}

// getCommitProxies returns all commit proxies from the current topology.
func (db *database) getCommitProxies() []ProxyInfo {
	info := db.dbInfo.Load()
	if info == nil {
		return nil
	}
	return info.CommitProxies
}

func (db *database) getOrDial(ctx context.Context, addr string) (*transport.Conn, error) {
	conn, _, err := db.getOrDialConn(ctx, addr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// handleDialError reacts to a getOrDial error. A context cancellation is the
// CALLER giving up, not the endpoint failing — evicting the pooled connection and
// marking the endpoint failed there would punish a healthy peer (and, in a
// concurrent cold dial, could drop the very connection a sibling caller just
// pooled). So only a genuine transport failure (ctx still live) feeds
// handleConnError.
func (db *database) handleDialError(ctx context.Context, addr string) {
	if ctx.Err() == nil {
		db.handleConnError(addr)
	}
}

// dialCall is one in-flight singleflight dial. Waiters block on done, then read
// conn/err — written by the dialer before close(done), so the channel close
// establishes the happens-before.
type dialCall struct {
	done chan struct{}
	conn *transport.Conn
	err  error
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
	// An already-canceled caller must not start (or coalesce onto) a dial.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	db.connMu.Lock()
	// Fast path: a live pooled connection.
	if c, ok := db.connPool[addr]; ok {
		if !c.IsClosed() {
			db.connMu.Unlock()
			return c, false, nil
		}
		delete(db.connPool, addr)
	}
	// Join the in-flight dial for addr, or start one (becoming its owner).
	call, owner := db.dialing[addr], false
	if call == nil {
		call = &dialCall{done: make(chan struct{})}
		db.dialing[addr] = call
		owner = true
	}
	db.connMu.Unlock()

	if owner {
		go db.dialAndPool(addr, call)
	}

	// Every caller — owner included — waits the same way: a caller's ctx cancels
	// only its own wait, never the shared dial.
	select {
	case <-call.done:
		if call.err != nil {
			return nil, false, call.err
		}
		return call.conn, owner, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
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
	// RFC-110 (codex P2): a panic in transport.Dial (or the pooling section)
	// skips the normal delete(db.dialing)+close(call.done) below, leaving the
	// singleflight entry for addr in db.dialing with call.done NEVER closed —
	// every later caller in getOrDialConn coalesces onto it and blocks until its
	// own ctx expires, and no fresh dial ever starts: a permanently poisoned
	// entry. The backstop publishes the failure so waiters wake and the next
	// caller re-dials. The connMu section below is closure-scoped so a panic in
	// it unwinds the lock (else this recover's Lock would deadlock).
	pb := &panicBackstop{name: "dialAndPool", db: db}
	defer func() {
		r := recover()
		if !pb.recovered(r) {
			return
		}
		db.connMu.Lock()
		delete(db.dialing, addr)
		db.connMu.Unlock()
		call.err = fmt.Errorf("fdbgo: panic dialing %s: %v", addr, r)
		close(call.done)
	}()

	dialCtx, cancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
	defer cancel() // RFC-110: release the dial timer even if transport.Dial panics
	c, dialErr := transport.Dial(dialCtx, addr, db.tlsConfig, db.dialFn)

	func() {
		db.connMu.Lock()
		defer db.connMu.Unlock()
		delete(db.dialing, addr)
		switch {
		case dialErr != nil:
			call.err = dialErr
		case db.ctx.Err() != nil:
			// The database closed while we dialed: Close() drains the pool under connMu,
			// so a conn pooled now would never be reaped (leaking its read/write/monitor
			// goroutines). Discard it and fail the call.
			call.err = db.ctx.Err()
			_ = c.Close()
		default:
			db.connPool[addr] = c
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
	}()
	close(call.done) // wake all waiters (conn/err already set under the lock)
}

// warmConnections pre-establishes TCP connections to all known proxies.
// Called after bootstrap to eliminate cold-start latency on first transaction.
// Dials GRV and commit proxies in parallel. Errors are silently ignored —
// connections will be established on demand if pre-warming fails.
func (db *database) warmConnections(ctx context.Context) {
	info := db.dbInfo.Load()
	if info == nil {
		return
	}

	// Collect unique addresses from all proxy types.
	seen := make(map[string]bool)
	var addrs []string
	for _, p := range info.GRVProxies {
		if !seen[p.Address] {
			seen[p.Address] = true
			addrs = append(addrs, p.Address)
		}
	}
	for _, p := range info.CommitProxies {
		if !seen[p.Address] {
			seen[p.Address] = true
			addrs = append(addrs, p.Address)
		}
	}

	// Dial all in parallel.
	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			db.getOrDial(ctx, a)
		}(addr)
	}
	wg.Wait()
}

// bootstrap connects to coordinators and fetches initial cluster topology.
// Tries all coordinators in parallel — first success wins. Retries with
// backoff on transient errors (e.g., failed_to_progress 1216).
func (db *database) bootstrap(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	for {
		snap := db.connRecord.get()
		info, err := db.tryAllCoordinators(ctx, snap)
		switch {
		case err == nil && info.Forward != "":
			// Path A: a coordinator forwarded us to a new set. Adopt + retry now.
			if db.followForward(snap, info.Forward) {
				continue
			}
			// self/empty/over-bound forward → fall through to backoff.
			err = fmt.Errorf("coordinator forward could not be followed")
		case err == nil:
			db.forwardHops = 0
			// New coordinators answered — persist a forward adopted in memory on a
			// previous iteration now that the set is confirmed reachable (Path A).
			db.connRecord.persistIfDirty()
			db.dbInfo.Store(info)
			close(db.connected)
			return nil
		default:
			// Path B: all coordinators unreachable — another process may have
			// rotated the set and rewritten the cluster file. Re-read it.
			if db.connRecord.adoptStoredIfChanged() {
				continue
			}
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("failed to connect to any coordinator: %w", err)
		case <-timer.C:
			if backoff < BootstrapMaxBackoff {
				backoff *= 2
			}
		}
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
func (db *database) tryAllCoordinators(ctx context.Context, snap *ClusterFile) (*DBInfo, error) {
	if snap == nil || len(snap.Coordinators) == 0 {
		// Defensive — production cluster-file parsing rejects empty
		// coordinator lists, but a hand-constructed *database in tests can
		// reach this path. Without the guard, the for-loop below returns
		// (nil, nil) and refreshTopology eventually nil-derefs in dbInfoEqual.
		return nil, fmt.Errorf("no coordinators configured")
	}
	if len(snap.Coordinators) == 1 {
		// Fast path: single coordinator, no goroutine overhead.
		return db.tryOneCoordinator(ctx, snap, snap.Coordinators[0])
	}

	type result struct {
		info *DBInfo
		err  error
	}
	ch := make(chan result, len(snap.Coordinators))
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, addr := range snap.Coordinators {
		go func(addr string) {
			info, err := db.tryOneCoordinator(raceCtx, snap, addr)
			ch <- result{info, err}
		}(addr)
	}

	var lastErr error
	for range snap.Coordinators {
		r := <-ch
		if r.err == nil {
			cancel() // cancel remaining attempts
			return r.info, nil
		}
		lastErr = r.err
	}
	return nil, lastErr
}

func (db *database) tryOneCoordinator(ctx context.Context, snap *ClusterFile, addr string) (info *DBInfo, err error) {
	// RFC-110 (codex P3): recover on the WORKER. tryAllCoordinators calls this
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
	conn, dialErr := db.getOrDial(ctx, addr)
	if dialErr != nil {
		return nil, dialErr
	}
	info, err = db.openDatabaseCoord(ctx, conn, snap, addr)
	if err != nil {
		return nil, fmt.Errorf("coordinator %s: %w", addr, err)
	}
	return info, nil
}

// Database is the public API entry point.
// C++: Database is a Reference<DatabaseContext> — a handle, not state.
//
// Safe for concurrent use by multiple goroutines.
type Database struct {
	db *database
}

// Metrics returns a point-in-time snapshot of this handle's operational
// counters (RFC-097) — the C++ DatabaseContext TransactionMetrics subset.
// Counters are monotonic; poll and diff for rates. This is the export hook:
// Prometheus/OTel consumers are pull-based readers of exactly this shape
// (see pkg/fdbgo/fdbmetrics for a ready-made scrape handler).
func (d *Database) Metrics() ClientMetricsSnapshot {
	return d.db.metrics.Snapshot()
}

// SetHedgeEnabled controls speculative second requests (hedge) for read RPCs.
// When enabled (default), slow reads are rescued by sending a backup request
// to a second server after max(10ms, 2×latency). Disable for debugging or
// benchmarking the non-hedged path.
func (d *Database) SetHedgeEnabled(enabled bool) {
	d.db.hedgeEnabled.Store(enabled)
}

// HedgeEnabled returns whether speculative second requests are active.
func (d *Database) HedgeEnabled() bool {
	return d.db.hedgeEnabled.Load()
}

// OpenDatabase opens a database connection using a cluster file.
// The provided ctx is used for the initial bootstrap (coordinator connection).
// Background goroutines use an internal context cancelled by Close().
func OpenDatabase(ctx context.Context, clusterFilePath string, opts ...Option) (*Database, error) {
	cf, err := ParseClusterFile(clusterFilePath)
	if err != nil {
		return nil, err
	}

	// Remember the file path so coordinator-set changes can be persisted back to it
	// (RFC-111). OpenDatabaseFromConfig (no path) is memory-only.
	opts = append(opts, withClusterFilePath(clusterFilePath))
	return OpenDatabaseFromConfig(ctx, cf, opts...)
}

// OpenDatabaseFromConfig creates and bootstraps a Database from a ClusterFile.
// See WithDialFunc / WithTLSConfig for options.
func OpenDatabaseFromConfig(ctx context.Context, cf *ClusterFile, opts ...Option) (*Database, error) {
	o := applyOptions(opts)

	// Mandatory-set API version (RFC-149): C++ cannot open a DB without
	// fdb_select_api_version, so "unset" never occurs there. Reject an unset
	// version here so the apiVersionAtLeast gate can never silently no-op.
	if o.apiVersion == 0 {
		return nil, &wire.FDBError{Code: 2200} // api_version_unset
	}

	// Resolve transport security (WithTLSConfig > ":tls"+FDB_TLS_* > plaintext).
	// A non-nil tlsConfig is the only "use TLS" signal. The default-config-dir
	// stat inside resolveTLSConfig is reached only for a TLS cluster, so a
	// plaintext open never touches /etc/foundationdb.
	tlsConfig, err := openTLSConfig(o, cf)
	if err != nil {
		return nil, err
	}

	logger := o.logger
	if logger == nil {
		logger = slog.Default()
	}

	bgCtx, cancel := context.WithCancel(context.Background())
	// Create the failure monitor first so the QueueModel can consult it for the
	// read load-balancing exclusion gate (RFC-115 §1; C++ loadBalance gates on
	// IFailureMonitor.failed before the QueueModel).
	failMon := newFailureMonitor()
	queueModel := newQueueModel()
	queueModel.failMon = failMon
	// Default to a no-op tracer (C++ NoopTracer) so span emission is unconditional and
	// zero-cost when WithTracer is unset (RFC-115 §4 Layer 2).
	tracer := o.tracer
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("fdbgo")
	}
	db := &database{
		connRecord:        newConnRecord(cf, o.clusterFilePath, logger),
		dialFn:            o.dialFn,
		tlsConfig:         tlsConfig,
		logger:            logger,
		rangeByteCeiling:  o.rangeByteCeiling,
		tracingSampleRate: o.tracingSampleRate,
		tracer:            tracer,
		apiVersion:        o.apiVersion,
		connPool:          make(map[string]*transport.Conn),
		dialing:           make(map[string]*dialCall),
		topologyKick:      make(chan struct{}, 1),
		proxiesChanged:    make(chan struct{}),
		connected:         make(chan struct{}),
		ctx:               bgCtx,
		cancel:            cancel,
		failMon:           failMon,
		queueModel:        queueModel,
		// hedgeEnabled default: set after struct init (atomic.Bool zero = false)
		locCache: locationCache{
			maxSize: 600_000,
		},
		grvBatchers: [3]*grvBatcher{
			grvBatcherBatch:           {batchTime: 1 * time.Millisecond, priority: grvPriorityBatch},
			grvBatcherDefault:         {batchTime: 1 * time.Millisecond, priority: grvPriorityDefault},
			grvBatcherSystemImmediate: {batchTime: 1 * time.Millisecond, priority: grvPrioritySystemImmediate},
		},
	}

	db.hedgeEnabled.Store(true)                       // default: hedge on (matches C++)
	db.maxWatches.Store(defaultMaxOutstandingWatches) // C++ DEFAULT_MAX_OUTSTANDING_WATCHES (10000)

	if err := db.bootstrap(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("connect to cluster: %w", err)
	}

	// Pre-warm connections to all known proxies. The first transaction
	// would dial lazily, but pre-warming saves ~5-10ms on first RPC.
	// Errors are ignored — connections will be established on demand.
	db.warmConnections(ctx)

	// Start topology monitor goroutine.
	db.wg.Add(1)
	go db.topologyMonitor()

	return &Database{db: db}, nil
}

// SetDialFunc sets a custom dialer for all new connections.
// Must be called before OpenDatabase or any transactions.
func (d *Database) SetDialFunc(fn transport.DialFunc) {
	d.db.dialFn = fn
}

// Transact runs a function in a transaction with automatic retry.
// This is the primary API for interacting with FDB.
// The transaction is reused across retries — retryCount and backoff escalate.
func (d *Database) Transact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
	tx := d.CreateTransaction()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		result, err := fn(tx)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr // non-retryable
			}
			continue // tx has been reset in place, retryCount/backoff preserved
		}

		// Honor a cancellation/deadline that arrived *during* fn before starting the
		// commit: skip creating the commit at all on an already-dead ctx. This is a
		// fast-abort, NOT the only GRV-abort point — the commit-path GRV inside Commit
		// (ensureReadVersion, transaction.go:1106) is itself ctx-bounded (RFC-093);
		// only the commit RPC + commit_unknown_result barrier are detached, inside
		// Commit (transaction.go:1126).
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// RFC-093: pass the LIVE ctx — the cancel/detach split lives inside Commit, not
		// here. A prior attempt that prefetched the commit read version in this loop
		// under the live ctx was reverted: done unconditionally it regresses Commit's
		// read-only/no-op fast path (which returns without a GRV when there are no
		// mutations or write-conflicts — transaction.go:1094), forcing an unnecessary
		// GRV RPC that can block/fail a no-op txn (codex P2); gating it on "has writes"
		// would duplicate Commit's gate under conflictMu. So Commit owns the split: it
		// threads this live ctx to its own ensureReadVersion (GRV cancellable, so a
		// cancel during the commit-path read version aborts promptly) and re-applies
		// WithoutCancel to ONLY the commit RPC + commit_unknown_result barrier
		// (transaction.go:1126). The retry backoff below still honors ctx via OnError.
		if err := tx.Commit(ctx); err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr
			}
			continue // for commit_unknown_result: self-conflicting applied
		}

		return result, nil
	}
}

// ReadTransact runs a read-only function in a transaction with automatic retry.
// The transaction is never committed — only reads are performed.
func (d *Database) ReadTransact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
	tx := d.CreateTransaction()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		result, err := fn(tx)
		if err != nil {
			if retryErr := tx.OnError(ctx, err); retryErr != nil {
				return nil, retryErr
			}
			continue
		}
		return result, nil
	}
}

// CreateTransaction creates a new transaction.
// Database-level defaults (timeout, retry limit, system key access) are applied.
func (d *Database) CreateTransaction() *Transaction {
	tx := &Transaction{
		db:           d.db,
		tenantId:     NoTenantID,
		creationTime: time.Now(),
	}
	// Apply database-level defaults (matches C++ applyTxDefaults).
	td := &d.db.txDefaults
	if td.Timeout > 0 {
		tx.SetTimeout(td.Timeout)
	}
	if td.HasRetryLimit {
		tx.SetRetryLimit(int64(td.RetryLimit))
	}
	if td.MaxRetryDelay > 0 {
		tx.SetMaxRetryDelay(td.MaxRetryDelay)
	}
	// C++ defaults EVERY transaction's sizeLimit to CLIENT_KNOBS->TRANSACTION_SIZE_LIMIT
	// (NativeAPI.actor.cpp:6133) — there is no "disabled" state: the SIZE_LIMIT option
	// floor is 32 and its default/ceiling is 10 MB. A 0/unset DB default must therefore
	// still enforce 10 MB, otherwise the Go client commits an oversized transaction that
	// libfdb_c rejects client-side with transaction_too_large (2101) — a wire divergence.
	// The DB option, when set (>0), lowers the limit.
	if td.SizeLimit > 0 {
		tx.SetSizeLimit(td.SizeLimit)
	} else {
		tx.SetSizeLimit(transactionSizeLimit)
	}
	if td.ReadSystemKeys {
		tx.SetReadSystemKeys()
	}
	if td.AccessSysKeys {
		tx.SetAccessSystemKeys()
	}
	// Generate the per-transaction trace span (RFC-115 §4) — a real, default-unsampled
	// SpanContext stamped on every request, matching C++ (which never sends a zero span).
	tx.regenerateSpan()
	return tx
}

// SetTransactionTimeout sets the default timeout (in milliseconds) for all
// transactions created from this database. Matches FDB_DB_OPTION_TRANSACTION_TIMEOUT.
func (d *Database) SetTransactionTimeout(ms int64) {
	d.db.txDefaults.Timeout = ms
}

// SetTransactionRetryLimit sets the default retry limit for all transactions.
// Matches FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT.
func (d *Database) SetTransactionRetryLimit(retries int64) {
	d.db.txDefaults.RetryLimit = int(retries)
	d.db.txDefaults.HasRetryLimit = true
}

// SetTransactionMaxRetryDelay sets the default max retry delay (ms) for all
// transactions. Matches FDB_DB_OPTION_TRANSACTION_MAX_RETRY_DELAY.
func (d *Database) SetTransactionMaxRetryDelay(ms int64) {
	d.db.txDefaults.MaxRetryDelay = ms
}

// SetTransactionSizeLimit sets the default size limit for all transactions.
// Matches FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT.
func (d *Database) SetTransactionSizeLimit(limit int64) {
	d.db.txDefaults.SizeLimit = limit
}

// SetDefaultReadSystemKeys makes all new transactions automatically call
// SetReadSystemKeys(), allowing reads of \xff-prefixed system keys.
func (d *Database) SetDefaultReadSystemKeys() {
	d.db.txDefaults.ReadSystemKeys = true
}

// SetDefaultAccessSystemKeys makes all new transactions automatically call
// SetAccessSystemKeys(), allowing reads AND writes of \xff-prefixed system keys.
func (d *Database) SetDefaultAccessSystemKeys() {
	d.db.txDefaults.AccessSysKeys = true
}

// SetMaxWatches sets the cap on concurrently-outstanding watches for this Database
// (FDB_DB_OPTION_MAX_WATCHES). Once the count would exceed it, a new watch fails with
// too_many_watches (1032). Matches C++ DatabaseContext maxOutstandingWatches; default 10000.
// 0 is a REAL cap (no watches allowed — the first fails), NOT "unlimited". An out-of-range value
// (< 0 or > ABSOLUTE_MAX_WATCHES) is REJECTED with invalid_option_value (2006) and the cap is left
// UNCHANGED — C++ extractIntOption(value, 0, ABSOLUTE_MAX_WATCHES) throws on out-of-range, it does
// NOT clamp (NativeAPI.actor.cpp:2092-2102 / :2139).
func (d *Database) SetMaxWatches(n int64) error {
	if n < 0 || n > absoluteMaxWatches {
		return &wire.FDBError{Code: 2006} // invalid_option_value; cap unchanged
	}
	d.db.maxWatches.Store(n)
	return nil
}

// InvalidateGRVCache resets the GRV cache so the next transaction fetches
// a fresh read version from the GRV proxy. Use after external writes.
func (d *Database) InvalidateGRVCache() {
	d.db.grvCache.version.Store(0)
}

// GetDBInfo returns the current cluster topology (proxy lists, cluster ID).
// Returns nil if not yet connected.
func (d *Database) GetDBInfo() *DBInfo {
	return d.db.dbInfo.Load()
}

// Close shuts down the database connection. Idempotent.
// Cancels background goroutines, waits for them to exit, closes all connections.
// registerBackgroundGoroutine reserves a db.wg slot for a lazily-started background goroutine (the
// GRV-cache refresher), returning true iff it was reserved. It returns FALSE once Close() has begun, so
// a slot is never Add()ed after Close's db.wg.Wait() is (or is about to be) waiting — which, once the
// topology-monitor slot has drained the counter toward zero, would panic with
// "sync: WaitGroup misuse: Add called concurrently with Wait". The closed check and the Add share
// closeMu with Close's `closed = true`, so the check-and-Add is atomic w.r.t. that store: either the
// Add happens before closed is set (so the counter is still ≥1 from the topology monitor and Close's
// Wait observes this slot) or closed is already set (so we skip). The caller's goroutine MUST
// db.wg.Done() on exit.
func (db *database) registerBackgroundGoroutine() bool {
	db.closeMu.Lock()
	defer db.closeMu.Unlock()
	if db.closed {
		return false
	}
	db.wg.Add(1)
	return true
}

func (d *Database) Close() error {
	d.db.closeOnce.Do(func() {
		// Set closed BEFORE cancel()/Wait() so a concurrent GRV-cache opt-in can't start the background
		// refresher (register a new wg slot) after we've begun waiting. The refresher launch checks
		// `closed` under closeMu and skips its Add when set (grv.go); taking the same lock here makes the
		// check-and-Add atomic w.r.t. this store, so the Add never races Wait at a zero counter.
		d.db.closeMu.Lock()
		d.db.closed = true
		d.db.closeMu.Unlock()
		d.db.cancel()
		d.db.wg.Wait()
		d.db.connMu.Lock()
		for addr, conn := range d.db.connPool {
			conn.Close()
			delete(d.db.connPool, addr)
		}
		d.db.connMu.Unlock()
	})
	return nil
}
