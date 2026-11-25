package transport

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"fdb.dev/internal/fdbgo/hrtimer"
	"fdb.dev/internal/fdbwire"
)

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

// ConnectFailedError is a failed connect to a peer, as a request waiting on that
// connect observes it. C++ connectionKeeper turns any connect or handshake failure,
// including its CONNECTION_MONITOR_TIMEOUT, into connection_failed
// (FlowTransport.actor.cpp:819-869, Net2.actor.cpp:363-391 and 413-430), discards the peer's
// unsent unreliable packets and fires peer->disconnect (FlowTransport.actor.cpp:933,
// 1001-1004). Every tryGetReply queued on the peer then completes with
// request_maybe_delivered (fdbrpc.h:797-816, genericactors.actor.h:380-385), whether
// or not its packet ever left the client. So this matches errors.As(err,
// **wire.FDBError) with code 1030 and nothing else: the dial error is message-only,
// so a dial timeout cannot match a caller's context probes.
type ConnectFailedError struct {
	Addr  string
	cause error
}

// NewConnectFailedError codes cause, the error of a failed dial to addr.
func NewConnectFailedError(addr string, cause error) *ConnectFailedError {
	return &ConnectFailedError{Addr: addr, cause: cause}
}

func (e *ConnectFailedError) Error() string {
	return "connect to " + e.Addr + " failed (request_maybe_delivered): " + e.cause.Error()
}

func (e *ConnectFailedError) Unwrap() error {
	return &wire.FDBError{Code: codeRequestMaybeDelivered}
}

// Cause returns the dial error, for diagnostics.
func (e *ConnectFailedError) Cause() error { return e.cause }

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

// WithCoalesceDelay sets the connection's timer-based batch delay bounds (default
// 0, 0 on D-NG; 10/20 µs on WithLegacyWriter(); the last linger/coalesce option wins).
func WithCoalesceDelay(minDelay, maxDelay time.Duration) DialOption {
	return func(c *Conn) {
		c.minCoalesce, c.maxCoalesce = max(minDelay, 0), max(maxDelay, 0)
		c.adaptiveLinger = 0
		c.coalesceSet = true
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

// configureTCPConn applies low-latency socket options for FDB traffic.
func configureTCPConn(netConn net.Conn) {
	// Disable Nagle's algorithm. FDB sends many small frames (Get/Set requests)
	// that must reach the server without 40ms coalescing delay.
	// C++ FlowTransport sets TCP_NODELAY on all connections.
	tc, ok := netConn.(*net.TCPConn)
	if !ok {
		return
	}
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

// startHandshakeCancelWatcher pushes rawConn's deadline into the past if ctx is
// cancelled before the handshake completes, unblocking blocking Read/Write calls.
func startHandshakeCancelWatcher(ctx context.Context, rawConn net.Conn) func() {
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
	return func() {
		stopOnce.Do(func() { close(handshakeDone) })
		watcherWG.Wait()
	}
}

// handshakeError prefers the ctx error for a handshake I/O failure: when the
// cancel watcher aborts the handshake on cancellation, the read/write returns an
// i/o-timeout-shaped error (from the past deadline), but the caller actually
// cancelled — so surface ctx.Err() (wrapped, so errors.Is(context.Canceled /
// DeadlineExceeded) holds). A genuine timeout with a live ctx keeps the i/o error.
func handshakeError(ctx context.Context, stage string, ioErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", stage, ctxErr)
	}
	return fmt.Errorf("%s: %w", stage, ioErr)
}

// newDialedConn allocates and configures a Conn before ConnectPacket exchange.
func newDialedConn(netConn net.Conn, tlsConfig *tls.Config, opts []DialOption) (*Conn, string) {
	connCtx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		conn:    netConn,
		useTLS:  tlsConfig != nil,         // drives frame-checksum omission; not a TLS switch
		writeCh: make(chan writeReq, 256), // buffered for concurrent senders
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
		turnDelayLimit:      maxTurnDelay,
	}
	c.nextToken.Store(splitmix64())
	for i := range c.pendingShards {
		c.pendingShards[i].m = make(map[UID]pendingEntry, 4)
	}
	c.wbuf = bufio.NewWriterSize(stampedWriter{c}, 64*1024)
	if tlsConfig == nil {
		c.raw = newRawWriter(netConn)
	}
	// Apply test-only knobs BEFORE any loop goroutine starts (no data race).
	for _, o := range opts {
		o(c)
	}
	design := c.design
	if design == "" {
		design = envDesign
	}
	if !c.coalesceSet {
		if design == "" || design == designLegacy || design == designD {
			c.minCoalesce = DefaultMinCoalesceDelay
			c.maxCoalesce = DefaultMaxCoalesceDelay
		}
	}
	return c, design
}

// exchangeConnectPackets performs the FDB ConnectPacket handshake and validates protocol compatibility.
func exchangeConnectPackets(ctx context.Context, netConn net.Conn) (*ConnectPacket, error) {
	connID := newConnectionID()
	if err := WriteConnectPacket(netConn, netConn.LocalAddr(), connID); err != nil {
		return nil, handshakeError(ctx, "write connect packet", err)
	}

	peerPkt, err := ReadConnectPacket(netConn)
	if err != nil {
		return nil, handshakeError(ctx, "read connect packet", err)
	}

	if !peerPkt.IsCompatible(ProtocolVersion73) {
		peerVer := peerPkt.ProtocolVersion & ^ObjectSerializerFlag
		return nil, fmt.Errorf("incompatible protocol version: peer=%#x, ours=%#x", peerVer, ProtocolVersion73)
	}

	return peerPkt, nil
}

// startConnLoops initializes writer state and launches the connection's background goroutines.
func startConnLoops(c *Conn, design string) {
	if c.minCoalesce > 0 || c.maxCoalesce > 0 {
		if t, err := hrtimer.New(); err == nil {
			c.coalesce = t // closed by failConnection and writeLoop
		}
	}

	c.lastWrite = time.Now() // connectionWriter's initial lastWriteTime
	if design == designD || design == designNG {
		c.na = &naWriter{
			q:        make([]byte, 0, 64*1024),
			sig:      make(chan struct{}, 1),
			delay:    c.coalesce != nil,
			adaptive: c.adaptiveLinger,
		}
	}

	// Start read, write, and connection monitor loops.
	c.loopWG.Add(3)
	if c.coldControls != nil {
		c.loopWG.Add(1)
		go c.coldControlLoop()
	}
	if h := c.hooks.Load(); h != nil && h.beforeReadLoop != nil {
		h.beforeReadLoop()
	}
	go c.readLoop()
	if c.na != nil {
		go c.naLoop()
	} else {
		go c.writeLoop()
	}
	go c.connectionMonitor()
}

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

	configureTCPConn(netConn)

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
	// Idempotent; covers every error return below. On the success path it is also
	// called explicitly before the deadline is cleared (see below).
	stopWatcher := startHandshakeCancelWatcher(ctx, netConn)
	defer stopWatcher()

	// Wrap in TLS iff a config was supplied. The non-nil config is the only
	// "use TLS" signal — there is no bool to disagree with it, so plaintext can
	// never be sent on a connection the caller wanted encrypted. An empty config
	// still attempts a real handshake and fails closed (never plaintext).
	if tlsConfig != nil {
		tlsConn, tlsErr := upgradeTLS(netConn, addr, tlsConfig)
		if tlsErr != nil {
			netConn.Close()
			return nil, handshakeError(ctx, fmt.Sprintf("TLS handshake %s", addr), tlsErr)
		}
		netConn = tlsConn
	}

	c, design := newDialedConn(netConn, tlsConfig, opts)

	// Exchange ConnectPackets.
	peerPkt, err := exchangeConnectPackets(ctx, netConn)
	if err != nil {
		c.cancel()
		netConn.Close()
		return nil, err
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

	startConnLoops(c, design)
	return c, nil
}

// upgradeTLS wraps conn in TLS using the caller's *tls.Config. The config is
// cloned so the caller's value is never mutated; we fill in two FDB-shaped
// defaults ONLY when the caller left them unset — ServerName (from the dialed
// host: SNI, and the stdlib's host check unless a VerifyPolicy config replaces
// it) and MinVersion (TLS 1.2). Everything else
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
