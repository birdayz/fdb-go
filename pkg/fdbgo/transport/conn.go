package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
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

		// Look up the pending request by reply token.
		if val, ok := c.pending.LoadAndDelete(token); ok {
			ch := val.(chan Response)
			ch <- Response{Body: body}
		}
		// Unknown tokens are silently dropped (e.g., late responses after timeout).
	}
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
