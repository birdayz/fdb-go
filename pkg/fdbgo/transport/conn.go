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
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// replyChanPool pools chan Response to reduce allocations.
// Each PrepareReply/Send allocates one; readLoop returns it after dispatch.
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
		delete(h.conn.pending, h.token)
		putReplyChannel(h.ch)
	}
	h.conn.pendingMu.Unlock()
}

// Release returns the handle to the pool. Call after Cancel or after
// the reply has been successfully received.
func (h *ReplyHandle) Release() {
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
	conn      net.Conn
	useTLS    bool
	wbuf      *bufio.Writer // owned exclusively by writeLoop
	hasDirty  atomic.Bool   // true when wbuf has unflushed data
	writeCh   chan writeReq // channel-based write loop for coalescing
	ctx       context.Context
	cancel    context.CancelFunc
	loopWG    sync.WaitGroup // tracks readLoop + writeLoop goroutines
	closeOnce sync.Once      // guards the single failConnection teardown

	// Connection monitor cadence. Defaults match C++ CONNECTION_MONITOR_LOOP_TIME
	// (0.75s) / CONNECTION_MONITOR_TIMEOUT (2s); set once at dial time before the
	// monitor goroutine starts. Tests inject small values for deterministic,
	// fast monitor-death assertions (see withMonitorCadence).
	monitorLoopInterval time.Duration
	monitorTimeout      time.Duration

	// Typed pending map avoids sync.Map's interface boxing (saves 3 allocs/RPC).
	pendingMu sync.RWMutex
	pending   map[UID]chan Response
	peerPkt   *ConnectPacket // peer's connect packet

	// Connection monitor: counts bytes received so connectionMonitor() can
	// detect dead connections in ~2s (vs 10s TCP keepalive).
	// Matches C++ FlowTransport.actor.cpp connectionMonitor().
	bytesReceived atomic.Int64

	// Debug tracing (set before first use).
	debugFrames bool
	debugWriter io.Writer
}

// writeReq is a frame queued for the write loop.
type writeReq struct {
	token UID
	body  []byte
	errCh chan<- error // nil = fire-and-forget (deferred writes)
}

// Response is a received message from the peer.
type Response struct {
	Body []byte
	Err  error
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
func Dial(ctx context.Context, addr string, tlsConfig *tls.Config, dialFn DialFunc) (*Conn, error) {
	return dialWith(ctx, addr, dialFn, tlsConfig)
}

// errConnClosed is delivered to in-flight requests and queued senders when the
// connection is torn down (by Close, the monitor, or a read error). The client
// treats any non-nil transport error as a connection failure → retry, so the
// concrete value only needs to be non-nil and stable.
var errConnClosed = errors.New("connection closed")

// connOption tweaks a Conn before its loop goroutines start. Test-only knobs
// (e.g. monitor cadence) ride this so the public Dial signature stays clean.
type connOption func(*Conn)

// withMonitorCadence overrides the connection-monitor loop interval and timeout.
// Test-only: lets the monitor-death path fire in tens of ms instead of ~3.5s.
func withMonitorCadence(loop, timeout time.Duration) connOption {
	return func(c *Conn) {
		c.monitorLoopInterval = loop
		c.monitorTimeout = timeout
	}
}

// dialWith is the implementation behind Dial, plus test-only connOptions.
func dialWith(ctx context.Context, addr string, dialFn DialFunc, tlsConfig *tls.Config, opts ...connOption) (*Conn, error) {
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

	// Wrap in TLS iff a config was supplied. The non-nil config is the only
	// "use TLS" signal — there is no bool to disagree with it, so plaintext can
	// never be sent on a connection the caller wanted encrypted. An empty config
	// still attempts a real handshake and fails closed (never plaintext).
	if tlsConfig != nil {
		tlsConn, tlsErr := upgradeTLS(netConn, addr, tlsConfig)
		if tlsErr != nil {
			netConn.Close()
			return nil, fmt.Errorf("TLS handshake %s: %w", addr, tlsErr)
		}
		netConn = tlsConn
	}

	connCtx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		conn:    netConn,
		useTLS:  tlsConfig != nil, // drives frame-checksum omission; not a TLS switch
		wbuf:    bufio.NewWriterSize(netConn, 64*1024),
		writeCh: make(chan writeReq, 256), // buffered for concurrent senders
		pending: make(map[UID]chan Response, 16),
		ctx:     connCtx,
		cancel:  cancel,
		// C++ CONNECTION_MONITOR_LOOP_TIME / CONNECTION_MONITOR_TIMEOUT defaults.
		monitorLoopInterval: 750 * time.Millisecond,
		monitorTimeout:      2 * time.Second,
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
		return nil, fmt.Errorf("write connect packet: %w", err)
	}

	peerPkt, err := ReadConnectPacket(netConn)
	if err != nil {
		cancel()
		netConn.Close()
		return nil, fmt.Errorf("read connect packet: %w", err)
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

	// Start read, write, and connection monitor loops.
	c.loopWG.Add(3)
	go c.readLoop()
	go c.writeLoop()
	go c.connectionMonitor()

	return c, nil
}

// Send sends a request and returns a channel that will receive the response.
// The destToken identifies the remote endpoint (e.g., a StorageServer's getValue endpoint).
// The replyToken is a fresh token for routing the response back.
func (c *Conn) Send(destToken UID, body []byte) (replyToken UID, replyCh <-chan Response, err error) {
	replyToken = NewUID()
	ch := getReplyChannel()
	c.pendingMu.Lock()
	c.pending[replyToken] = ch
	c.pendingMu.Unlock()

	if err = c.SendFrame(destToken, body); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, replyToken)
		c.pendingMu.Unlock()
		putReplyChannel(ch)
		return UID{}, nil, fmt.Errorf("write frame: %w", err)
	}

	return replyToken, ch, nil
}

// PrepareReply allocates a reply token and registers it for response routing.
// Use this when you need the token BEFORE building the request body
// (e.g., to embed it in the FDB request's Reply field).
//
// Returns a ReplyHandle whose Cancel() removes the pending token. Call
// handle.Release() when done to return it to the pool.
func (c *Conn) PrepareReply() (UID, <-chan Response, *ReplyHandle) {
	h := replyHandlePool.Get().(*ReplyHandle)
	h.conn = c
	h.token = NewUID()
	h.ch = getReplyChannel()
	c.pendingMu.Lock()
	c.pending[h.token] = h.ch
	c.pendingMu.Unlock()
	return h.token, h.ch, h
}

// getReplyChannel gets a reply channel from the pool.
func getReplyChannel() chan Response {
	return replyChanPool.Get().(chan Response)
}

// putReplyChannel returns a reply channel to the pool after draining it.
func putReplyChannel(ch chan Response) {
	// Drain any buffered value (shouldn't normally happen).
	select {
	case <-ch:
	default:
	}
	replyChanPool.Put(ch)
}

// SendFrame writes a raw frame and flushes. Blocks until the frame is written
// to the TCP socket (or returns error). For single-frame RPCs where we
// immediately wait for the response.
func (c *Conn) SendFrame(destToken UID, body []byte) error {
	if c.debugFrames {
		fmt.Fprintf(c.debugWriter, "[send %s] token=%016x:%016x bodyLen=%d\n", c.conn.RemoteAddr(),
			destToken.First, destToken.Second, len(body))
	}
	LogSend(destToken, body)
	errCh := errChanPool.Get().(chan error)
	select {
	case c.writeCh <- writeReq{token: destToken, body: body, errCh: errCh}:
	case <-c.ctx.Done():
		errChanPool.Put(errCh)
		return errConnClosed
	}
	// Wait for writeLoop to write+flush, OR bail if the connection is torn down
	// (Close/monitor/read-error cancels ctx). Without the ctx.Done arm a sender
	// whose frame is still queued when writeLoop exits would block on errCh
	// forever. On the ctx.Done path errCh is deliberately NOT returned to the
	// pool: writeLoop may still hold a reference and send to it, which would
	// surface as a stale buffered value on the next pool user (audit #13).
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
	LogSend(destToken, body)
	c.hasDirty.Store(true) // mark dirty so Flush() knows to synchronize
	select {
	case c.writeCh <- writeReq{token: destToken, body: body}:
		return nil
	case <-c.ctx.Done():
		return errConnClosed
	}
}

// Flush ensures all pending frames are flushed to the TCP socket.
// For the write-loop architecture, this sends a synchronous flush marker
// through the write channel and waits for acknowledgment.
func (c *Conn) Flush() error {
	if !c.hasDirty.Load() {
		return nil
	}
	errCh := errChanPool.Get().(chan error)
	select {
	case c.writeCh <- writeReq{errCh: errCh}: // empty token+body = flush-only request
	case <-c.ctx.Done():
		errChanPool.Put(errCh)
		return errConnClosed
	}
	// Bail on connection teardown rather than block forever on errCh (see SendFrame).
	// errCh is not pooled on the ctx.Done path (stale-value hazard, audit #13).
	select {
	case err := <-errCh:
		c.hasDirty.Store(false)
		errChanPool.Put(errCh)
		return err
	case <-c.ctx.Done():
		return errConnClosed
	}
}

// writeLoop is the dedicated write goroutine. It reads frames from writeCh,
// writes them to the buffered writer, and flushes. Natural batching: after
// processing the first frame, it drains all other queued frames before
// flushing — so N concurrent SendFrame/SendFrameDeferred calls result in
// one flush (one write() syscall).
//
// This matches C++ FlowTransport's connectionWriter actor which yields to
// let senders enqueue, then flushes everything at once.
func (c *Conn) writeLoop() {
	defer c.loopWG.Done()

	// Collect errCh channels that need notification after flush.
	var errChans []chan<- error

	for {
		// Wait for first frame.
		var req writeReq
		select {
		case req = <-c.writeCh:
		case <-c.ctx.Done():
			return
		}

		// Process first frame.
		var writeErr error
		if req.token != (UID{}) || req.body != nil {
			writeErr = WriteFrame(c.wbuf, req.token, req.body, c.useTLS)
		}
		if req.errCh != nil {
			errChans = append(errChans, req.errCh)
		}

		// Drain all queued frames without blocking (natural coalescing).
		draining := true
		for draining && writeErr == nil {
			select {
			case req = <-c.writeCh:
				if req.token != (UID{}) || req.body != nil {
					writeErr = WriteFrame(c.wbuf, req.token, req.body, c.useTLS)
				}
				if req.errCh != nil {
					errChans = append(errChans, req.errCh)
				}
			default:
				draining = false
			}
		}

		// Flush all accumulated frames in one syscall.
		if writeErr == nil {
			writeErr = c.wbuf.Flush()
		}
		c.hasDirty.Store(false)

		// Notify all waiting senders.
		for _, ch := range errChans {
			ch <- writeErr
		}
		errChans = errChans[:0]
	}
}

// SendAndWait sends a request and blocks until the response arrives.
func (c *Conn) SendAndWait(ctx context.Context, destToken UID, body []byte) ([]byte, error) {
	_, replyCh, err := c.Send(destToken, body)
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-replyCh:
		return resp.Body, resp.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errConnClosed
	}
}

// Close closes the connection and waits for readLoop to exit.
// Safe to call multiple times. After Close returns, zero goroutines
// are running on this connection.
//
// Shutdown sequence (matches C++ connectionKeeper):
// 1. Cancel context — signals all waiters that conn is dead
// 2. Close TCP socket — unblocks readLoop's ReadFrame
// 3. readLoop delivers errors to all pending requests
// 4. readLoop exits, signals WaitGroup
// 5. Close returns
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
		_ = c.conn.Close()
		c.failAllPending(err)
	})
}

// SetDebug enables frame-level debug tracing to stderr.
func (c *Conn) SetDebug(enabled bool) {
	c.debugFrames = enabled
	c.debugWriter = os.Stderr
}

// BytesReceived returns the total bytes received on this connection.
// Used by the connection monitor for dead-connection detection.
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
	// Teardown on ANY exit — the normal read-error path AND an unexpected panic
	// in frame parsing. The single failConnection path (cancel + close socket +
	// fail all pending; C++ disconnect-promise equivalent) is idempotent, so if
	// Close/monitor already fired this is a no-op, and the first caller's error
	// wins. exitErr carries the real read error when there is one.
	exitErr := errConnClosed
	defer func() {
		c.failConnection(exitErr)
		c.loopWG.Done()
	}()

	pingToken := WellKnownToken(WLTokenPingPacket)
	var fr FrameReader

	for {
		token, body, err := fr.Read(c.conn, c.useTLS)
		if err != nil {
			// Only log if this is unexpected (server died, not our Close).
			if c.debugFrames && c.ctx.Err() == nil {
				fmt.Fprintf(c.debugWriter, "[recv] ERROR: %v\n", err)
			}
			exitErr = err
			return
		}

		// Track bytes received for connection monitor dead-connection detection.
		c.bytesReceived.Add(int64(len(body)))

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

		// Look up the pending request by reply token.
		c.pendingMu.Lock()
		ch, ok := c.pending[token]
		if ok {
			delete(c.pending, token)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- Response{Body: body}
		}
		// Unknown tokens are silently dropped (e.g., late responses after timeout).
	}
}

// failAllPending delivers an error to all pending request channels.
// Matches C++ connectionKeeper's disconnect promise that wakes all
// in-flight deliver() actors.
func (c *Conn) failAllPending(err error) {
	c.pendingMu.Lock()
	for token, ch := range c.pending {
		select {
		case ch <- Response{Err: err}:
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
	select {
	case c.writeCh <- writeReq{token: replyToken, body: replyBody}:
	default:
		// Write channel full — drop ping reply. Server will retry.
	}
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

	// PingRequest has 1 field: reply (ReplyPromise).
	// ReplyPromise uses save/load → serialized as opaque blob via WriteBytes.
	// The blob contains the UID token (16 bytes: part[0] + part[1]).
	if !r.FieldPresent(0) {
		return UID{}, false
	}

	// Try reading as bytes (length-prefixed blob).
	replyBytes := r.ReadBytes(0)
	if replyBytes != nil && len(replyBytes) >= 16 {
		uid := UID{
			First:  binary.LittleEndian.Uint64(replyBytes[0:8]),
			Second: binary.LittleEndian.Uint64(replyBytes[8:16]),
		}
		return uid, true
	}

	// Try reading as nested struct.
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
// Matches C++ FlowTransport.actor.cpp connectionMonitor() (lines 641-721):
//
// Outer loop:
//  1. Sleep CONNECTION_MONITOR_LOOP_TIME (750ms)
//  2. If no pending requests → check idle timeout (skip PING)
//  3. Sleep again (jittered), then send PING
//  4. Inner loop: wait CONNECTION_MONITOR_TIMEOUT (2s) per round
//     - If bytesReceived unchanged → kill connection
//     - If bytesReceived changed → update baseline, continue inner loop
//     - If PING reply arrives → break (connection alive)
//
// The inner retry loop is key: a single 2s timeout with no bytes kills,
// but if ANY bytes arrive (server PINGs, other responses), the baseline
// resets and we wait another 2s. This tolerates slow-but-alive connections.
func (c *Conn) connectionMonitor() {
	defer c.loopWG.Done()

	for {
		// Outer loop: sleep, then decide whether to PING.
		// C++ CONNECTION_MONITOR_LOOP_TIME = 0.75s (configurable for tests).
		select {
		case <-time.After(c.monitorLoopInterval):
		case <-c.ctx.Done():
			return
		}

		// C++ checks peer->reliable.empty() && peer->unsent.empty() && peer->outstandingReplies == 0.
		// If no pending requests, the connection is idle — skip PING, let TCP keepalive handle it.
		c.pendingMu.RLock()
		hasPending := len(c.pending) > 0
		c.pendingMu.RUnlock()
		if !hasPending {
			continue
		}

		// C++ second delay (jittered) before sending PING.
		select {
		case <-time.After(c.monitorLoopInterval):
		case <-c.ctx.Done():
			return
		}

		// Send PING and register for reply.
		replyCh := c.sendPingWithReply()

		// Inner loop: C++ lines 690-720.
		// Wait 2s per round. Kill only if bytesReceived is truly frozen.
		startingBytes := c.bytesReceived.Load()
		for {
			timer := time.NewTimer(c.monitorTimeout)
			select {
			case <-replyCh:
				// PING reply arrived — connection alive. C++ line 710-714.
				timer.Stop()
			case <-timer.C:
				// Timeout. Check if ANY bytes arrived.
				current := c.bytesReceived.Load()
				if current == startingBytes {
					// No bytes at all since PING was sent — connection is dead.
					// C++ line 698-699: throw connection_failed. Route through the
					// single teardown path: cancel + CLOSE THE SOCKET (so readLoop's
					// blocking Read unblocks — the old bare cancel() leaked the fd +
					// goroutine until TCP keepalive) + fail all pending.
					c.failConnection(errConnClosed)
					return
				}
				// Bytes arrived (server PINGs, other traffic) but not our PING reply.
				// C++ line 707-708: update baseline, loop again.
				// C++ uses timeouts counter here only for logging (ConnectionSlowPing),
				// NOT for kill decisions — the kill condition is solely bytesReceived.
				startingBytes = current
				continue
			case <-c.ctx.Done():
				timer.Stop()
				return
			}
			break
		}
	}
}

// sendPingWithReply sends a PingRequest and returns a channel that closes
// when the PING reply arrives. Matches C++ pingRequest.reply.getFuture().
// The reply is registered in the pending map so readLoop dispatches it.
func (c *Conn) sendPingWithReply() <-chan struct{} {
	done := make(chan struct{})
	replyToken, replyCh, replyHandle := c.PrepareReply()
	pingEP := WellKnownToken(WLTokenPingPacket)
	body := BuildPingRequest(replyToken)
	select {
	case c.writeCh <- writeReq{token: pingEP, body: body}:
	default:
		// Write channel full — cancel and return closed channel
		// so the inner loop falls through to bytesReceived check.
		replyHandle.Cancel()
		replyHandle.Release()
		close(done)
		return done
	}
	// Wait for reply in background, then signal done.
	go func() {
		defer replyHandle.Release()
		defer close(done)
		select {
		case <-replyCh:
			// Reply arrived.
		case <-c.ctx.Done():
			replyHandle.Cancel()
		}
	}()
	return done
}

// PingRequest file identifier from C++ flow/genericactors.actor.h.
const pingRequestFileID uint32 = 4707015

// pingRequestVTable: PingRequest has 1 field (bytes/RelativeOffset, 4 bytes, align 4).
// vtable[0] = 2*1+4 = 6 (vtable wire size)
// vtable[1] = 4+4 = 8 (object size with soffset)
// vtable[2] = 4 (field 0 offset)
var pingRequestVTable = wire.VTable{6, 8, 4}

var pingRequestVTableClosure = []wire.VTable{
	{6, 8, 4}, // PingRequest
	{6, 8, 4}, // FakeRoot
}

var pingRequestTemplate = wire.NewMessageTemplate(
	pingRequestFileID, pingRequestVTable, 4, pingRequestVTableClosure,
)

// BuildPingRequest builds a PingRequest FlatBuffers body.
// PingRequest has one field: ReplyPromise<Void> serialized as a bytes blob
// (Standalone<StringRef>) containing the 16-byte reply token UID.
// The server extracts this token and sends back a VoidReply to it.
func BuildPingRequest(replyToken UID) []byte {
	// The ReplyPromise is serialized via save/load trait → bytes blob.
	// The blob contains the serialized Endpoint, which starts with the
	// 16-byte token (two uint64 LE). This is the minimum the server
	// needs to route the reply back.
	var tokenBytes [16]byte
	binary.LittleEndian.PutUint64(tokenBytes[0:8], replyToken.First)
	binary.LittleEndian.PutUint64(tokenBytes[8:16], replyToken.Second)

	return wire.MarshalDirect(
		pingRequestTemplate,
		func(endOff int) int {
			// OOL: bytes blob = 4-byte length + 16-byte token + padding.
			return wire.MeasureBytesOOL(endOff, tokenBytes[:])
		},
		func(dw *wire.DirectWriter) int {
			// Write the bytes blob (OOL data).
			blobPos := dw.WriteBytesOOL(tokenBytes[:])

			// Write the PingRequest object.
			objPos, obj := dw.WriteObject(pingRequestVTable, 4)

			// Field 0: RelativeOffset to the bytes blob.
			fieldOff := int(pingRequestVTable[2]) // offset 4
			wire.PatchRelOff(obj, fieldOff, objPos, blobPos)

			return objPos
		},
	)
}

// fastRNG is a per-goroutine fast PRNG for UID generation.
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
