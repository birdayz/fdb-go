package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// Conn is a multiplexed FDB connection. Multiple concurrent requests
// share one TCP connection, matched by endpoint tokens.
type Conn struct {
	conn    net.Conn
	tls     bool
	mu      sync.Mutex     // protects writes
	pending sync.Map       // UID → chan Response
	peerPkt *ConnectPacket // peer's connect packet
	done    chan struct{}

	// Debug tracing (set before first use).
	debugFrames bool
	debugWriter io.Writer
}

// Response is a received message from the peer.
type Response struct {
	Body []byte
	Err  error
}

// Dial connects to an FDB process, exchanges ConnectPackets, and starts
// the read loop for response multiplexing.
func Dial(ctx context.Context, addr string, tls bool) (*Conn, error) {
	var d net.Dialer
	netConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	c := &Conn{
		conn: netConn,
		tls:  tls,
		done: make(chan struct{}),
	}

	// Exchange ConnectPackets.
	connID := newConnectionID()
	if err := WriteConnectPacket(netConn, netConn.LocalAddr(), connID); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("write connect packet: %w", err)
	}

	peerPkt, err := ReadConnectPacket(netConn)
	if err != nil {
		netConn.Close()
		return nil, fmt.Errorf("read connect packet: %w", err)
	}

	if !peerPkt.IsCompatible(ProtocolVersion73) {
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

	// Start read loop.
	go c.readLoop()

	return c, nil
}

// Send sends a request and returns a channel that will receive the response.
// The destToken identifies the remote endpoint (e.g., a StorageServer's getValue endpoint).
// The replyToken is a fresh token for routing the response back.
func (c *Conn) Send(destToken UID, body []byte) (replyToken UID, replyCh <-chan Response, err error) {
	replyToken = NewUID()
	ch := make(chan Response, 1)
	c.pending.Store(replyToken, ch)

	c.mu.Lock()
	err = WriteFrame(c.conn, destToken, body, c.tls)
	c.mu.Unlock()

	if err != nil {
		c.pending.Delete(replyToken)
		return UID{}, nil, fmt.Errorf("write frame: %w", err)
	}

	return replyToken, ch, nil
}

// PrepareReply allocates a reply token and registers it for response routing.
// Use this when you need the token BEFORE building the request body
// (e.g., to embed it in the FDB request's Reply field).
func (c *Conn) PrepareReply() (UID, <-chan Response) {
	token := NewUID()
	ch := make(chan Response, 1)
	c.pending.Store(token, ch)
	return token, ch
}

// SendFrame writes a raw frame to the connection. The destToken is the
// remote endpoint token placed in the frame header. The body is sent as-is.
func (c *Conn) SendFrame(destToken UID, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.debugFrames {
		fmt.Fprintf(c.debugWriter, "[send] token=%016x:%016x bodyLen=%d\n",
			destToken.First, destToken.Second, len(body))
	}
	return WriteFrame(c.conn, destToken, body, c.tls)
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
	case <-c.done:
		return nil, fmt.Errorf("connection closed")
	}
}

// Close closes the connection.
func (c *Conn) Close() error {
	select {
	case <-c.done:
		return nil // already closed
	default:
		close(c.done)
	}
	return c.conn.Close()
}

// SetDebug enables frame-level debug tracing to stderr.
func (c *Conn) SetDebug(enabled bool) {
	c.debugFrames = enabled
	c.debugWriter = os.Stderr
}

// IsClosed returns true if the connection has been closed.
func (c *Conn) IsClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// PeerProtocolVersion returns the peer's protocol version.
func (c *Conn) PeerProtocolVersion() uint64 {
	return c.peerPkt.ProtocolVersion & ^ObjectSerializerFlag
}

// readLoop reads frames and dispatches responses to waiting goroutines.
func (c *Conn) readLoop() {
	defer c.Close()

	pingToken := WellKnownToken(WLTokenPingPacket)

	for {
		token, body, err := ReadFrame(c.conn, c.tls)
		if err != nil {
			if c.debugFrames {
				fmt.Fprintf(c.debugWriter, "[recv] ERROR: %v\n", err)
			}
			// Deliver error to all pending requests.
			c.pending.Range(func(key, value any) bool {
				ch := value.(chan Response)
				select {
				case ch <- Response{Err: err}:
				default:
				}
				c.pending.Delete(key)
				return true
			})
			return
		}

		if c.debugFrames {
			_, isPending := c.pending.Load(token)
			fmt.Fprintf(c.debugWriter, "[recv] token=%016x:%016x bodyLen=%d ping=%v pending=%v\n",
				token.First, token.Second, len(body), token == pingToken, isPending)
		}

		// Handle PING requests from the server.
		if token == pingToken {
			c.handlePing(body)
			continue
		}

		// Look up the pending request by reply token.
		if val, ok := c.pending.LoadAndDelete(token); ok {
			ch := val.(chan Response)
			ch <- Response{Body: body}
		}
		// Unknown tokens are silently dropped (e.g., late responses after timeout).
	}
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

	c.mu.Lock()
	_ = WriteFrame(c.conn, replyToken, replyBody, c.tls)
	c.mu.Unlock()
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

// voidReplyGroundTruth is the C++ ObjectWriter ground-truth bytes for
// ErrorOr<EnsureTable<Void>> with present=true (success).
//
// Generated by FDB 7.3.75's real ObjectWriter::toValue(). The key insight
// is that FakeRoot FLATTENS the root type's members into its own object.
// For ErrorOr (union_like_traits), this means the FakeRoot object directly
// contains the union's type byte + value RelativeOffset — there is NO
// separate message object with an extra level of indirection.
//
// Structure (48 bytes):
//
//	[root_offset=32][file_id=0x021EAD4A][padding]
//	[VT_empty:{4,4}][VT_fakeroot_void:{6,6,4}][VT_erroror:{8,9,8,4}]
//	[FakeRoot: soff=8 → VT_erroror | value_reloff=8 → nested | type=2 | pad]
//	[EnsureTable<Void>: soff=30 → VT_empty]
var voidReplyGroundTruth, _ = hex.DecodeString(
	"200000004aad1e02" + // root_offset=32, file_id
		"0000000000000400" + // padding, VT_empty start
		"0400060006000400" + // VT_empty end, VT_fakeroot_void
		"0800090008000400" + // VT_erroror {8,9,8,4}
		"0800000008000000" + // FakeRoot: soff=8, value_reloff=8
		"020000001e000000") // type=2, EnsureTable soff=30

// buildVoidReply returns the ground-truth ErrorOr<EnsureTable<Void>> reply.
func buildVoidReply() []byte {
	return voidReplyGroundTruth
}

// NewUID generates a random 128-bit UID for endpoint tokens.
func NewUID() UID {
	var buf [16]byte
	rand.Read(buf[:])
	return UID{
		First:  binary.LittleEndian.Uint64(buf[0:8]),
		Second: binary.LittleEndian.Uint64(buf[8:16]),
	}
}

func newConnectionID() uint64 {
	var buf [8]byte
	rand.Read(buf[:])
	return binary.LittleEndian.Uint64(buf[:])
}
