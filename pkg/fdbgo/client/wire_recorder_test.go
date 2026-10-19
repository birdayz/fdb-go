package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// sendRecorder records the storage and location requests one Database writes,
// decoded from the client side of its sockets. Unlike transport.CaptureWireLog
// it sees only its own Database, so a test using it can run in parallel with
// other tests in the binary.
type sendRecorder struct {
	mu   sync.Mutex
	on   bool
	reqs []string
}

// wrap returns a DialFunc whose connections report every frame they send.
func (r *sendRecorder) wrap(dial transport.DialFunc) transport.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &recordingConn{Conn: c, rec: r}, nil
	}
}

// start discards what was recorded and records from now on.
func (r *sendRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on, r.reqs = true, nil
}

// stop ends recording and returns the requests recorded since start.
func (r *sendRecorder) stop() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
	return r.reqs
}

func (r *sendRecorder) add(req string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		r.reqs = append(r.reqs, req)
	}
}

// recordingConn parses the bytes the client writes: the 44-byte ConnectPacket,
// then non-TLS frames [len(4) checksum(8) token(16) body]. A frame is recorded
// inside Write, before its bytes reach the server, so a request is recorded
// before any request that waited on its reply.
type recordingConn struct {
	net.Conn
	rec *sendRecorder

	mu        sync.Mutex
	buf       []byte
	connected bool
}

func (c *recordingConn) Write(b []byte) (int, error) {
	c.observe(b)
	return c.Conn.Write(b)
}

func (c *recordingConn) observe(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, b...)
	if !c.connected {
		if len(c.buf) < transport.ConnectPacketSize {
			return
		}
		c.buf = c.buf[transport.ConnectPacketSize:]
		c.connected = true
	}
	for len(c.buf) >= 12 {
		n := int(binary.LittleEndian.Uint32(c.buf))
		if len(c.buf) < 12+n {
			return
		}
		if n >= 16 {
			if req, ok := decodeRecordedRequest(c.buf[12+16 : 12+n]); ok {
				c.rec.add(req)
			}
		}
		c.buf = c.buf[12+n:]
	}
}

// decodeRecordedRequest renders a storage or location request without its
// reply token and span context, the fields that identify a run rather than a
// request (the wire-golden normalization). The body length is kept, so a field
// encoded differently still shows.
func decodeRecordedRequest(body []byte) (string, bool) {
	off := 0
	if len(body) >= 16 && body[7] == 0x0F && body[6] == 0xDB {
		off = 8 // protocol version prefix
	}
	if len(body) < off+8 {
		return "", false
	}
	r, err := wire.NewReader(body)
	if err != nil {
		return "", false
	}
	switch binary.LittleEndian.Uint32(body[off+4 : off+8]) {
	case types.GetKeyValuesRequestFileID:
		var m types.GetKeyValuesRequest
		m.UnmarshalFromReader(r)
		m.Reply, m.SpanContext = types.ReplyPromise{}, types.SpanContext{}
		return fmt.Sprintf("GetKeyValues len=%d %+v", len(body), m), true
	case types.GetMappedKeyValuesRequestFileID:
		var m types.GetMappedKeyValuesRequest
		m.UnmarshalFromReader(r)
		m.Reply, m.SpanContext = types.ReplyPromise{}, types.SpanContext{}
		return fmt.Sprintf("GetMappedKeyValues len=%d %+v", len(body), m), true
	case types.GetKeyRequestFileID:
		var m types.GetKeyRequest
		m.UnmarshalFromReader(r)
		m.Reply, m.SpanContext = types.ReplyPromise{}, types.SpanContext{}
		return fmt.Sprintf("GetKey len=%d %+v", len(body), m), true
	case types.GetKeyServerLocationsRequestFileID:
		var m types.GetKeyServerLocationsRequest
		m.UnmarshalFromReader(r)
		m.Reply, m.SpanContext = types.ReplyPromise{}, types.SpanContext{}
		return fmt.Sprintf("GetKeyServerLocations len=%d %+v", len(body), m), true
	}
	return "", false
}
