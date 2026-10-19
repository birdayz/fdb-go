package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sync"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func plainDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// sendRecorder records the requests one Database writes, decoded from the
// client side of its sockets. Unlike transport.CaptureWireLog it sees only its
// own Database, so a test using it can run in parallel with other tests.
//
// It fails closed. While recording, a frame is either connection upkeep (a
// PING to the well-known ping endpoint, or the empty reply the client sends
// to a server's PING; counted in excluded), a request of an expected kind
// (rendered into requests), or a problem: a frame of another kind, a frame
// that does not decode, or bytes left over as a partial frame when recording
// stops. A test must treat any problem as a failure. "Decodes" means the
// generated decoder accepts the body and re-encoding what it decoded gives the
// same bytes back: the decoder reads an out-of-bounds or unreadable field as
// absent, so the re-encoding is what catches a malformed field of a known kind.
//
// Lock order: a recordingConn's mu, then the recorder's mu. A connection's
// parser holds its own mu while it reports frames (so one connection's frames
// are recorded in the order written), so nothing may take a connection's mu
// while holding the recorder's: code that needs both copies the connection
// list under the recorder's mu and releases it first (stop).
type sendRecorder struct {
	mu    sync.Mutex
	on    bool
	conns []*recordingConn
	rec   recording
}

// recording is what one start..stop window saw.
type recording struct {
	requests []string
	problems []string
	excluded int
	// storageReplies are the reply tokens of the storage requests sent, so a
	// fault can target the reply to a storage request.
	storageReplies map[transport.UID]bool
	// replyKinds maps each recorded request's reply token to its kind, so a
	// fault can target the reply to one kind of request.
	replyKinds map[transport.UID]string
	versions   map[int64]string
}

var (
	pingToken      = transport.WellKnownToken(transport.WLTokenPingPacket)
	pingReplyBody  = (&types.VoidReply{}).MarshalFDB()
	droppedFields  = map[string]bool{"Reply": true, "SpanContext": true, "DebugID": true, "HasDebugID": true, "Arena": true}
	versionFields  = map[string]bool{"Version": true, "ReadSnapshot": true, "MinKnownCommittedVersion": true}
	recordedKinds  = map[uint32]func() any{}
	storageRequest = map[string]bool{"GetValueRequest": true, "GetKeyRequest": true, "GetKeyValuesRequest": true, "GetMappedKeyValuesRequest": true}
)

// The kinds a storage read may send: storage requests, location lookups,
// read versions, and the other storage-bound requests of the client.
func init() {
	for id, mk := range map[uint32]func() any{
		types.GetValueRequestFileID:              func() any { return &types.GetValueRequest{} },
		types.GetKeyRequestFileID:                func() any { return &types.GetKeyRequest{} },
		types.GetKeyValuesRequestFileID:          func() any { return &types.GetKeyValuesRequest{} },
		types.GetMappedKeyValuesRequestFileID:    func() any { return &types.GetMappedKeyValuesRequest{} },
		types.GetKeyServerLocationsRequestFileID: func() any { return &types.GetKeyServerLocationsRequest{} },
		types.GetReadVersionRequestFileID:        func() any { return &types.GetReadVersionRequest{} },
		types.WatchValueRequestFileID:            func() any { return &types.WatchValueRequest{} },
		types.WaitMetricsRequestFileID:           func() any { return &types.WaitMetricsRequest{} },
		types.SplitRangeRequestFileID:            func() any { return &types.SplitRangeRequest{} },
	} {
		recordedKinds[id] = mk
	}
}

// wrap returns a DialFunc whose connections report every frame they send.
func (r *sendRecorder) wrap(dial transport.DialFunc) transport.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		rc := &recordingConn{Conn: c, rec: r}
		r.mu.Lock()
		r.conns = append(r.conns, rc)
		r.mu.Unlock()
		return rc, nil
	}
}

// parser registers a connection parser with no socket behind it, so bytes fed
// to its observe reach the recorder and nothing else, and no real write can
// interleave with them. remove unregisters it.
func (r *sendRecorder) parser() (rc *recordingConn, remove func()) {
	rc = &recordingConn{rec: r, connected: true}
	r.mu.Lock()
	r.conns = append(r.conns, rc)
	r.mu.Unlock()
	return rc, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.conns = slices.DeleteFunc(r.conns, func(c *recordingConn) bool { return c == rc })
	}
}

// start records from now on, with versions named from V1 again.
func (r *sendRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = true
	r.rec = recording{storageReplies: map[transport.UID]bool{}, replyKinds: map[transport.UID]string{}, versions: map[int64]string{}}
}

// stop ends recording. A connection holding part of a frame is a problem:
// that frame was neither compared nor excluded. Each connection is inspected
// with the recorder's mu released (lock order, sendRecorder).
func (r *sendRecorder) stop() recording {
	r.mu.Lock()
	conns := append([]*recordingConn(nil), r.conns...)
	r.mu.Unlock()
	var partial []string
	for _, c := range conns {
		if n := c.pending(); n > 0 {
			partial = append(partial, fmt.Sprintf("partial frame: %d bytes left on a connection", n))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
	rec := r.rec
	rec.problems = append(rec.problems, partial...)
	return rec
}

// replyKind returns the kind of the request recorded in this window whose
// reply token is token, or "" for any other reply.
func (r *sendRecorder) replyKind(token transport.UID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.on {
		return ""
	}
	return r.rec.replyKinds[token]
}

// isStorageReply reports whether token is the reply token of a storage request
// recorded in this window.
func (r *sendRecorder) isStorageReply(token transport.UID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.on && r.rec.storageReplies[token]
}

func (r *sendRecorder) frame(token transport.UID, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.on {
		return
	}
	if token == pingToken || bytes.Equal(body, pingReplyBody) {
		r.rec.excluded++
		return
	}
	req, kind, reply, err := decodeRecordedRequest(body, r.rec.versionName)
	if err != nil {
		r.rec.problems = append(r.rec.problems, fmt.Sprintf("%v (token %x, %d bytes)", err, token, len(body)))
		return
	}
	if storageRequest[kind] {
		r.rec.storageReplies[reply] = true
	}
	if reply != (transport.UID{}) {
		r.rec.replyKinds[reply] = kind
	}
	r.rec.requests = append(r.rec.requests, req)
}

func (r *sendRecorder) problem(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		r.rec.problems = append(r.rec.problems, p)
	}
}

// versionName numbers versions in order of appearance, so arms that took
// different read versions compare equal when they used them alike.
func (rec *recording) versionName(v int64) string {
	if v <= 0 {
		return fmt.Sprint(v) // invalidVersion, latestVersion and 0 keep their values
	}
	if n, ok := rec.versions[v]; ok {
		return n
	}
	n := fmt.Sprintf("V%d", len(rec.versions)+1)
	rec.versions[v] = n
	return n
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

func (c *recordingConn) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buf)
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
		if n < 16 {
			c.rec.problem(fmt.Sprintf("malformed frame: payload length %d", n))
			c.buf = nil // the stream cannot be resynchronized
			return
		}
		if len(c.buf) < 12+n {
			return
		}
		token := transport.UID{
			First:  binary.LittleEndian.Uint64(c.buf[12:20]),
			Second: binary.LittleEndian.Uint64(c.buf[20:28]),
		}
		c.rec.frame(token, c.buf[28:12+n])
		c.buf = c.buf[12+n:]
	}
}

// decodeRecordedRequest renders a request of an expected kind with its reply
// token, span and debug IDs left out (they identify a run, not a request) and
// versions named; the body length is kept, so a field encoded differently
// still shows. It returns the request kind and its reply token. The body must
// re-encode to itself: the generated decoder reads an out-of-bounds field as
// absent instead of failing, so without that check a malformed field of a
// known kind would compare as a missing one.
func decodeRecordedRequest(body []byte, versionName func(int64) string) (_, _ string, _ transport.UID, err error) {
	off := 0
	if len(body) >= 16 && body[7] == 0x0F && body[6] == 0xDB {
		off = 8 // protocol version prefix
	}
	if len(body) < off+8 {
		return "", "", transport.UID{}, fmt.Errorf("undecodable frame: no file identifier")
	}
	id := binary.LittleEndian.Uint32(body[off+4 : off+8])
	mk := recordedKinds[id]
	if mk == nil {
		return "", "", transport.UID{}, fmt.Errorf("unexpected request kind: file identifier %d", id)
	}
	r, err := wire.NewReader(body)
	if err != nil {
		return "", "", transport.UID{}, fmt.Errorf("undecodable frame: %w", err)
	}
	m := mk()
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("undecodable frame: decoder panicked: %v", p)
			}
		}()
		m.(interface{ UnmarshalFromReader(*wire.Reader) }).UnmarshalFromReader(r)
	}()
	if err != nil {
		return "", "", transport.UID{}, err
	}
	if again := m.(interface{ MarshalFDB() []byte }).MarshalFDB(); !bytes.Equal(again, body[off:]) {
		return "", "", transport.UID{}, fmt.Errorf("undecodable frame: %T does not re-encode to the bytes sent (%d bytes, re-encoded %d)", m, len(body)-off, len(again))
	}
	v := reflect.ValueOf(m).Elem()
	kind := v.Type().Name()
	var reply transport.UID
	if f := v.FieldByName("Reply"); f.IsValid() {
		tok := f.Interface().(types.ReplyPromise).Token
		reply = transport.UID{First: binary.LittleEndian.Uint64(tok[:8]), Second: binary.LittleEndian.Uint64(tok[8:])}
	}
	fields := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		f := v.Field(i)
		switch {
		case droppedFields[name]:
		case versionFields[name] && f.Kind() == reflect.Int64:
			fields = append(fields, name+"="+versionName(f.Int()))
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.Uint8:
			fields = append(fields, fmt.Sprintf("%s=%q", name, f.Bytes()))
		default:
			fields = append(fields, fmt.Sprintf("%s=%+v", name, f.Interface()))
		}
	}
	return fmt.Sprintf("%s len=%d %v", kind, len(body), fields), kind, reply, nil
}

// recorderFrame is a non-TLS frame as the client writes it: length, an
// unchecked checksum, the destination token, then the body.
func recorderFrame(token transport.UID, body []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(16+len(body)))
	out = append(out, make([]byte, 8)...)
	out = binary.LittleEndian.AppendUint64(out, token.First)
	out = binary.LittleEndian.AppendUint64(out, token.Second)
	return append(out, body...)
}

// malformedGetKeyValues is a GetKeyValuesRequest whose Begin field offset
// points past the object. wire.NewReader accepts it and the generated decoder
// reads Begin as absent rather than failing.
func malformedGetKeyValues() []byte {
	body := (&types.GetKeyValuesRequest{
		Begin:   types.KeySelectorRef{Key: []byte("a"), Offset: 1},
		End:     types.KeySelectorRef{Key: []byte("b"), Offset: 1},
		Version: 1, Limit: 1, LimitBytes: 1,
	}).MarshalFDB()
	le := binary.LittleEndian
	root := int(le.Uint32(body))
	msg := root + 4 + int(le.Uint32(body[root+4:]))
	vtable := msg - int(int32(le.Uint32(body[msg:])))
	le.PutUint16(body[vtable+2*(types.GetKeyValuesRequestSlotBegin+2):], 0xfff0)
	return body
}
