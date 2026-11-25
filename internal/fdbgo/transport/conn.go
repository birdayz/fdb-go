package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/internal/fdbgo/hrtimer"
	"fdb.dev/internal/fdbgo/sendqueue"
)

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

	// Sharded typed pending maps avoid both sync.Map's interface boxing and
	// single-lock contention across concurrent senders and readLoop.
	pendingShards [numPendingShards]pendingShard
	nextToken     atomic.Uint64
	peerPkt       *ConnectPacket // peer's connect packet

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

	// Legacy writer state (WithLegacyWriter): writeMu orders socket writes between
	// writeLoop and direct writers; queued counts frames in writeLoop.
	writeMu sync.Mutex
	queued  atomic.Int64
	// raw is set for plaintext TCP only: a legacy direct write needs a non-blocking
	// socket write, so a full socket never holds its caller past its ctx.
	raw       *rawWriter
	tail      []byte        // unwritten end of a direct write, under writeMu
	tailFrame []byte        // the whole frame tail ends, pooled once written
	tailBuf   *[]byte       // tailFrame's pool handle
	tailKick  chan struct{} // wakes writeLoop for a tail or held-outbox change

	// minCoalesce/maxCoalesce are connectionWriter's write delay bounds; coalesce
	// is its timer (nil when disabled or unsupported). lastWrite is owned by
	// naLoop on D-NG connections, or guarded by writeMu on WithLegacyWriter().
	minCoalesce, maxCoalesce time.Duration
	coalesce                 *hrtimer.Timer
	lastWrite                time.Time
	tails                    atomic.Int64 // partial direct writes, so tests can tell a tail occurred

	// Legacy writer-turn state (writer_turn.go, WithLegacyWriter): turnMu orders a
	// sender taking writeMu and collecting frames from writeLoop.
	turnMu         sync.Mutex
	turnCollecting bool          // a turn is waiting out the delay; under turnMu
	turnFrames     []writeReq    // frames writeLoop handed to that turn; under turnMu
	tailMarkers    []writeReq    // flush markers acknowledged with the pending tail; under turnMu
	writerExited   bool          // writeLoop has failed tailMarkers and exited; under turnMu
	turnDelayLimit time.Duration // the longest delay a sender waits out itself
	turns          atomic.Int64  // turns taken, so tests can tell a sender wrote its batch

	// na is the enqueue-only D-NG writer (na_writer.go).
	na              *naWriter
	design          string // dial-option override of envDesign; see na_writer.go
	adaptiveLinger  time.Duration
	coalesceSet     bool
	coldOrderActive atomic.Bool
	hooks           atomic.Pointer[connTestHooks]

	// Debug tracing (set before first use).
	debugFrames bool
	debugWriter io.Writer
}

type connTestHooks struct {
	beforeReadLoop    func() // optional test hook run in dialWith just before readLoop starts
	beforeControlLock func() // optional test hook run in tryEnqueueNAControl before controlMu.Lock
	putReplyChannel   func(chan Response)
	seriousLog        func(msg string, attrs ...any)
}

func (c *Conn) testHooks() *connTestHooks {
	if h := c.hooks.Load(); h != nil {
		return h
	}
	h := &connTestHooks{}
	if c.hooks.CompareAndSwap(nil, h) {
		return h
	}
	return c.hooks.Load()
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

// Close closes the connection and waits for its loop goroutines to exit.
// Safe to call multiple times. After Close returns, zero goroutines
// are running on this connection.
//
// Shutdown sequence (matches C++ connectionKeeper):
//  1. Cancel context — signals all waiters that conn is dead
//  2. Close TCP socket — unblocks readLoop's ReadFrame
//  3. failConnection delivers errors to all pending requests
//  4. readLoop, naLoop/writeLoop, connectionMonitor and coldControlLoop exit and
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
// guaranteed by failAllPending's per-shard lock + delete-as-you-go, not by the
// Once; the Once only ensures the meaningful error wins over a later
// "use of closed connection" read error.
//
// Callable from a loop goroutine (readLoop/monitor): the Once body never touches
// loopWG, and only Close() waits on loopWG — after failConnection returns — so
// there is no self-deadlock.
func (c *Conn) failConnection(err error) {
	c.closeOnce.Do(func() {
		c.cancel()
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

// logSerious reports an unexpected, must-not-be-silent event (a recovered panic in
// the connection's background goroutines) at ERROR level through log/slog, passing
// structured attributes rather than a pre-formatted string so a JSON/structured
// handler gets queryable fields. Routing through slog.Default() makes these
// diagnostics pluggable via the standard Go mechanism (slog.SetDefault) with no
// fdbgo-specific logging API.
func (c *Conn) logSerious(msg string, attrs ...any) {
	if c != nil {
		if h := c.hooks.Load(); h != nil && h.seriousLog != nil {
			h.seriousLog(msg, attrs...)
			return
		}
	}
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
		c.logSerious("recovered panic in connection goroutine",
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
			c.logSerious("recovered panic in readLoop goroutine",
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
			isPending := c.HasPendingReply(token)
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

		// Stamp arrival time before acquiring the shard lock to minimize lock hold time.
		recvAt := time.Now()
		s := c.pendingShard(token)
		s.mu.Lock()
		entry, ok := s.m[token]
		if ok {
			// The fresh size-one channel has one producer. Publish together
			// with deletion so cancellation cannot fall into a delivery gap.
			entry.ch <- Response{Body: body, RecvAt: recvAt}
			delete(s.m, token)
		}
		s.mu.Unlock()
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
	for i := range c.pendingShards {
		s := &c.pendingShards[i]
		s.mu.Lock()
		for token, entry := range s.m {
			select {
			case entry.ch <- Response{Err: err}:
			default:
			}
			delete(s.m, token)
		}
		s.mu.Unlock()
	}
}
