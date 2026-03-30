package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
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

		if pingDebugLog {
			fmt.Printf("[READLOOP] frame: token=%016x:%016x body=%d bytes\n", token.First, token.Second, len(body))
		}

		// Handle PING requests from the server.
		// TODO: Fix ErrorOr<EnsureTable<Void>> format to properly reply.
		// For now, just consume the PING frame without replying.
		// The server times out after ~3s, but our requests can still be
		// processed within that window.
		if token == pingToken {
			if pingDebugLog {
				fmt.Printf("[PING] consumed PING (%d body bytes), NOT replying (format WIP)\n", len(body))
			}
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

// pingDebugLog is set to true to enable PING debug logging via println.
// TODO: remove after debugging is complete.
var pingDebugLog = true

// extractPingReplyToken extracts the reply UID from a PingRequest FlatBuffers body.
func extractPingReplyToken(body []byte) (UID, bool) {
	if len(body) < 40 {
		if pingDebugLog {
			fmt.Printf("[PING] body too short: %d bytes\n", len(body))
		}
		return UID{}, false
	}

	r, err := wire.NewReader(body)
	if err != nil {
		if pingDebugLog {
			fmt.Printf("[PING] NewReader error: %v\n", err)
		}
		return UID{}, false
	}

	if pingDebugLog {
		fmt.Printf("[PING] body hex (%d bytes): %x\n", len(body), body)
		fmt.Printf("[PING] file_id: %d, vtable_len: %d\n", r.FileIdentifier(), r.VTableLength())
		for i := 0; i < r.VTableLength()-2; i++ {
			fmt.Printf("[PING]   slot %d: present=%v offset=%d\n", i, r.FieldPresent(i), r.FieldOffset(i))
		}
	}

	// PingRequest has 1 field: reply (ReplyPromise).
	// ReplyPromise uses save/load → serialized as opaque blob via WriteBytes.
	// The blob contains the UID token (16 bytes: part[0] + part[1]).
	if !r.FieldPresent(0) {
		if pingDebugLog {
			fmt.Printf("[PING] field 0 not present\n")
		}
		return UID{}, false
	}

	// Try reading as bytes (length-prefixed blob).
	replyBytes := r.ReadBytes(0)
	if pingDebugLog {
		if replyBytes != nil {
			fmt.Printf("[PING] ReadBytes(0): %d bytes: %x\n", len(replyBytes), replyBytes)
		} else {
			fmt.Printf("[PING] ReadBytes(0): nil\n")
		}
	}

	if replyBytes != nil && len(replyBytes) >= 16 {
		uid := UID{
			First:  binary.LittleEndian.Uint64(replyBytes[0:8]),
			Second: binary.LittleEndian.Uint64(replyBytes[8:16]),
		}
		if pingDebugLog {
			fmt.Printf("[PING] extracted token: %016x:%016x\n", uid.First, uid.Second)
		}
		return uid, true
	}

	// Try reading as nested struct.
	nestedR, err := r.ReadNestedReader(0)
	if err != nil {
		if pingDebugLog {
			fmt.Printf("[PING] ReadNestedReader(0) error: %v\n", err)
		}
		return UID{}, false
	}

	if pingDebugLog {
		fmt.Printf("[PING] nested vtable_len: %d\n", nestedR.VTableLength())
		for i := 0; i < nestedR.VTableLength()-2; i++ {
			fmt.Printf("[PING]   nested slot %d: present=%v offset=%d\n", i, nestedR.FieldPresent(i), nestedR.FieldOffset(i))
		}
	}

	// Read UID inline at the nested struct's field 0.
	if !nestedR.FieldPresent(0) {
		return UID{}, false
	}
	off := nestedR.FieldOffset(0)
	obj := nestedR.ObjectBytes()
	if off+16 > len(obj) {
		if pingDebugLog {
			fmt.Printf("[PING] nested object too short: off=%d, len=%d\n", off, len(obj))
		}
		return UID{}, false
	}
	uid := UID{
		First:  binary.LittleEndian.Uint64(obj[off:]),
		Second: binary.LittleEndian.Uint64(obj[off+8:]),
	}
	if pingDebugLog {
		fmt.Printf("[PING] extracted token (nested): %016x:%016x\n", uid.First, uid.Second)
	}
	return uid, true
}

// buildVoidReply builds a minimal ErrorOr<EnsureTable<Void>> FlatBuffers response.
// This is the reply to a PingRequest (ReplyPromise<Void>).
func buildVoidReply() []byte {
	// ErrorOr<T> has union_like_traits in FlatBuffers:
	//   alternatives = pack<Error, T>  →  index 0=Error, index 1=T
	//   Union serialization: 2 vtable slots:
	//     slot 0: type tag (uint8) — 0=Error, 1=present(T)
	//     slot 1: value (RelativeOffset to nested struct)
	//
	// For T = EnsureTable<Void>:
	//   type = 1 (present)
	//   value → nested EnsureTable<Void> struct (empty, just vtable soffset)
	//
	// file_identifier = (2 << 24) | Void::file_identifier
	const errorOrVoidFileID uint32 = (2 << 24) | 2010442 // 0x021EAD4A
	// ErrorOr is union_like: slot 0=type(uint8), slot 1=value(RelOffset)
	// For present(T): fb_type_tag = index + 1 = 1 + 1 = 2
	// Sorted by size: value(4)→offset 4, type(1)→offset 8
	// Since Void is empty, the value RelOffset just needs to point somewhere valid.
	// The server's load_helper for EnsureTable<Void> creates SerializeFun at the
	// RelOffset position but reads zero fields (Void::serialize is empty).
	//
	// Try minimal: just write the type tag and leave value as zero (absent).
	// This might work since ErrorOr::empty() returns false, and the load code
	// checks field_present() for the value slot separately.
	errorOrVoidVTable := wire.VTable{8, 9, 8, 4}
	ensureTableVoidVTable := wire.VTable{4, 4}
	w := wire.NewWriter(nil)
	return w.WriteMessage(errorOrVoidFileID, errorOrVoidVTable, 4, func(obj *wire.ObjectWriter) {
		obj.WriteUint8(8, 2) // slot 0: type = 2 (present T)
		obj.WriteStruct(4, ensureTableVoidVTable, 4, func(inner *wire.ObjectWriter) {
			// EnsureTable<Void>: Void is zero-size, nothing to write.
			// The server's LoadSaveHelper wraps Void in FakeRoot<Void>
			// but since Void is size 0, no data is actually read.
		})
	})
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
