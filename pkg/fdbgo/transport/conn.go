package transport

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// replyChanPool pools chan Response to reduce allocations.
// Each PrepareReply/Send allocates one; readLoop returns it after dispatch.
var replyChanPool = sync.Pool{New: func() any { return make(chan Response, 1) }}

// errChanPool pools chan error for SendFrame/Flush synchronization.
var errChanPool = sync.Pool{New: func() any { return make(chan error, 1) }}

// Conn is a multiplexed FDB connection. Multiple concurrent requests
// share one TCP connection, matched by endpoint tokens.
//
// Lifecycle: after Close() returns, zero goroutines are running.
// If the server kills the connection, readLoop cancels the context
// and IsClosed() returns true — the connection pool will evict it.
type Conn struct {
	conn     net.Conn
	useTLS   bool
	wbuf     *bufio.Writer // owned exclusively by writeLoop
	hasDirty atomic.Bool   // true when wbuf has unflushed data
	writeCh  chan writeReq // channel-based write loop for coalescing
	ctx      context.Context
	cancel   context.CancelFunc
	loopWG   sync.WaitGroup // tracks readLoop + writeLoop goroutines

	// Typed pending map avoids sync.Map's interface boxing (saves 3 allocs/RPC).
	pendingMu sync.RWMutex
	pending   map[UID]chan Response
	peerPkt   *ConnectPacket // peer's connect packet

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

// Dial connects to an FDB process, exchanges ConnectPackets, and starts
// the read loop for response multiplexing.
// DialFunc is the signature for custom dialers. Same as net.Dialer.DialContext.
// Default is net.Dialer{}.DialContext. Override for testing (fault injection,
// custom Docker networking, traffic shaping).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Dial connects to an FDB server using the default net.Dialer.
func Dial(ctx context.Context, addr string, tls bool) (*Conn, error) {
	return DialWith(ctx, addr, tls, nil)
}

// DialWith connects to an FDB server using a custom dialer.
// If dialFn is nil, uses the default net.Dialer.
// TLSConfig holds TLS configuration for FDB connections.
// If non-nil, connections use TLS with the specified certificates.
type TLSConfig struct {
	CertFile string // Path to client certificate (PEM)
	KeyFile  string // Path to client private key (PEM)
	CAFile   string // Path to CA certificate (PEM)
}

func DialWith(ctx context.Context, addr string, tls bool, dialFn DialFunc) (*Conn, error) {
	return DialWithTLS(ctx, addr, tls, dialFn, nil)
}

// DialWithTLS connects to an FDB server with optional TLS.
// If tlsCfg is non-nil and tls is true, the connection is encrypted.
func DialWithTLS(ctx context.Context, addr string, useTLS bool, dialFn DialFunc, tlsCfg *TLSConfig) (*Conn, error) {
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
	}

	// Wrap in TLS if configured.
	if useTLS && tlsCfg != nil {
		tlsConn, tlsErr := upgradeTLS(netConn, addr, tlsCfg)
		if tlsErr != nil {
			netConn.Close()
			return nil, fmt.Errorf("TLS handshake %s: %w", addr, tlsErr)
		}
		netConn = tlsConn
	}

	connCtx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		conn:    netConn,
		useTLS:  useTLS,
		wbuf:    bufio.NewWriterSize(netConn, 64*1024),
		writeCh: make(chan writeReq, 256), // buffered for concurrent senders
		pending: make(map[UID]chan Response, 16),
		ctx:     connCtx,
		cancel:  cancel,
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

	// Start read and write loops.
	c.loopWG.Add(2)
	go c.readLoop()
	go c.writeLoop()

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
// Returns a cancel function that removes the pending token from the map.
func (c *Conn) PrepareReply() (UID, <-chan Response, func()) {
	token := NewUID()
	ch := getReplyChannel()
	c.pendingMu.Lock()
	c.pending[token] = ch
	c.pendingMu.Unlock()
	cancel := func() {
		c.pendingMu.Lock()
		if _, ok := c.pending[token]; ok {
			delete(c.pending, token)
			putReplyChannel(ch)
		}
		c.pendingMu.Unlock()
	}
	return token, ch, cancel
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
		fmt.Fprintf(c.debugWriter, "[send] token=%016x:%016x bodyLen=%d\n",
			destToken.First, destToken.Second, len(body))
	}
	LogSend(destToken, body)
	errCh := errChanPool.Get().(chan error)
	select {
	case c.writeCh <- writeReq{token: destToken, body: body, errCh: errCh}:
	case <-c.ctx.Done():
		errChanPool.Put(errCh)
		return fmt.Errorf("connection closed")
	}
	err := <-errCh
	errChanPool.Put(errCh)
	return err
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
		return fmt.Errorf("connection closed")
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
		return fmt.Errorf("connection closed")
	}
	err := <-errCh
	c.hasDirty.Store(false)
	errChanPool.Put(errCh)
	return err
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
		return nil, fmt.Errorf("connection closed")
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
	c.cancel() // idempotent — safe to call multiple times
	err := c.conn.Close()
	c.loopWG.Wait()
	return err
}

// SetDebug enables frame-level debug tracing to stderr.
func (c *Conn) SetDebug(enabled bool) {
	c.debugFrames = enabled
	c.debugWriter = os.Stderr
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
	defer c.loopWG.Done()
	defer c.cancel()     // if server kills us, mark connection dead
	defer c.conn.Close() // close TCP socket — prevents fd leak on Path B (server dies)

	pingToken := WellKnownToken(WLTokenPingPacket)

	for {
		token, body, err := ReadFrame(c.conn, c.useTLS)
		if err != nil {
			// Only log if this is unexpected (server died, not our Close).
			if c.debugFrames && c.ctx.Err() == nil {
				fmt.Fprintf(c.debugWriter, "[recv] ERROR: %v\n", err)
			}
			// Deliver error to all pending requests (C++ disconnect promise equivalent).
			c.failAllPending(err)
			return
		}

		LogRecv(token, body)

		if c.debugFrames {
			c.pendingMu.RLock()
			_, isPending := c.pending[token]
			c.pendingMu.RUnlock()
			fmt.Fprintf(c.debugWriter, "[recv] token=%016x:%016x bodyLen=%d ping=%v pending=%v\n",
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

// upgradeTLS wraps a TCP connection in TLS using the provided certificates.
// Matches FDB's TLS requirements: mutual authentication with client cert.
func upgradeTLS(conn net.Conn, addr string, cfg *TLSConfig) (net.Conn, error) {
	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load client certificate for mutual TLS.
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsConf.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate for server verification.
	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("load CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert")
		}
		tlsConf.RootCAs = pool
	}

	// Extract hostname for SNI.
	host, _, _ := net.SplitHostPort(addr)
	tlsConf.ServerName = host

	tlsConn := tls.Client(conn, tlsConf)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	return tlsConn, nil
}
