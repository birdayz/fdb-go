package transport

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/internal/hrtimer"
	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// replyChanPool pools chan Response to reduce allocations. A channel is
// returned to the pool only when no send can still race it: by the SUCCESS path
// (the caller received its reply, then calls ReplyHandle.Release / SendAndWait
// returns) or by Cancel when it won the delete race against readLoop. A channel
// whose delivery raced a cancel/timeout is deliberately NOT pooled — it may hold
// a stale buffered value, and a future request must never read it. readLoop does
// NOT return the channel (it only delivers); the earlier comment claiming it did
// was wrong and caused the success-path channel to leak.
var replyChanPool = sync.Pool{New: func() any { return make(chan Response, 1) }}

// ReplyHandle holds state for a pending reply. Pooled to avoid closure allocation
// in PrepareReply (the cancel closure captured conn+token+ch, causing a heap alloc
// on every RPC call — ~9% of total allocs in SaveRecord benchmarks).
type ReplyHandle struct {
	conn  *Conn
	token UID
	ch    chan Response
}

var replyHandlePool = sync.Pool{New: func() any { return &ReplyHandle{} }}

// Cancel removes the pending reply from the connection's pending map
// and returns the reply channel to the pool. Safe to call on a handle
// with a nil conn (no-op).
func (h *ReplyHandle) Cancel() {
	if h.conn == nil {
		return
	}
	h.conn.pendingMu.Lock()
	if _, ok := h.conn.pending[h.token]; ok {
		// Won the race: the token is still pending, so readLoop has not
		// delivered and never will (we delete it here under the same lock).
		// The channel is clean — pool it.
		delete(h.conn.pending, h.token)
		putReplyChannel(h.ch)
	}
	// Else readLoop already delivered (or is mid-delivery): h.ch may hold a
	// buffered value, so we must NOT pool it — leave it to GC. This rare
	// race-loser leak is the price of never handing a stale reply to a future
	// request.
	h.conn.pendingMu.Unlock()
	h.ch = nil // mark the channel handled so Release does not re-pool it
}

// TakeReadyOrCancel chooses response publication or cancellation under the
// connection's reply lock. The caller must exclusively own this handle and
// channel (no concurrent receive/Cancel/Release). Release is still required.
func (h *ReplyHandle) TakeReadyOrCancel() (Response, bool) {
	if h.conn == nil {
		return Response{}, false
	}
	h.conn.pendingMu.Lock()
	defer h.conn.pendingMu.Unlock()
	select {
	case response := <-h.ch:
		delete(h.conn.pending, h.token)
		return response, true
	default:
		if _, pending := h.conn.pending[h.token]; pending {
			delete(h.conn.pending, h.token)
			putReplyChannel(h.ch)
		}
		h.ch = nil
		return Response{}, false
	}
}

// KeepReadyOrCancel leaves a published response for the caller's normal receive,
// or cancels an unpublished reply atomically. Like TakeReadyOrCancel, this needs
// exclusive handle ownership. It never adds a second channel producer.
func (h *ReplyHandle) KeepReadyOrCancel() bool {
	if h.conn == nil {
		return false
	}
	h.conn.pendingMu.Lock()
	defer h.conn.pendingMu.Unlock()
	if len(h.ch) != 0 {
		return true
	}
	if _, pending := h.conn.pending[h.token]; pending {
		delete(h.conn.pending, h.token)
		putReplyChannel(h.ch)
	}
	h.ch = nil
	return false
}

// NotifyWait marks suspension only when the response is not already published.
// The caller owns the handle and has not received from its channel yet.
func (h *ReplyHandle) NotifyWait(ctx context.Context) {
	if len(h.ch) == 0 {
		NotifySuspend(ctx)
	}
}

// Release returns the handle to the pool. Call EITHER after a successful receive
// (no Cancel) OR after Cancel. On the success path h.ch is still set and is
// pooled here: response publication and token deletion share pendingMu, in that
// order. Once the caller receives the response, that publisher has no further
// channel send, and the lock excludes another publisher until deletion. Cancel
// nils h.ch, so the cancel path never double-pools.
func (h *ReplyHandle) Release() {
	if h.ch != nil {
		putReplyChannel(h.ch)
	}
	h.conn = nil
	h.ch = nil
	replyHandlePool.Put(h)
}

// errChanPool pools chan error for SendFrame/Flush synchronization.
var errChanPool = sync.Pool{New: func() any { return make(chan error, 1) }}

// Conn is a multiplexed FDB connection. Multiple concurrent requests
// share one TCP connection, matched by endpoint tokens.
//
// Lifecycle: after Close() returns, zero goroutines are running.
// If the server kills the connection, readLoop cancels the context
// and IsClosed() returns true — the connection pool will evict it.
type Conn struct {
	conn            net.Conn
	useTLS          bool
	wbuf            *bufio.Writer // owned exclusively by writeLoop
	writeCh         chan writeReq // channel-based write loop for coalescing
	writeAdmission  sync.RWMutex
	writeClosed     bool // protected by writeAdmission
	sendWaiters     sendqueue.Queue
	peerSendOrder   atomic.Pointer[sendqueue.Queue]
	prefixCh        chan writeReq
	heldCh          chan writeReq
	prefixDone      <-chan struct{}
	prefixOpen      bool // writer-owned after dial
	heldAdmission   sync.RWMutex
	heldClosed      bool // protected by heldAdmission
	heldWaiters     sendqueue.Queue
	heldCount       atomic.Int64 // buffered held frames and admission waiters
	coldControls    chan coldControl
	controlMu       sync.Mutex // pre-seal control reservation and inbox append
	controlsClosed  bool       // protected by writeAdmission
	pendingWrite    writeReq   // writer-owned ordinary request held behind the outbox
	hasPendingWrite bool
	ctx             context.Context
	cancel          context.CancelFunc
	loopWG          sync.WaitGroup // joins readLoop, writeLoop, connectionMonitor (and its ping reply), coldControlLoop
	closeOnce       sync.Once      // guards the single failConnection teardown

	// Connection monitor cadence. Set once at dial time before the monitor
	// goroutine starts. Tests inject small values for deterministic, fast
	// monitor-death assertions (see withMonitorCadence).
	//
	// These were MEASURED against flow/Knobs.cpp:102-103 and the reading held:
	// C++ declares each knob with a simulation value and a real one,
	//
	//	CONNECTION_MONITOR_LOOP_TIME  isSimulated ? 0.75 : 1.0
	//	CONNECTION_MONITOR_TIMEOUT    isSimulated ? 1.50 : 2.0
	//
	// and Go used to run 0.75s alongside 2s — the loop time's SIMULATED value
	// paired with the timeout's REAL one, a combination the C client never uses
	// in either mode. Both are now taken from the real column. C++ is the spec
	// for this package, so the mixed pair was a Go bug and not a tuning choice.
	//
	// Tests inject small values through withMonitorCadence rather than relying
	// on these, which is why tightening the loop to 1s does not slow any test.
	monitorLoopInterval time.Duration
	monitorTimeout      time.Duration

	// Typed pending map avoids sync.Map's interface boxing (saves 3 allocs/RPC).
	pendingMu sync.RWMutex
	pending   map[UID]pendingEntry
	peerPkt   *ConnectPacket // peer's connect packet

	// onEndpointNotFound observes every ENDPOINT_NOT_FOUND, waiter or not, as
	// C++ marks the endpoint failed regardless (FlowTransport.actor.cpp:227-233).
	onEndpointNotFound atomic.Pointer[func(UID)]

	// Connection monitor: counts bytes received so connectionMonitor() can
	// detect dead connections in ~2s (vs 10s TCP keepalive).
	// Matches C++ FlowTransport.actor.cpp connectionMonitor().
	bytesReceived atomic.Int64

	// monitorCycles counts completed connectionMonitor iterations. It exists so
	// the loop's PACING is observable: an idle connection must cost one cycle
	// per loop interval, and a version of this loop that reached its `continue`
	// without waiting spun hot and hung the package's test target at its
	// 300-second timeout rather than failing an assertion. A timeout is not a
	// regression test, so the rate is asserted directly.
	monitorCycles atomic.Int64

	// writeMu orders socket writes between writeLoop and direct writers; queued
	// counts frames handed to writeLoop and not yet flushed.
	writeMu sync.Mutex
	queued  atomic.Int64
	// raw is set for plaintext TCP only: a direct write needs a non-blocking
	// socket write, so a full socket never holds its caller past its ctx.
	raw       *rawWriter
	tail      []byte        // unwritten end of a direct write, under writeMu
	tailFrame []byte        // the whole frame tail ends, pooled once written
	tailBuf   *[]byte       // tailFrame's pool handle
	tailKick  chan struct{} // wakes writeLoop for a tail or held-outbox change

	// minCoalesce/maxCoalesce are connectionWriter's write delay bounds; coalesce
	// is its timer, nil when the delay is disabled or unsupported, in which case
	// an idle connection writes a frame on its sender's goroutine.
	minCoalesce, maxCoalesce time.Duration
	coalesce                 *hrtimer.Timer
	lastWrite                time.Time    // when wbuf last wrote to the socket; under writeMu
	tails                    atomic.Int64 // partial direct writes, so tests can tell a tail occurred

	// A sender may take the writer's turn (writer_turn.go). turnMu orders it
	// taking writeMu and starting to collect against writeLoop handing it frames.
	turnMu         sync.Mutex
	turnCollecting bool          // a turn is waiting out the delay; under turnMu
	turnFrames     []writeReq    // frames writeLoop handed to that turn; under turnMu
	tailMarkers    []writeReq    // flush markers acknowledged with the pending tail; under turnMu
	writerExited   bool          // writeLoop has failed tailMarkers and exited; under turnMu
	turnDelayLimit time.Duration // the longest delay a sender waits out itself
	turns          atomic.Int64  // turns taken, so tests can tell a sender wrote its batch

	// ng is set when the experimental network goroutine owns this connection's I/O.
	ng *ngConn
	// firstQueued is when the oldest frame not yet in a writeLoop batch was
	// queued (monoNow; 0 = none), for the FDBGO_COALESCE_FROM_SENDER experiment.
	firstQueued atomic.Int64

	// Debug tracing (set before first use).
	debugFrames bool
	debugWriter io.Writer
}

// writeReq is a frame queued for the write loop.
type writeReq struct {
	owned *writeCompletion // cancellable read request; separate from legacy errCh
	token UID
	body  []byte
	errCh chan<- error // nil = fire-and-forget (deferred writes)
}

// Response is a received message from the peer.
type Response struct {
	Body []byte
	Err  error
	// RecvAt is when the read loop delivered this reply (≈ arrival time). Set only
	// on a successful body delivery; lets a caller measure send→receive latency
	// independent of when it gets around to consuming the reply (RFC-114).
	RecvAt time.Time
}

// DialFunc is the signature for custom dialers. Same as net.Dialer.DialContext.
// Default is net.Dialer{}.DialContext. Override for testing (fault injection,
// custom Docker networking, traffic shaping).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Dial connects to an FDB process, exchanges ConnectPackets, and starts the
// read/write/monitor loops.
//
// tlsConfig is the single source of truth for transport security: non-nil wraps
// the connection in TLS using that standard *crypto/tls.Config (bring any config
// — in-memory certs, rotation via GetClientCertificate, custom
// VerifyPeerCertificate, cipher/version policy); nil means plaintext. There is no
// separate "use TLS" bool to disagree with it, so a connection can never be
// silently downgraded. dialFn overrides the dialer (nil → net.Dialer).
func Dial(ctx context.Context, addr string, tlsConfig *tls.Config, dialFn DialFunc, opts ...DialOption) (*Conn, error) {
	return dialWith(ctx, addr, dialFn, tlsConfig, opts...)
}

// ErrConnClosed is the identity sentinel for a connection teardown. Every error
// this package delivers for a torn-down connection matches it via
// errors.Is(err, ErrConnClosed).
//
// Exported so a caller can tell a TRANSPORT teardown apart from an answer the
// cluster actually gave. The delivered error additionally carries FDB code 1030
// request_maybe_delivered (see ConnClosedError), so downstream code has two
// structural handles: errors.Is against this sentinel while the transport error
// travels as-is, and the 1030 code once a facade has converted it to its own
// error type (which drops wrap-chain identity) — matching the rendered message
// is not one.
var ErrConnClosed = errors.New("connection closed")

// codeRequestMaybeDelivered is FDB error 1030 request_maybe_delivered
// (flow/error_definitions.h:57, release-7.3): "Request may or may not have been
// delivered". This is the code an in-flight unreliable RPC observes in C++ when
// its peer connection dies: tryGetReply waits on
// IFailureMonitor::onDisconnectOrFailure and completes with
// request_maybe_delivered() (fdbrpc/include/fdbrpc/fdbrpc.h:794-799 and
// :815-820), and the load balancers classify broken_promise and
// request_maybe_delivered as one maybeDelivered class
// (fdbrpc/include/fdbrpc/LoadBalance.actor.h:344 for reads, :823-824 for GRV and
// commit) — retrying another alternative for reads, which go through
// loadBalance at AtMostOnce::False, and throwing request_maybe_delivered for a
// commit, which goes through basicLoadBalance at AtMostOnce::True
// (LoadBalance.actor.h:828-830, called from fdbclient/NativeAPI.actor.cpp:6638).
// tryCommit maps that to commit_unknown_result handling. Reads and commits take
// DIFFERENT load balancers; attributing the commit arm to loadBalance names a
// function the commit path never calls.
const codeRequestMaybeDelivered = 1030

// ConnClosedError is the teardown error delivered to in-flight requests and
// queued senders when the connection is torn down (by Close, the monitor, a
// read error, or a write error). It matches BOTH errors.Is(err, ErrConnClosed)
// (teardown identity) and errors.As(err, **wire.FDBError) with code 1030
// request_maybe_delivered, so the client's retry machinery sees the same coded,
// C++-faithful condition libfdb_c's loadBalance sees. Cause (the underlying
// read/write error, when there is one) is carried in the message only — NOT in
// the Unwrap chain — so a teardown can never spuriously match errors.Is/As
// probes aimed at its cause (e.g. a context error check).
type ConnClosedError struct {
	cause error // optional underlying read/write error; message-only
}

func (e *ConnClosedError) Error() string {
	if e.cause != nil {
		return "connection closed (request_maybe_delivered): " + e.cause.Error()
	}
	return "connection closed (request_maybe_delivered)"
}

func (e *ConnClosedError) Unwrap() []error {
	return []error{ErrConnClosed, &wire.FDBError{Code: codeRequestMaybeDelivered}}
}

// errConnClosed is the shared no-cause teardown value. A single value (not a
// fresh allocation per site) so the cheap hot paths (SendFrame/Flush ctx.Done
// arms) stay allocation-free.
var errConnClosed error = &ConnClosedError{}

// codedConnClosed wraps a raw teardown cause (a readLoop/writeLoop I/O error, a
// recovered loop panic) into the coded ConnClosedError. Idempotent: an error
// that is already a teardown error passes through unchanged.
func codedConnClosed(err error) error {
	if err == nil || errors.Is(err, ErrConnClosed) {
		if err == nil {
			return errConnClosed
		}
		return err
	}
	return &ConnClosedError{cause: err}
}

// DialOption configures a Conn before its loop goroutines start.
type DialOption func(*Conn)

// MIN_COALESCE_DELAY and MAX_COALESCE_DELAY (flow/Knobs.cpp:211-212).
const (
	DefaultMinCoalesceDelay = 10 * time.Microsecond
	DefaultMaxCoalesceDelay = 20 * time.Microsecond
)

// WithCoalesceDelay sets how long the connection waits before writing a batch,
// so frames sent meanwhile share it: max(min, max - time since the last write),
// jittered by 0.9-1.1, as libfdb_c's connectionWriter does
// (FlowTransport.actor.cpp:704-710). The defaults are libfdb_c's knobs. Zero for
// both writes a frame at once when nothing is queued ahead of it. The wait needs
// a sub-millisecond timer: off Linux, or if the timer cannot be created, frames
// are written at once.
func WithCoalesceDelay(minDelay, maxDelay time.Duration) DialOption {
	return func(c *Conn) {
		c.minCoalesce, c.maxCoalesce = max(minDelay, 0), max(maxDelay, 0)
	}
}

// withMonitorCadence overrides the connection-monitor loop interval and timeout.
// Test-only: lets the monitor-death path fire in tens of ms instead of ~3.5s.
func withMonitorCadence(loop, timeout time.Duration) DialOption {
	return func(c *Conn) {
		c.monitorLoopInterval = loop
		c.monitorTimeout = timeout
	}
}

// defaultHandshakeTimeout bounds the TLS upgrade + ConnectPacket exchange when the
// caller's ctx carries no deadline. The handshake is normally sub-second; this is a
// generous ceiling so a stalled peer can't wedge the dial forever.
const defaultHandshakeTimeout = 10 * time.Second

// dialWith is the implementation behind Dial.
func dialWith(ctx context.Context, addr string, dialFn DialFunc, tlsConfig *tls.Config, opts ...DialOption) (*Conn, error) {
	if dialFn == nil {
		var d net.Dialer
		dialFn = d.DialContext
	}
	netConn, err := dialFn(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// Disable Nagle's algorithm. FDB sends many small frames (Get/Set requests)
	// that must reach the server without 40ms coalescing delay.
	// C++ FlowTransport sets TCP_NODELAY on all connections.
	if tc, ok := netConn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)

		// Send RST (not FIN) on close to avoid TIME_WAIT state.
		// Without this, the OS may reuse the ephemeral source port while
		// the FDB server still has a stale Peer entry for the old connection,
		// triggering an ASSERT at FlowTransport.actor.cpp:1569.
		// RST causes immediate Peer cleanup on the server side.
		tc.SetLinger(0)

		// Detect dead connections faster than read timeouts.
		// Under Docker/CI load, socat proxies can silently drop connections.
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(10 * time.Second)
	}

	// Bound the handshake (TLS upgrade + ConnectPacket exchange) with a deadline.
	// A peer that accepts the TCP socket but never completes the handshake — an
	// overloaded fdbserver, or a Docker/socat proxy that half-opens the connection
	// under load — would otherwise block ReadConnectPacket's io.ReadFull forever:
	// ctx cancellation cannot interrupt a blocking socket read, and this dial runs
	// under the database's global connection lock, so a single wedged handshake
	// freezes every connection acquisition (the load-dependent chaos-test deadlock).
	// Derive the deadline from ctx so an explicit dial timeout finally bounds the
	// handshake too; fall back to defaultHandshakeTimeout. Cleared before the I/O
	// loops start — liveness past that point is the connectionMonitor's job. The
	// deadline is set on the raw TCP conn and cleared on the (possibly TLS-wrapped)
	// conn; tls.Conn.SetDeadline delegates to the same underlying socket.
	handshakeDeadline := time.Now().Add(defaultHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(handshakeDeadline) {
		handshakeDeadline = d
	}
	_ = netConn.SetDeadline(handshakeDeadline)

	// Honor ctx *cancellation*, not just its deadline, for the whole handshake.
	// A cancel-only ctx (no deadline) would otherwise block on the socket I/O up
	// to handshakeDeadline, since a blocking Read/Write cannot observe ctx itself.
	// The watcher pushes the deadline into the past on cancellation, unblocking
	// the in-flight TLS handshake AND ConnectPacket exchange immediately. It targets
	// the raw TCP conn (a stable handle the TLS wrapper delegates SetDeadline to),
	// so it stays race-free after netConn is reassigned to the TLS conn below.
	rawConn := netConn
	handshakeDone := make(chan struct{})
	var watcherWG sync.WaitGroup
	watcherWG.Add(1)
	go func() {
		defer watcherWG.Done()
		select {
		case <-ctx.Done():
			_ = rawConn.SetDeadline(time.Now())
		case <-handshakeDone:
		}
	}()
	var stopOnce sync.Once
	stopWatcher := func() {
		stopOnce.Do(func() { close(handshakeDone) })
		watcherWG.Wait()
	}
	// Idempotent; covers every error return below. On the success path it is also
	// called explicitly before the deadline is cleared (see below).
	defer stopWatcher()

	// handshakeErr prefers the ctx error for a handshake I/O failure: when the
	// watcher above aborts the handshake on cancellation, the read/write returns an
	// i/o-timeout-shaped error (from the past deadline), but the caller actually
	// cancelled — so surface ctx.Err() (wrapped, so errors.Is(context.Canceled /
	// DeadlineExceeded) holds). A genuine timeout with a live ctx keeps the i/o error.
	handshakeErr := func(stage string, ioErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%s: %w", stage, ctxErr)
		}
		return fmt.Errorf("%s: %w", stage, ioErr)
	}

	// Wrap in TLS iff a config was supplied. The non-nil config is the only
	// "use TLS" signal — there is no bool to disagree with it, so plaintext can
	// never be sent on a connection the caller wanted encrypted. An empty config
	// still attempts a real handshake and fails closed (never plaintext).
	if tlsConfig != nil {
		tlsConn, tlsErr := upgradeTLS(netConn, addr, tlsConfig)
		if tlsErr != nil {
			netConn.Close()
			return nil, handshakeErr(fmt.Sprintf("TLS handshake %s", addr), tlsErr)
		}
		netConn = tlsConn
	}

	connCtx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		conn:    netConn,
		useTLS:  tlsConfig != nil,         // drives frame-checksum omission; not a TLS switch
		writeCh: make(chan writeReq, 256), // buffered for concurrent senders
		pending: make(map[UID]pendingEntry, 16),
		ctx:     connCtx,
		cancel:  cancel,
		// C++ CONNECTION_MONITOR_LOOP_TIME / CONNECTION_MONITOR_TIMEOUT, both
		// taken from the NON-simulated column of flow/Knobs.cpp:102-103:
		//
		//	init( CONNECTION_MONITOR_LOOP_TIME, isSimulated ? 0.75 : 1.0 );
		//	init( CONNECTION_MONITOR_TIMEOUT,   isSimulated ? 1.50 : 2.0 );
		monitorLoopInterval: 1 * time.Second,
		monitorTimeout:      2 * time.Second,
		tailKick:            make(chan struct{}, 1),
		minCoalesce:         DefaultMinCoalesceDelay,
		maxCoalesce:         DefaultMaxCoalesceDelay,
		turnDelayLimit:      maxTurnDelay,
	}
	c.wbuf = bufio.NewWriterSize(stampedWriter{c}, 64*1024)
	if tlsConfig == nil {
		c.raw = newRawWriter(netConn)
	}
	// Apply test-only knobs BEFORE any loop goroutine starts (no data race).
	for _, o := range opts {
		o(c)
	}

	// Exchange ConnectPackets.
	connID := newConnectionID()
	if err := WriteConnectPacket(netConn, netConn.LocalAddr(), connID); err != nil {
		cancel()
		netConn.Close()
		return nil, handshakeErr("write connect packet", err)
	}

	peerPkt, err := ReadConnectPacket(netConn)
	if err != nil {
		cancel()
		netConn.Close()
		return nil, handshakeErr("read connect packet", err)
	}

	if !peerPkt.IsCompatible(ProtocolVersion73) {
		cancel()
		netConn.Close()
		peerVer := peerPkt.ProtocolVersion & ^ObjectSerializerFlag
		return nil, fmt.Errorf("incompatible protocol version: peer=%#x, ours=%#x", peerVer, ProtocolVersion73)
	}

	c.peerPkt = peerPkt

	// Enable debug tracing via environment variable.
	if os.Getenv("FDB_DEBUG_FRAMES") != "" {
		c.debugFrames = true
		c.debugWriter = os.Stderr
	}

	// Handshake complete. Stop the cancellation watcher and wait for it to exit
	// BEFORE clearing the deadline: from here the conn's lifetime is governed by
	// connCtx, so a late dial-ctx cancellation must not push a past deadline onto
	// the now-live socket. (stopWatcher is idempotent; the deferred call no-ops.)
	stopWatcher()

	// Clear the deadline so the long-lived I/O loops run without one
	// (connectionMonitor + TCP keepalive handle liveness from here).
	_ = netConn.SetDeadline(time.Time{})

	if netgEnabled && tlsConfig == nil {
		c.loopWG.Add(3) // the network goroutine's registration, connectionMonitor, readLoop
		if netgRegister(c) {
			if netgWriter {
				go c.readLoop()
			} else {
				c.loopWG.Done()
			}
			go c.connectionMonitor()
			return c, nil
		}
		c.loopWG.Add(-3)
	}

	if c.minCoalesce > 0 || c.maxCoalesce > 0 {
		if t, err := hrtimer.New(); err == nil {
			c.coalesce = t // closed by failConnection and writeLoop
		}
	}

	c.lastWrite = time.Now() // connectionWriter's initial lastWriteTime

	// Start read, write, and connection monitor loops.
	c.loopWG.Add(3)
	if c.coldControls != nil {
		c.loopWG.Add(1)
		go c.coldControlLoop()
	}
	go c.readLoop()
	go c.writeLoop()
	go c.connectionMonitor()

	return c, nil
}

// Send sends a request and returns a channel that will receive the response.
// The destToken identifies the remote endpoint (e.g., a StorageServer's getValue endpoint).
// The replyToken is a fresh token for routing the response back.
func (c *Conn) Send(destToken UID, body []byte) (replyToken UID, replyCh <-chan Response, err error) {
	return c.sendInternal(destToken, body)
}

// sendInternal is Send returning the bidirectional reply channel so internal
// callers (SendAndWait) can return it to the pool. Send exposes only the
// receive-only view.
func (c *Conn) sendInternal(destToken UID, body []byte) (replyToken UID, replyCh chan Response, err error) {
	token := NewUID()
	ch := getReplyChannel()
	c.pendingMu.Lock()
	c.pending[token] = pendingEntry{ch: ch, dest: destToken}
	c.pendingMu.Unlock()
	transferred := false
	defer func() {
		if !transferred {
			c.cancelPending(token, ch)
		}
	}()

	if err = c.SendFrame(destToken, body); err != nil {
		return UID{}, nil, fmt.Errorf("write frame: %w", err)
	}

	transferred = true
	return token, ch, nil
}

// cancelPending removes a still-pending reply token and returns its channel to
// the pool iff it won the race against readLoop's delivery — the same discipline
// as ReplyHandle.Cancel, for the raw token+channel SendAndWait holds.
func (c *Conn) cancelPending(token UID, ch chan Response) {
	c.pendingMu.Lock()
	if _, ok := c.pending[token]; ok {
		delete(c.pending, token)
		putReplyChannel(ch)
	}
	c.pendingMu.Unlock()
}

// PrepareReply allocates a reply token and registers it for response routing.
// Use this when you need the token BEFORE building the request body
// (e.g., to embed it in the FDB request's Reply field).
//
// Returns a ReplyHandle whose Cancel() removes the pending token. Call
// handle.Release() when done to return it to the pool.
func (c *Conn) PrepareReply() (UID, <-chan Response, *ReplyHandle) {
	return c.PrepareReplyTo(UID{})
}

// PrepareReplyTo is PrepareReply for a request that will be sent to dest, so an
// ENDPOINT_NOT_FOUND for dest fails this reply instead of leaving it to time out.
func (c *Conn) PrepareReplyTo(dest UID) (UID, <-chan Response, *ReplyHandle) {
	h := replyHandlePool.Get().(*ReplyHandle)
	h.conn = c
	h.token = NewUID()
	h.ch = getReplyChannel()
	c.pendingMu.Lock()
	c.pending[h.token] = pendingEntry{ch: h.ch, dest: dest}
	c.pendingMu.Unlock()
	return h.token, h.ch, h
}

// HasPendingReply reports whether token still owns a reply registration.
func (c *Conn) HasPendingReply(token UID) bool {
	c.pendingMu.RLock()
	_, pending := c.pending[token]
	c.pendingMu.RUnlock()
	return pending
}

// getReplyChannel gets a reply channel from the pool.
func getReplyChannel() chan Response {
	return replyChanPool.Get().(chan Response)
}

// putReplyChannel returns a reply channel to the pool after draining it.
// It is a var (defaulting to putReplyChannelImpl) so the pool-discipline tests
// can observe exactly which channels are returned to the pool, on which path,
// without depending on sync.Pool's non-deterministic reuse.
var putReplyChannel = putReplyChannelImpl

func putReplyChannelImpl(ch chan Response) {
	// Drain any buffered value (shouldn't normally happen).
	select {
	case <-ch:
	default:
	}
	replyChanPool.Put(ch)
}

// SendFrame writes a raw frame and flushes. Returns once the frame is written, or
// once writeLoop holds whatever a full socket refused, ahead of every later frame.
func (c *Conn) SendFrame(destToken UID, body []byte) error {
	if c.ng != nil {
		_, err := c.ngWrite(c.ctx, destToken, body, false, nil)
		return err
	}
	admission, err := c.beginSend(c.ctx, nil)
	if err != nil {
		return err
	}
	defer admission.permit.Release()
	if c.debugFrames {
		fmt.Fprintf(c.debugWriter, "[send %s] token=%016x:%016x bodyLen=%d\n", c.conn.RemoteAddr(),
			destToken.First, destToken.Second, len(body))
	}
	LogSend(destToken, body)
	if admission.ordinary() {
		if direct, err := c.tryWriteDirect(destToken, body); direct {
			return err
		}
		if took, err := c.takeTurn(c.ctx, destToken, body); took {
			return err
		}
	}
	errCh := errChanPool.Get().(chan error)
	if ok, _ := c.enqueueAdmitted(c.ctx, writeReq{token: destToken, body: body, errCh: errCh}, nil, admission); !ok {
		errChanPool.Put(errCh)
		return errConnClosed
	}
	// Wait for writeLoop to write+flush, OR bail if the connection is torn down
	// (Close/monitor/read-error cancels ctx). Without the ctx.Done arm a sender
	// whose frame is still queued when writeLoop exits would block on errCh
	// forever. On the ctx.Done path errCh is deliberately NOT returned to the
	// pool: writeLoop may still hold a reference and send to it, which would
	// surface as a stale buffered value on the next pool user.
	select {
	case err := <-errCh:
		errChanPool.Put(errCh)
		return err
	case <-c.ctx.Done():
		return errConnClosed
	}
}

// SendFrameDeferred writes a raw frame WITHOUT waiting for flush.
// The write loop will flush it with the next batch. Used for pipelining:
// send N frames, then call Flush() (or let the write loop auto-flush).
func (c *Conn) SendFrameDeferred(destToken UID, body []byte) error {
	if c.ng != nil {
		_, err := c.ngWrite(c.ctx, destToken, body, false, nil)
		return err
	}
	admission, err := c.beginSend(c.ctx, nil)
	if err != nil {
		return err
	}
	defer admission.permit.Release()
	LogSend(destToken, body)
	if ok, _ := c.enqueueAdmitted(c.ctx, writeReq{token: destToken, body: body}, nil, admission); !ok {
		return errConnClosed
	}
	return nil
}

// Flush ensures all pending frames are flushed to the TCP socket. With nothing
// queued every earlier frame is already written; otherwise it sends a flush
// marker through the write channel and waits for acknowledgment.
func (c *Conn) Flush() error {
	if c.ng != nil {
		_, err := c.ngWrite(c.ctx, UID{}, nil, true, nil)
		return err
	}
	admission, err := c.beginSend(c.ctx, nil)
	if err != nil {
		return err
	}
	defer admission.permit.Release()
	if admission.ordinary() && c.queued.Load() == 0 && c.ctx.Err() == nil {
		return nil
	}
	errCh := errChanPool.Get().(chan error)
	if ok, _ := c.enqueueAdmitted(c.ctx, writeReq{errCh: errCh}, nil, admission); !ok { // empty token+body = flush-only request
		errChanPool.Put(errCh)
		return errConnClosed
	}
	// Bail on connection teardown rather than block forever on errCh (see SendFrame).
	// errCh is not pooled on the ctx.Done path (stale-value hazard).
	select {
	case err := <-errCh:
		errChanPool.Put(errCh)
		return err
	case <-c.ctx.Done():
		return errConnClosed
	}
}

// coalesceWait is connectionWriter's
// delayJittered(max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - sinceWrite))
// (FlowTransport.actor.cpp:708-710; DELAY_JITTER_OFFSET/RANGE 0.9/0.2,
// flow/Knobs.cpp:54-55). failConnection closes the timer, which ends the wait.
func (c *Conn) coalesceWait(sinceWrite time.Duration) {
	c.coalesceWaitSince(sinceWrite, 0)
}

// coalesceWaitSince is coalesceWait for a batch whose first frame was queued at
// queuedAt (monoNow, 0 = unknown): the wait is shortened by the time it spent queued.
func (c *Conn) coalesceWaitSince(sinceWrite time.Duration, queuedAt int64) {
	if c.coalesce == nil {
		return
	}
	d := coalesceDelay(c.minCoalesce, c.maxCoalesce, sinceWrite, mathrand.Float64())
	if queuedAt != 0 {
		if d -= time.Duration(monoNow() - queuedAt); d <= 0 {
			return
		}
	}
	if c.coalesce.Reset(d) == nil {
		_ = c.coalesce.Wait()
	}
}

// stampedWriter is wbuf's socket: connectionWriter stamps lastWriteTime just
// before each connection write (FlowTransport.actor.cpp:716-718), including a
// batch that spills the buffer.
type stampedWriter struct{ c *Conn }

func (w stampedWriter) Write(p []byte) (int, error) {
	w.c.lastWrite = time.Now()
	return w.c.conn.Write(p)
}

// CoalesceDelay reports the connection's write delay bounds and whether it waits.
func (c *Conn) CoalesceDelay() (minDelay, maxDelay time.Duration, waits bool) {
	return c.minCoalesce, c.maxCoalesce, c.coalesce != nil || (c.ng != nil && (c.minCoalesce > 0 || c.maxCoalesce > 0))
}

// coalesceDelay is the jittered wait for a uniform jitter in [0, 1).
func coalesceDelay(minDelay, maxDelay, sinceWrite time.Duration, jitter float64) time.Duration {
	d := max(minDelay, maxDelay-sinceWrite)
	return time.Duration(float64(d) * (0.9 + 0.2*jitter))
}

// writeLoop is the dedicated write goroutine. It reads frames from writeCh,
// writes them to the buffered writer, and flushes. After the first frame it
// waits the coalescing delay, then drains all other queued frames before
// flushing — so N concurrent SendFrame/SendFrameDeferred calls share one
// flush, as connectionWriter writes everything unsent after its delay
// (FlowTransport.actor.cpp:704-732). It holds writeMu from before the delay
// to the flush, so neither a direct writer nor a sender's turn interleaves a batch.
func (c *Conn) writeLoop() {
	defer c.loopWG.Done()
	if c.coalesce != nil {
		defer c.coalesce.Close()
	}
	var owned []*writeCompletion
	var inBatch int64
	defer func() {
		c.queued.Add(-inBatch)
		c.finishOwnedWrites(owned)
	}()
	defer c.failTailMarkers()
	defer c.recoverLoop("writeLoop")

	// Collect errCh channels that need notification after flush.
	var errChans []chan<- error

	for {
		// The finite cold prefix precedes held frames, including any held
		// admission head not yet in its channel, then ordinary warm traffic.
		req, first, ready := c.nextWrite(true)
		if !ready {
			return
		}
		if first {
			inBatch = 1
			if req.owned != nil {
				owned = append(owned, req.owned)
			}
		}
		if !c.lockForBatch(req, first) {
			inBatch = 0
			clear(owned)
			owned = owned[:0]
			continue // a sender's turn now owns this request and its completion
		}
		if first && c.tail == nil {
			// The rest of a partial write goes on without a new delay, as
			// connectionWriter keeps writing until nothing is unsent
			// (FlowTransport.actor.cpp:714-731).
			var queuedAt int64
			if coalesceFromSender {
				queuedAt = c.firstQueued.Swap(0)
			}
			c.coalesceWaitSince(time.Since(c.lastWrite), queuedAt)
		}
		if c.ctx.Err() != nil {
			c.writeMu.Unlock()
			return // torn down while waiting: write nothing
		}
		var writeErr error
		// A direct write's unwritten end precedes every frame queued after it.
		if c.tail != nil {
			inBatch++
			_, writeErr = c.wbuf.Write(c.tail)
			putFrame(c.tailBuf, c.tailFrame) // wbuf has copied or written it
			c.tail, c.tailFrame, c.tailBuf = nil, nil, nil
			// Flush markers a turn left behind the tail complete with this batch.
			for _, m := range c.takeTailMarkers() {
				inBatch++
				if m.owned != nil {
					owned = append(owned, m.owned)
				}
				if m.errCh != nil {
					errChans = append(errChans, m.errCh)
				}
			}
		}
		// Process first frame.
		if first {
			if writeErr == nil && (req.token != (UID{}) || req.body != nil) {
				writeErr = WriteFrame(c.wbuf, req.token, req.body, c.useTLS)
			}
			if req.errCh != nil {
				errChans = append(errChans, req.errCh)
			}
		}
		if inBatch == 0 { // an outbox wake, or an already-written tail's stale kick
			c.writeMu.Unlock()
			continue
		}

		// Do not wait here for an unbuilt prefix member or held head: flush
		// this available batch before the next blocking priority read.
		for writeErr == nil {
			var available bool
			req, _, available = c.nextWrite(false)
			if !available {
				break
			}
			inBatch++
			if req.owned != nil {
				owned = append(owned, req.owned)
			}
			if req.token != (UID{}) || req.body != nil {
				writeErr = WriteFrame(c.wbuf, req.token, req.body, c.useTLS)
			}
			if req.errCh != nil {
				errChans = append(errChans, req.errCh)
			}
		}

		// Flush all accumulated frames.
		if writeErr == nil {
			writeErr = c.wbuf.Flush()
		}
		if writeErr != nil {
			// Fail under writeMu so no direct write can follow a torn frame.
			writeErr = codedConnClosed(writeErr)
			c.failConnection(writeErr)
		}
		c.queued.Add(-inBatch)
		inBatch = 0
		if coalesceFromSender {
			c.firstQueued.Store(0) // frames queued during this batch went out with it
		}
		c.writeMu.Unlock()

		// Notify all waiting senders. A write error means the frame may or may
		// not have reached the peer before the socket died — code it as the
		// request_maybe_delivered teardown (C++ has no synchronous send errors
		// at all; a failed send there surfaces only as the disconnect signal →
		// request_maybe_delivered, fdbrpc.h:794-799).
		for _, ch := range errChans {
			ch <- writeErr
		}
		errChans = errChans[:0]
		for i, completion := range owned {
			owned[i] = nil // recovery must not complete an already-released owner
			completion.complete(writeErr)
		}
		owned = owned[:0]
		if writeErr != nil {
			// The socket writer is terminal after any frame/flush failure; the
			// deferred ownership finalizer drains requests outside this batch.
			return
		}
	}
}

// SendAndWait sends a request and blocks until the response arrives.
func (c *Conn) SendAndWait(ctx context.Context, destToken UID, body []byte) ([]byte, error) {
	replyToken, replyCh, err := c.sendInternal(destToken, body)
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-replyCh:
		// Received: readLoop deleted the token before delivering, so no further
		// send can race — return the channel to the pool.
		putReplyChannel(replyCh)
		return resp.Body, resp.Err
	case <-ctx.Done():
		// Timed out: remove the still-pending token; pool the channel only if we
		// beat readLoop's delivery (cancelPending), else leave it to GC.
		c.cancelPending(replyToken, replyCh)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		// Connection teardown: failAllPending owns the pending entry and may be
		// delivering an error to replyCh concurrently — leave the channel to GC.
		return nil, errConnClosed
	}
}

// Close closes the connection and waits for its loop goroutines to exit.
// Safe to call multiple times. After Close returns, zero goroutines
// are running on this connection.
//
// Shutdown sequence (matches C++ connectionKeeper):
//  1. Cancel context — signals all waiters that conn is dead
//  2. Close TCP socket — unblocks readLoop's ReadFrame
//  3. failConnection delivers errors to all pending requests
//  4. readLoop, writeLoop, connectionMonitor and coldControlLoop exit and
//     signal loopWG; the monitor first releases its PING reply (RFC-269)
//  5. Close returns
func (c *Conn) Close() error {
	c.failConnection(errConnClosed)
	c.loopWG.Wait()
	// Always nil: the socket close happens inside failConnection, whose error is
	// not actionable (closing an already-dead/closed conn yields a spurious
	// "use of closed connection"; C++ likewise does not surface it). The previous
	// implementation returned conn.Close()'s error here — callers that checked it
	// will now always see nil.
	return nil
}

// failConnection is the single connection-teardown path (C++ connectionKeeper):
// cancel the context (unblocks SendFrame/Flush/SendAndWait selects), close the
// socket (unblocks readLoop's blocking Read so the fd + goroutine can't leak),
// and deliver err to every in-flight reply. sync.Once makes the trio run exactly
// once with the FIRST caller's error, no matter how many of {Close, monitor
// death, readLoop error} fire. Single-delivery to a given pending reply is
// guaranteed by failAllPending's own pendingMu + delete-as-you-go, not by the
// Once; the Once only ensures the meaningful error wins over a later
// "use of closed connection" read error.
//
// Callable from a loop goroutine (readLoop/monitor): the Once body never touches
// loopWG, and only Close() waits on loopWG — after failConnection returns — so
// there is no self-deadlock.
func (c *Conn) failConnection(err error) {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.ng != nil {
			c.ngClose()
		}
		if c.coalesce != nil {
			// Wakes a writeLoop waiting to write, as cancelling connectionWriter
			// cancels its delay (FlowTransport.actor.cpp:882-883).
			_ = c.coalesce.Close()
		}
		_ = c.conn.Close()
		// Code the fan-out: whatever raw cause killed the connection (EOF,
		// ECONNRESET, a recovered loop panic), every in-flight reply observes a
		// coded request_maybe_delivered teardown — the exact condition C++'s
		// waitValueOrSignal delivers on peer disconnect (fdbrpc.h:794-799) —
		// never a bare I/O error the retry machinery cannot classify.
		c.failAllPending(codedConnClosed(err))
	})
}

// seriousLog reports an unexpected, must-not-be-silent event (a recovered panic in
// the connection's background goroutines) at ERROR level through log/slog, passing
// structured attributes rather than a pre-formatted string so a JSON/structured
// handler gets queryable fields. Routing through slog.Default() makes these
// diagnostics pluggable via the standard Go mechanism (slog.SetDefault) with no
// fdbgo-specific logging API. A var so tests can capture it.
var seriousLog = func(msg string, attrs ...any) {
	slog.Default().Error(msg, attrs...)
}

// recoverLoop contains a panic in a long-lived connection goroutine. A malformed
// frame or an internal bug must fail THIS connection — callers recover via retry /
// wrong-shard reroute, exactly as libfdb_c turns a bad reply into connection_failed
// — and must never crash the whole host: a recover in a request goroutine cannot
// catch a panic raised on a different goroutine's stack. debug.Stack here still
// shows the original panic site (the stack stays live until recover completes the
// unwind). See TODO-production.md P0.2.
func (c *Conn) recoverLoop(which string) {
	if r := recover(); r != nil {
		err := fmt.Errorf("fdbgo: panic in %s goroutine: %v", which, r)
		seriousLog("recovered panic in connection goroutine",
			"goroutine", which, "err", err, "stack", string(debug.Stack()))
		c.failConnection(err)
	}
}

// SetDebug enables frame-level debug tracing to stderr.
func (c *Conn) SetDebug(enabled bool) {
	c.debugFrames = enabled
	c.debugWriter = os.Stderr
}

// BytesReceived returns the socket bytes received since the handshake, frame
// headers and partial frames included. The connection monitor watches it.
func (c *Conn) BytesReceived() int64 {
	return c.bytesReceived.Load()
}

// IsClosed returns true if the connection has been closed or the server
// killed it. Uses context cancellation — works for both shutdown paths.
func (c *Conn) IsClosed() bool {
	return c.ctx.Err() != nil
}

// Done returns a channel that is closed when the connection is dead.
// Use in select statements to detect connection death.
func (c *Conn) Done() <-chan struct{} {
	return c.ctx.Done()
}

// PeerProtocolVersion returns the peer's protocol version.
func (c *Conn) PeerProtocolVersion() uint64 {
	return c.peerPkt.ProtocolVersion & ^ObjectSerializerFlag
}

// readLoop reads frames and dispatches responses to waiting goroutines.
//
// Two exit paths:
// 1. Client calls Close() → conn.Close() → ReadFrame returns error → we exit
// 2. Server dies → ReadFrame returns EOF → we cancel ctx → pool sees IsClosed()
//
// Both paths: deliver errors to all pending, signal WaitGroup, return.
func (c *Conn) readLoop() {
	// Teardown on ANY exit — the normal read-error path AND an unexpected panic in
	// frame parsing, which is now actually recovered below. (Previously the defer
	// ran on panic but did NOT call recover, so a malformed frame propagated and
	// crashed the whole host.) The single failConnection path (cancel + close
	// socket + fail all pending; C++ disconnect-promise equivalent) is idempotent,
	// so if Close/monitor already fired this is a no-op and the first caller's error
	// wins. exitErr carries the real read error, or the panic, when there is one.
	exitErr := errConnClosed
	defer c.loopWG.Done()
	defer func() {
		if r := recover(); r != nil {
			exitErr = fmt.Errorf("fdbgo: panic in readLoop goroutine: %v", r)
			seriousLog("recovered panic in readLoop goroutine",
				"err", exitErr, "stack", string(debug.Stack()))
		}
		c.failConnection(exitErr)
	}()

	pingToken := WellKnownToken(WLTokenPingPacket)
	var fr FrameReader
	// C++ connectionReader reads into a packet buffer: one read usually carries
	// whole frames, where length, checksum and payload reads cost three each.
	in := bufio.NewReaderSize(socketCounter{c.conn, &c.bytesReceived}, readBufferSize)

	for {
		token, body, err := fr.Read(in, c.useTLS)
		if err != nil {
			// Only log if this is unexpected (server died, not our Close).
			if c.debugFrames && c.ctx.Err() == nil {
				fmt.Fprintf(c.debugWriter, "[recv] ERROR: %v\n", err)
			}
			exitErr = err
			return
		}

		LogRecv(token, body)

		if c.debugFrames {
			c.pendingMu.RLock()
			_, isPending := c.pending[token]
			c.pendingMu.RUnlock()
			fmt.Fprintf(c.debugWriter, "[recv %s] token=%016x:%016x bodyLen=%d ping=%v pending=%v\n", c.conn.RemoteAddr(),
				token.First, token.Second, len(body), token == pingToken, isPending)
		}

		// Handle PING requests from the server.
		if token == pingToken {
			c.handlePing(body)
			continue
		}
		if token == endpointNotFoundToken {
			c.handleEndpointNotFound(body)
			continue
		}

		// Look up the pending request by reply token.
		c.pendingMu.Lock()
		entry, ok := c.pending[token]
		if ok {
			// The fresh size-one channel has one producer. Publish together
			// with deletion so cancellation cannot fall into a delivery gap.
			entry.ch <- Response{Body: body, RecvAt: time.Now()}
			delete(c.pending, token)
		}
		c.pendingMu.Unlock()
		// Unknown tokens are silently dropped (e.g., late responses after timeout).
	}
}

// socketCounter counts bytes as the socket delivers them, as C++ connectionReader
// does (peer->bytesReceived += totalReadBytes), so a large frame in transit is progress.
type socketCounter struct {
	r io.Reader
	n *atomic.Int64
}

func (s socketCounter) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n.Add(int64(n))
	return n, err
}

// readBufferSize exceeds C++ MIN_PACKET_BUFFER_BYTES (4 KiB, flow/Knobs.cpp:226)
// so a burst of replies drains in one read; larger payloads bypass it.
const readBufferSize = 16 << 10

// failAllPending delivers an error to all pending request channels.
// Matches C++ connectionKeeper's disconnect promise that wakes all
// in-flight deliver() actors.
func (c *Conn) failAllPending(err error) {
	c.pendingMu.Lock()
	for token, entry := range c.pending {
		select {
		case entry.ch <- Response{Err: err}:
		default:
		}
		delete(c.pending, token)
	}
	c.pendingMu.Unlock()
}

// WLTokenPingPacket is the well-known token for PING keepalive.
const WLTokenPingPacket = 1

// handlePing responds to a PingRequest from the server.
// PingRequest has file_identifier 4707015 and a single field: ReplyPromise<Void>.
// The reply token is a 16-byte UID embedded in the body. We extract it and
// send back a minimal ErrorOr<Void> reply.
func (c *Conn) handlePing(body []byte) {
	replyToken, ok := extractPingReplyToken(body)
	if !ok {
		return
	}

	replyBody := buildVoidReply()

	// Send ping reply through the write loop. Fire-and-forget (no errCh).
	// A full write channel drops the ping reply; the server will retry.
	c.tryEnqueue(writeReq{token: replyToken, body: replyBody})
}

// extractPingReplyToken extracts the reply UID from a PingRequest FlatBuffers body.
func extractPingReplyToken(body []byte) (UID, bool) {
	if len(body) < 40 {
		return UID{}, false
	}

	r, err := wire.NewReader(body)
	if err != nil {
		return UID{}, false
	}

	// Object serialization wraps ReplyPromise's inline UID in a nested table.
	if !r.FieldPresent(0) {
		return UID{}, false
	}

	nestedR, err := r.ReadNestedReader(0)
	if err != nil {
		return UID{}, false
	}

	// Read UID inline at the nested struct's field 0.
	if !nestedR.FieldPresent(0) {
		return UID{}, false
	}
	off := nestedR.FieldOffset(0)
	obj := nestedR.ObjectBytes()
	if off+16 > len(obj) {
		return UID{}, false
	}
	uid := UID{
		First:  binary.LittleEndian.Uint64(obj[off:]),
		Second: binary.LittleEndian.Uint64(obj[off+8:]),
	}
	return uid, true
}

func buildVoidReply() []byte {
	return (&types.VoidReply{}).MarshalFDB()
}

// connectionMonitor detects dead connections by sending outbound PINGs.
// Matches C++ FlowTransport.actor.cpp connectionMonitor() (lines 621-702):
//
// Outer loop:
//  1. Sleep CONNECTION_MONITOR_LOOP_TIME (1s), jittered — the client's ONLY
//     per-cycle delay; C++'s other one at :631 is server-only
//  2. If no pending requests → skip the PING and loop (see the divergence note
//     at the check itself)
//  3. Send PING
//  4. Inner loop: wait CONNECTION_MONITOR_TIMEOUT (2s) per round
//     - If bytesReceived unchanged → kill connection
//     - If bytesReceived changed → update baseline, continue inner loop
//     - If PING reply arrives → break (connection alive)
//
// The inner retry loop is key: a single 2s timeout with no bytes kills,
// but if ANY bytes arrive (server PINGs, other responses), the baseline
// resets and we wait another 2s. This tolerates slow-but-alive connections.
// jitteredDelay ports flow's delayJittered (flow.h:1481):
//
//	seconds * (DELAY_JITTER_OFFSET + DELAY_JITTER_RANGE * random01())
//
// with the knob values from Knobs.cpp:54-55 — offset 0.9, range 0.2 — so the
// result is uniform over [0.9d, 1.1d). Both constants are named rather than
// folded into a single ±10% because they are two independent knobs in C++ and a
// future divergence should show up against the knob it came from.
const (
	delayJitterOffset = 0.9
	delayJitterRange  = 0.2
)

func jitteredDelay(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (delayJitterOffset + delayJitterRange*mathrand.Float64()))
}

func (c *Conn) connectionMonitor() {
	defer c.loopWG.Done()
	defer c.recoverLoop("connectionMonitor")

	for {
		// ONE jittered delay per cycle, and it is the client's only one.
		//
		// C++'s FIRST `delay(CONNECTION_MONITOR_LOOP_TIME)`
		// (FlowTransport.actor.cpp:631) sits inside a block guarded by
		// `!FlowTransport::isClient() && !peer->destination.isPublic()` (:624),
		// so a CLIENT never executes it. The client path is delay(0) (:647),
		// the idle/unreferenced checks (:649-660), then ONE delayJittered
		// (:662), the PING, and delay(CONNECTION_MONITOR_TIMEOUT) (:672).
		//
		// Go used to sleep here AS WELL as below, porting the server-only
		// delay, which put worst-case dead-connection detection at
		// 2*loop+timeout instead of loop+timeout. That is why correcting the
		// knob VALUE alone made things worse rather than better: at the real
		// 1.0s the doubled loop moved the worst case to 4.0s, further from the
		// C client's 3.0s than the 3.5s the wrong value happened to give.
		//
		// The delay sits BEFORE the idle check rather than after it, unlike
		// C++. That is forced by the divergence noted below: Go's idle branch
		// loops instead of throwing, so a delay placed only on the ping path
		// would leave the idle path spinning hot.
		//
		// JITTERED, matching `delayJittered` at :662. Not cosmetic — without it
		// every connection a client holds pings on the same period, so a process
		// that opened many at once keeps them in lockstep and the PINGs arrive
		// as a synchronized burst.
		select {
		case <-time.After(jitteredDelay(c.monitorLoopInterval)):
		case <-c.ctx.Done():
			return
		}

		// C++ checks peer->reliable.empty() && peer->unsent.empty() &&
		// peer->outstandingReplies == 0 (:649).
		//
		// KNOWN DIVERGENCE, pre-existing and not introduced here: on that
		// branch C++ either THROWS — connection_unreferenced (:653) or
		// connection_idle (:658, past CONNECTION_MONITOR_IDLE_TIMEOUT = 180s) —
		// or falls through and PINGs anyway. Go instead skips the ping and
		// loops, so it never pings an idle connection and never closes one:
		// a dead idle socket goes undetected until TCP keepalive. Closing that
		// gap needs both missing knobs (IDLE_TIMEOUT and
		// UNREFERENCED_CLOSE_DELAY) and a peer-reference count Go does not
		// track, so it is a separate change with its own client-review lap.
		c.monitorCycles.Add(1)

		c.pendingMu.RLock()
		hasPending := len(c.pending) > 0
		c.pendingMu.RUnlock()
		if !hasPending {
			continue
		}

		switch c.monitorPing() {
		case pingStop:
			return
		case pingDead:
			// monitorPing released the reply first, as the C++ actor destroys its
			// state before publishing connection_failed (ActorCompiler.cs:387-391).
			c.failConnection(errConnClosed)
			return
		}
	}
}

// pingOutcome is how one monitor PING wait ended.
type pingOutcome uint8

const (
	pingContinue pingOutcome = iota // reply, or a dropped PING with traffic still flowing
	pingStop                        // the connection context was cancelled
	pingDead                        // no bytes arrived within the timeout
)

// monitorPing sends one PING and waits for its reply (FlowTransport.actor.cpp:665-699).
// It owns the reply as C++ actor state does, releasing it on the joined monitor goroutine.
func (c *Conn) monitorPing() pingOutcome {
	replyCh, reply := c.sendPingWithReply()
	received := false
	if reply != nil {
		defer func() { releasePing(reply, received) }()
	}
	// A dropped PING has no handle and a nil replyCh, which is never selected.
	// Wait monitorTimeout per round. Kill only if bytesReceived is truly frozen.
	startingBytes := c.bytesReceived.Load()
	for {
		timer := time.NewTimer(c.monitorTimeout)
		select {
		case <-replyCh:
			// PING reply arrived (C++ :690-695), or failAllPending's teardown
			// response, after which the next wait sees the cancelled context.
			received = true
			timer.Stop()
			return pingContinue
		case <-timer.C:
			// Timeout. Check if ANY bytes arrived.
			current := c.bytesReceived.Load()
			if current == startingBytes {
				// No bytes at all since PING was sent — connection is dead.
				// C++ :674-679: throw connection_failed.
				return pingDead
			}
			// Bytes arrived (server PINGs, other traffic) but not our PING reply.
			// C++ :687-688: update baseline, loop again.
			// C++ uses timeouts counter here only for logging (ConnectionSlowPing),
			// NOT for kill decisions — the kill condition is solely bytesReceived.
			startingBytes = current
			if replyCh == nil {
				// The PING was never sent (writeCh was saturated), so no reply can
				// arrive on this nil channel — the wait has no success path.
				// We just confirmed bytes are still flowing, so the connection is
				// NOT frozen; staying here would wait forever for an impossible
				// reply and kill a healthy-but-quiet connection on the next idle
				// window (failing slow-but-live requests). Return to the outer loop
				// to re-attempt the PING next cycle, by which point writeCh has
				// likely drained. C++ never hits this — Peer::send is unbounded, so
				// the ping always goes out; this is the bounded-writeCh fallback.
				return pingContinue
			}
		case <-c.ctx.Done():
			timer.Stop()
			return pingStop
		}
	}
}

// releasePing ends the PING reply's registration. TakeReadyOrCancel takes a reply
// published after the wait ended (Close's teardown when the ctx arm won), so it is pooled.
func releasePing(reply *ReplyHandle, received bool) {
	if !received {
		reply.TakeReadyOrCancel()
	}
	reply.Release()
}

// sendPingWithReply registers and enqueues a PING, returning its reply channel and
// handle for releasePing; both are nil when the PING could not be enqueued.
func (c *Conn) sendPingWithReply() (<-chan Response, *ReplyHandle) {
	replyToken, replyCh, replyHandle := c.PrepareReply()
	transferred := false
	defer func() {
		if !transferred {
			replyHandle.Cancel()
			replyHandle.Release()
		}
	}()
	pingEP := WellKnownToken(WLTokenPingPacket)
	body := BuildPingRequest(replyToken)
	if !c.tryEnqueue(writeReq{token: pingEP, body: body}) {
		// writeCh is saturated — we could not send the PING. Return a nil
		// channel, NOT a closed one. A closed channel makes the monitor's
		// reply arm fire immediately and falsely conclude "connection
		// alive", skipping the bytesReceived liveness check exactly when a
		// saturated buffer signals a stuck connection (writeLoop blocked on an
		// undrained socket). A nil channel is never selected, so the monitor
		// falls through to the timer → bytesReceived check and kills a genuinely
		// stuck conn. The pending reply is cancelled and released here (no leak),
		// so there is no handle to return.
		return nil, nil
	}
	transferred = true
	return replyCh, replyHandle
}

// pingRequestFileID is from C++ fdbrpc/FlowTransport.actor.cpp.
const pingRequestFileID uint32 = 4707015

// pingRequestVTable: PingRequest has 1 field (ReplyPromise/RelativeOffset, align 4).
// vtable[0] = 2*1+4 = 6 (vtable wire size)
// vtable[1] = 4+4 = 8 (object size with soffset)
// vtable[2] = 4 (field 0 offset)
var pingRequestVTable = wire.VTable{6, 8, 4}

var pingRequestVTableClosure = []wire.VTable{
	{6, 8, 4}, // PingRequest
	types.ReplyPromiseVTable,
	{6, 8, 4}, // FakeRoot
}

var pingRequestTemplate = wire.NewMessageTemplate(
	pingRequestFileID, pingRequestVTable, 4, pingRequestVTableClosure,
)

// BuildPingRequest builds a PingRequest FlatBuffers body.
// ReplyPromise's object-serializer trait emits a nested table with an inline UID.
func BuildPingRequest(replyToken UID) []byte {
	return wire.MarshalDirect(
		pingRequestTemplate,
		func(endOff int) int {
			return wire.MeasureObject(endOff, types.ReplyPromiseVTable, types.ReplyPromiseMaxAlign)
		},
		func(dw *wire.DirectWriter) int {
			replyPos, reply := dw.WriteObject(types.ReplyPromiseVTable, types.ReplyPromiseMaxAlign)
			tokenOff := int(types.ReplyPromiseVTable[types.ReplyPromiseSlotToken+2])
			binary.LittleEndian.PutUint64(reply[tokenOff:], replyToken.First)
			binary.LittleEndian.PutUint64(reply[tokenOff+8:], replyToken.Second)

			// Write the PingRequest object.
			objPos, obj := dw.WriteObject(pingRequestVTable, 4)

			// Field 0: RelativeOffset to the ReplyPromise table.
			fieldOff := int(pingRequestVTable[2]) // offset 4
			wire.PatchRelOff(obj, fieldOff, objPos, replyPos)

			return objPos
		},
	)
}

// fastRNGState is the shared, atomically advanced PRNG state for UID generation.
// Uses SplitMix64 (same as java.util.SplittableRandom). Seeded from crypto/rand.
// Reply tokens only need uniqueness within a connection, not crypto security.
var fastRNGState atomic.Uint64

func init() {
	var buf [8]byte
	rand.Read(buf[:])
	fastRNGState.Store(binary.LittleEndian.Uint64(buf[:]))
}

// splitmix64 is a fast 64-bit PRNG. Period: 2^64.
func splitmix64() uint64 {
	// Atomic add gives each caller a unique state progression.
	s := fastRNGState.Add(0x9e3779b97f4a7c15)
	s = (s ^ (s >> 30)) * 0xbf58476d1ce4e5b9
	s = (s ^ (s >> 27)) * 0x94d049bb133111eb
	return s ^ (s >> 31)
}

// NewUID generates a random 128-bit UID for endpoint tokens.
// Uses a fast non-crypto PRNG — UIDs are for reply routing, not security.
func NewUID() UID {
	return UID{
		First:  splitmix64(),
		Second: splitmix64(),
	}
}

func newConnectionID() uint64 {
	// Connection IDs use crypto/rand for true uniqueness across processes.
	var buf [8]byte
	rand.Read(buf[:])
	return binary.LittleEndian.Uint64(buf[:])
}

// upgradeTLS wraps conn in TLS using the caller's *tls.Config. The config is
// cloned so the caller's value is never mutated; we fill in two FDB-shaped
// defaults ONLY when the caller left them unset — ServerName (from the dialed
// host, for SNI + verification) and MinVersion (TLS 1.2). Everything else
// (certs, RootCAs, GetClientCertificate, VerifyPeerCertificate, cipher suites)
// is the caller's to control — this is a plain *crypto/tls.Config.
func upgradeTLS(conn net.Conn, addr string, cfg *tls.Config) (net.Conn, error) {
	cfg = cfg.Clone()
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			cfg.ServerName = host
		}
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	return tlsConn, nil
}
