package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

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
// valid PingRequest to the well-known ping endpoint, or the empty reply the client sends
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
	// msgs are the decoded requests, parallel to requests.
	msgs     []any
	problems []string
	excluded int
	writes   []sendRecordedWrite // raw bodies and connection identity, upkeep included
	// storageReplies are the reply tokens of the storage requests sent, so a
	// fault can target the reply to a storage request.
	storageReplies map[transport.UID]bool
	// replyKinds maps each recorded request's reply token to its kind, so a
	// fault can target the reply to one kind of request.
	replyKinds map[transport.UID]string
	versions   map[int64]string
}

type sendRecordedWrite struct {
	conn    *recordingConn
	token   transport.UID
	body    []byte // payload, or rejected framing bytes when no payload could be decoded
	kind    string
	reply   transport.UID
	problem string
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

// decodeRecordedCoordWrite recognizes only the client's typed-vector encoding.
// The generated OpenDatabaseCoordRequest treats those vectors as byte strings,
// so its MarshalFDB cannot round-trip a real coordinator long poll.
func decodeRecordedCoordWrite(body []byte) (reply transport.UID, recognized bool, err error) {
	off := 0
	if len(body) >= 16 && body[7] == 0x0F && body[6] == 0xDB {
		off = 8
	}
	if len(body) < off+8 || binary.LittleEndian.Uint32(body[off+4:off+8]) != types.OpenDatabaseCoordRequestFileID {
		return reply, false, nil
	}
	recognized = true
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("OpenDatabaseCoordRequest decoder panicked: %v", p)
		}
	}()
	r, err := wire.NewReader(body[off:])
	if err != nil {
		return reply, true, err
	}
	var scalar types.OpenDatabaseCoordRequest
	scalar.UnmarshalFromReader(r)
	req := types.CoordOpenDatabaseRequest{
		TraceLogGroup: scalar.TraceLogGroup, KnownClientInfoID: scalar.KnownClientInfoID,
		ClusterKey: scalar.ClusterKey, Reply: scalar.Reply, Internal: scalar.Internal,
	}
	n, err := r.ReadVectorCount(types.OpenDatabaseCoordRequestSlotIssues)
	if err != nil {
		return reply, true, err
	}
	for i := 0; i < n; i++ {
		issue, err := r.ReadVectorBytes(types.OpenDatabaseCoordRequestSlotIssues, i)
		if err != nil {
			return reply, true, err
		}
		req.Issues = append(req.Issues, issue)
	}
	for _, vector := range []struct {
		slot   int
		decode func(*wire.Reader)
	}{
		{types.OpenDatabaseCoordRequestSlotSupportedVersions, func(v *wire.Reader) {
			req.SupportedVersions = append(req.SupportedVersions, types.ClientVersionRef{ClientVersion: v.ReadBytes(0), SourceVersion: v.ReadBytes(1), ProtocolVersion: v.ReadBytes(2)})
		}},
		{types.OpenDatabaseCoordRequestSlotCoordinators, func(v *wire.Reader) {
			var addr types.NetworkAddress
			addr.UnmarshalFromReader(v)
			req.Coordinators = append(req.Coordinators, addr)
		}},
		{types.OpenDatabaseCoordRequestSlotHostnames, func(v *wire.Reader) {
			req.Hostnames = append(req.Hostnames, types.Hostname{Host: v.ReadBytes(0), Service: v.ReadBytes(1), IsTLS: v.ReadBool(2)})
		}},
	} {
		n, err := r.ReadVectorCount(vector.slot)
		if err != nil {
			return reply, true, err
		}
		for i := 0; i < n; i++ {
			element, err := r.ReadVectorElementReader(vector.slot, i)
			if err != nil {
				return reply, true, err
			}
			vector.decode(element)
		}
	}
	if again := req.MarshalFDB(); !bytes.Equal(again, body[off:]) {
		return reply, true, fmt.Errorf("OpenDatabaseCoordRequest does not re-encode to the bytes sent")
	}
	return transport.UID{First: binary.LittleEndian.Uint64(req.Reply.Token[:8]), Second: binary.LittleEndian.Uint64(req.Reply.Token[8:])}, true, nil
}

// priorNonStorageWrites returns kinds other than the four storage-read RPCs in
// storageRequest preceding the unique target reply token on its own connection
// in the latest recording window. This is not a server-role classification:
// location, GRV, watches and metrics are outside that four-kind set. The target
// itself is excluded, even when it is a location request. Decoder failures are
// errors, never evidence that an attempt may be retried. No connection lock is
// acquired here; observe takes that lock before the recorder lock.
func (r *sendRecorder) priorNonStorageWrites(target transport.UID) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if target == (transport.UID{}) {
		return nil, fmt.Errorf("zero target reply token")
	}
	at := -1
	for i, write := range r.rec.writes {
		if write.reply == target {
			if at != -1 {
				return nil, fmt.Errorf("ambiguous target reply token %x", target)
			}
			at = i
		}
	}
	if at == -1 {
		return nil, fmt.Errorf("target reply token %x was not recorded", target)
	}
	conn := r.rec.writes[at].conn
	var kinds []string
	for _, write := range r.rec.writes[:at] {
		if write.conn != conn {
			continue
		}
		if write.problem != "" {
			return nil, fmt.Errorf("preceding write on target connection: %s", write.problem)
		}
		if !storageRequest[write.kind] {
			kinds = append(kinds, write.kind)
		}
	}
	return kinds, nil
}

// validateRecordedPing decodes C++ 7.3.77's PingRequest (file identifier
// 4707015, one nested ReplyPromise<Void> with an inline UID). As with other
// recorded requests, re-encoding catches fields the permissive Reader treats as
// absent. The forced-control fixture appends zero padding, which the C++ object
// reader ignores. This fixture policy accepts only that zero padding, not
// every C++-legal layout. frame rejects embedded protocol prefixes first:
// network delivery uses AssumeVersion and does not consume a version header.
func validateRecordedPing(body []byte) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("PingRequest decoder panicked: %v", p)
		}
	}()
	r, err := wire.NewReader(body)
	if err != nil {
		return err
	}
	if r.FileIdentifier() != 4707015 {
		return fmt.Errorf("PingRequest file identifier %d, want 4707015", r.FileIdentifier())
	}
	if !r.FieldPresent(0) {
		return fmt.Errorf("PingRequest missing ReplyPromise")
	}
	reply, err := r.ReadNestedReader(0)
	if err != nil {
		return err
	}
	off, obj := reply.FieldOffset(0), reply.ObjectBytes()
	if off < 4 || off+16 > len(obj) {
		return fmt.Errorf("PingRequest ReplyPromise UID is absent or out of bounds")
	}
	token := transport.UID{First: binary.LittleEndian.Uint64(obj[off:]), Second: binary.LittleEndian.Uint64(obj[off+8:])}
	again := transport.BuildPingRequest(token)
	if len(body) < len(again) || !bytes.Equal(again, body[:len(again)]) {
		return fmt.Errorf("PingRequest does not re-encode to the bytes sent")
	}
	for _, b := range body[len(again):] {
		if b != 0 {
			return fmt.Errorf("PingRequest has a nonzero trailing byte")
		}
	}
	return nil
}

func (r *sendRecorder) frame(conn *recordingConn, token transport.UID, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.on {
		return
	}
	write := sendRecordedWrite{conn: conn, token: token, body: bytes.Clone(body)}
	// Unlike wire.Reader's generic IncludeVersion support, C++ network
	// delivery uses AssumeVersion and reads the file identifier at raw offset
	// four. Do not let any classifier normalize an invalid network prefix.
	if len(body) >= 8 && body[7] == 0x0F && body[6] == 0xDB {
		write.problem = fmt.Sprintf("embedded protocol prefix in raw network frame (token %x, %d bytes)", token, len(body))
		r.rec.writes = append(r.rec.writes, write)
		r.rec.problems = append(r.rec.problems, write.problem)
		return
	}
	if token == pingToken {
		write.kind = "PING"
		if err := validateRecordedPing(body); err != nil {
			write.problem = fmt.Sprintf("invalid PingRequest: %v (token %x, %d bytes)", err, token, len(body))
			r.rec.problems = append(r.rec.problems, write.problem)
		} else {
			r.rec.excluded++
		}
		r.rec.writes = append(r.rec.writes, write)
		return
	}
	if bytes.Equal(body, pingReplyBody) {
		write.kind = "VoidReply"
		r.rec.writes = append(r.rec.writes, write)
		r.rec.excluded++
		return
	}
	req, kind, reply, msg, err := decodeRecordedRequest(write.body, r.rec.versionName)
	write.kind, write.reply = kind, reply
	if err != nil {
		problem := fmt.Sprintf("%v (token %x, %d bytes)", err, token, len(body))
		write.problem = problem
		var strictProblem string
		if coordReply, recognized, coordErr := decodeRecordedCoordWrite(write.body); recognized {
			write.kind, write.reply, write.problem = "OpenDatabaseCoordRequest", coordReply, ""
			if coordErr != nil {
				strictProblem = fmt.Sprintf("invalid OpenDatabaseCoordRequest: %v (token %x, %d bytes)", coordErr, token, len(body))
				write.problem = strictProblem
			}
		}
		r.rec.writes = append(r.rec.writes, write)
		r.rec.problems = append(r.rec.problems, problem) // retain valid coordinator long-poll semantics
		if strictProblem != "" {
			// This must survive readProblems even if the bad frame belongs to
			// another connection or follows the retry census's target frame.
			r.rec.problems = append(r.rec.problems, strictProblem)
		}
		return
	}
	r.rec.writes = append(r.rec.writes, write)
	if storageRequest[kind] {
		r.rec.storageReplies[reply] = true
	}
	if reply != (transport.UID{}) {
		r.rec.replyKinds[reply] = kind
	}
	r.rec.requests = append(r.rec.requests, req)
	r.rec.msgs = append(r.rec.msgs, msg)
}

// messages returns the decoded requests recorded so far in this window.
func (r *sendRecorder) messages() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.rec.msgs...)
}

func (r *sendRecorder) problem(conn *recordingConn, raw []byte, p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		r.rec.problems = append(r.rec.problems, p)
		r.rec.writes = append(r.rec.writes, sendRecordedWrite{conn: conn, body: bytes.Clone(raw), problem: p})
	}
}

// readProblems is the recorder validation used by taggedRequests, including
// before a handoff fixture discards an independently observed control-write
// confound. Only the existing coordinator long-poll exclusion is filtered;
// strict coordinator and PING decoding failures remain fatal on every connection.
func (rec recording) readProblems() []string {
	coordPoll := fmt.Sprintf("unexpected request kind: file identifier %d ", types.OpenDatabaseCoordRequestFileID)
	var problems []string
	for _, p := range rec.problems {
		if !strings.HasPrefix(p, coordPoll) {
			problems = append(problems, p)
		}
	}
	return problems
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
			c.rec.problem(c, c.buf, fmt.Sprintf("malformed frame: payload length %d", n))
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
		c.rec.frame(c, token, c.buf[28:12+n])
		c.buf = c.buf[12+n:]
	}
}

// decodeRecordedRequest renders a request of an expected kind with its reply
// token, span and debug IDs left out (they identify a run, not a request) and
// versions named; the body length is kept, so a field encoded differently
// still shows. It returns the request kind, its reply token and the decoded
// request. The body must re-encode to itself: the generated decoder reads an
// out-of-bounds field as absent instead of failing, so without that check a
// malformed field of a known kind would compare as a missing one.
func decodeRecordedRequest(body []byte, versionName func(int64) string) (_, _ string, _ transport.UID, _ any, err error) {
	off := 0
	if len(body) >= 16 && body[7] == 0x0F && body[6] == 0xDB {
		off = 8 // protocol version prefix
	}
	if len(body) < off+8 {
		return "", "", transport.UID{}, nil, fmt.Errorf("undecodable frame: no file identifier")
	}
	id := binary.LittleEndian.Uint32(body[off+4 : off+8])
	mk := recordedKinds[id]
	if mk == nil {
		return "", "", transport.UID{}, nil, fmt.Errorf("unexpected request kind: file identifier %d", id)
	}
	r, err := wire.NewReader(body)
	if err != nil {
		return "", "", transport.UID{}, nil, fmt.Errorf("undecodable frame: %w", err)
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
		return "", "", transport.UID{}, nil, err
	}
	if again := m.(interface{ MarshalFDB() []byte }).MarshalFDB(); !bytes.Equal(again, body[off:]) {
		return "", "", transport.UID{}, nil, fmt.Errorf("undecodable frame: %T does not re-encode to the bytes sent (%d bytes, re-encoded %d)", m, len(body)-off, len(again))
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
	return fmt.Sprintf("%s len=%d %v", kind, len(body), fields), kind, reply, m, nil
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

func TestSendRecorderPriorNonStorageWrites(t *testing.T) {
	t.Parallel()
	target := transport.UID{First: 70, Second: 7}
	promise := func(first uint64) types.ReplyPromise {
		return types.ReplyPromise{Token: wire.UIDFromParts(first, 7)}
	}
	// Literal C++ well-known UID(-1,1) and VoidReply bytes: do not derive
	// these positive controls from the classifier's pingToken/pingReplyBody.
	voidBody, err := hex.DecodeString("200000004aad1e020000000000000400040006000600040008000900080004000800000008000000020000001e000000")
	if err != nil {
		t.Fatal(err)
	}
	type write struct {
		conn  int
		token transport.UID
		body  []byte
		raw   []byte
	}
	ping := write{token: transport.UID{First: ^uint64(0), Second: 1}, body: transport.BuildPingRequest(transport.UID{First: 8})}
	// Captured libfdb_c 7.3.77 PingRequest, independent of BuildPingRequest.
	cppPingBody, err := hex.DecodeString("14000000c7d247000600080004000600140004000c00000004000000140000000400000016000000d89e50573e31458e1b000000480ea5c5")
	if err != nil {
		t.Fatal(err)
	}
	cppPing := write{token: ping.token, body: cppPingBody}
	paddedPing := write{token: ping.token, body: append(bytes.Clone(cppPing.body), make([]byte, 1<<20)...)}
	prefixedPing := write{token: ping.token, body: append(binary.LittleEndian.AppendUint64(nil, transport.ProtocolVersion73), paddedPing.body...)}
	badPingPrefix := write{token: ping.token, body: append(binary.LittleEndian.AppendUint64(nil, 0x0FDB000000000000), cppPing.body...)}
	futurePingPrefix := write{token: ping.token, body: append(binary.LittleEndian.AppendUint64(nil, 0x0FDB00B075000000), cppPing.body...)}
	badPing := write{token: ping.token, body: []byte{1, 2, 3}}
	wrongPingKind := write{token: ping.token, body: bytes.Clone(cppPing.body)}
	binary.LittleEndian.PutUint32(wrongPingKind.body[4:8], 0xdeadbeef)
	missingPingReply := write{token: ping.token, body: bytes.Clone(cppPing.body)}
	binary.LittleEndian.PutUint16(missingPingReply.body[12:14], 0)
	badPingReply := write{token: ping.token, body: bytes.Clone(cppPing.body)}
	binary.LittleEndian.PutUint32(badPingReply.body[32:36], ^uint32(0))
	badPingUID := write{token: ping.token, body: bytes.Clone(cppPing.body[:len(cppPing.body)-1])}
	badPingLayout := write{token: ping.token, body: bytes.Clone(cppPing.body)}
	badPingLayout.body[8]++ // invalid PingRequest vtable length, still Reader-decodable
	badPingSuffix := write{token: ping.token, body: append(bytes.Clone(cppPing.body), 0, 1)}
	otherBadPing, otherPrefixedPing := badPing, prefixedPing
	otherBadPing.conn, otherPrefixedPing.conn = 1, 1
	void := write{token: transport.UID{First: 9}, body: voidBody}
	grv := write{token: transport.UID{First: 10}, body: (&types.GetReadVersionRequest{Reply: promise(60)}).MarshalFDB()}
	location := write{token: transport.UID{First: 11}, body: (&types.GetKeyServerLocationsRequest{Reply: promise(61)}).MarshalFDB()}
	coord := write{token: transport.UID{First: 20}, body: (&types.CoordOpenDatabaseRequest{
		Issues:            [][]byte{[]byte("issue")},
		SupportedVersions: []types.ClientVersionRef{{ClientVersion: []byte("7.3.77"), SourceVersion: []byte("source"), ProtocolVersion: []byte("protocol")}},
		TraceLogGroup:     []byte("test"), ClusterKey: []byte("desc:abcdef"), KnownClientInfoID: [16]byte{3},
		Coordinators: []types.NetworkAddress{{Ip: types.IPAddress{AddrTag: 1, AddrAlt0: 0x7f000001}, Port: 4500, Flags: types.FlagTLS}},
		Reply:        promise(66), Hostnames: []types.Hostname{{Host: []byte("coord.invalid"), Service: []byte("4500"), IsTLS: true}}, Internal: true,
	}).MarshalFDB()}
	prefixedCoord := coord
	prefixedCoord.body = append(binary.LittleEndian.AppendUint64(nil, transport.ProtocolVersion73), coord.body...)
	otherPrefixedCoord := prefixedCoord
	otherPrefixedCoord.conn = 1
	badCoord := coord
	badCoord.body = bytes.Clone(coord.body)
	coordReader, err := wire.NewReader(badCoord.body)
	if err != nil {
		t.Fatal(err)
	}
	vectorAt := coordReader.FieldOffset(types.OpenDatabaseCoordRequestSlotSupportedVersions)
	if vectorAt < 4 || vectorAt+4 > len(coordReader.ObjectBytes()) {
		t.Fatal("valid coordinator request omitted its supported-version vector")
	}
	binary.LittleEndian.PutUint32(coordReader.ObjectBytes()[vectorAt:], ^uint32(0))
	nonRoundTripCoord := coord
	nonRoundTripCoord.body = append(bytes.Clone(coord.body), 0xff)
	value := write{token: transport.UID{First: 12}, body: (&types.GetValueRequest{Reply: promise(62)}).MarshalFDB()}
	key := write{token: transport.UID{First: 13}, body: (&types.GetKeyRequest{Reply: promise(63)}).MarshalFDB()}
	rangeRead := write{token: transport.UID{First: 14}, body: (&types.GetKeyValuesRequest{Reply: promise(64)}).MarshalFDB()}
	mapped := write{token: transport.UID{First: 15}, body: (&types.GetMappedKeyValuesRequest{Reply: promise(65)}).MarshalFDB()}
	prefixedValue, prefixedGRV, prefixedLocation := value, grv, location
	prefixedValue.body = append(binary.LittleEndian.AppendUint64(nil, transport.ProtocolVersion73), value.body...)
	prefixedGRV.body = append(binary.LittleEndian.AppendUint64(nil, transport.ProtocolVersion73), grv.body...)
	prefixedLocation.body = append(binary.LittleEndian.AppendUint64(nil, transport.ProtocolVersion73), location.body...)
	targetWrite := write{token: transport.UID{First: 16}, body: (&types.GetKeyRequest{Reply: promise(70)}).MarshalFDB()}
	targetLocation := write{token: transport.UID{First: 17}, body: (&types.GetKeyServerLocationsRequest{Reply: promise(70)}).MarshalFDB()}
	malformed := write{token: transport.UID{First: 18}, body: malformedGetKeyValues()}
	unknown := write{token: transport.UID{First: 19}, body: []byte{0, 0, 0, 0, 0xef, 0xbe, 0xad, 0xde}}
	badFrame := write{raw: append(binary.LittleEndian.AppendUint32(nil, 1), make([]byte, 8)...)}
	otherPing, otherVoid, otherGRV, otherMalformed, duplicate := ping, void, grv, malformed, targetWrite
	otherPing.conn, otherVoid.conn, otherGRV.conn, otherMalformed.conn, duplicate.conn = 1, 1, 1, 1, 1
	otherBadCoord := badCoord
	otherBadCoord.conn = 1
	for _, tc := range []struct {
		name                         string
		writes                       []write
		target                       transport.UID
		want                         []string
		wantErr                      string
		requests, excluded, problems int
		readProblems                 int
	}{
		{name: "ping-literal-uid", writes: []write{ping, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1},
		{name: "ping-cpp-payload", writes: []write{cppPing, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1},
		{name: "ping-valid-padded-payload", writes: []write{paddedPing, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1},
		{name: "ping-exact73-prefix-predecessor", writes: []write{prefixedPing, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-exact73-prefix-other-connection-blocks-retry", writes: []write{ping, otherPrefixedPing, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "ping-exact73-prefix-post-target-blocks-retry", writes: []write{ping, targetWrite, prefixedPing}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "ping-invalid-protocol-prefix", writes: []write{badPingPrefix, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-future-protocol-prefix", writes: []write{futurePingPrefix, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "bad-ping-predecessor", writes: []write{badPing, targetWrite}, target: target, wantErr: "invalid PingRequest", requests: 1, problems: 1, readProblems: 1},
		{name: "bad-ping-other-connection-blocks-retry", writes: []write{ping, otherBadPing, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "bad-ping-post-target-blocks-retry", writes: []write{ping, targetWrite, badPing}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "ping-wrong-file-identifier", writes: []write{wrongPingKind, targetWrite}, target: target, wantErr: "file identifier", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-missing-reply", writes: []write{missingPingReply, targetWrite}, target: target, wantErr: "missing ReplyPromise", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-invalid-reply-offset", writes: []write{badPingReply, targetWrite}, target: target, wantErr: "out of bounds", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-truncated-reply-uid", writes: []write{badPingUID, targetWrite}, target: target, wantErr: "UID is absent or out of bounds", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-noncanonical-layout", writes: []write{badPingLayout, targetWrite}, target: target, wantErr: "does not re-encode", requests: 1, problems: 1, readProblems: 1},
		{name: "ping-nonzero-suffix", writes: []write{badPingSuffix, targetWrite}, target: target, wantErr: "nonzero trailing byte", requests: 1, problems: 1, readProblems: 1},
		{name: "void-literal-payload", writes: []write{void, targetWrite}, target: target, want: []string{"VoidReply"}, requests: 1, excluded: 1},
		{name: "known-nonstorage", writes: []write{grv, location, targetWrite}, target: target, want: []string{"GetReadVersionRequest", "GetKeyServerLocationsRequest"}, requests: 3},
		{name: "known-coordinator-typed-vectors", writes: []write{coord, targetWrite}, target: target, want: []string{"OpenDatabaseCoordRequest"}, requests: 1, problems: 1},
		{name: "coordinator-protocol-prefix-blocks-retry", writes: []write{prefixedCoord, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "coordinator-prefix-other-connection-blocks-retry", writes: []write{ping, otherPrefixedCoord, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "coordinator-prefix-post-target-blocks-retry", writes: []write{ping, targetWrite, prefixedCoord}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "storage-protocol-prefix-blocks-retry", writes: []write{prefixedValue, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "grv-protocol-prefix-blocks-retry", writes: []write{prefixedGRV, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "location-protocol-prefix-blocks-retry", writes: []write{prefixedLocation, targetWrite}, target: target, wantErr: "protocol prefix", requests: 1, problems: 1, readProblems: 1},
		{name: "malformed-coordinator-vector", writes: []write{badCoord, targetWrite}, target: target, wantErr: "vector start out of bounds", requests: 1, problems: 2, readProblems: 1},
		{name: "coordinator-roundtrip-rejects-trailing-bytes", writes: []write{nonRoundTripCoord, targetWrite}, target: target, wantErr: "does not re-encode", requests: 1, problems: 2, readProblems: 1},
		{name: "bad-coordinator-other-connection-blocks-retry", writes: []write{ping, otherBadCoord, targetWrite}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 2, readProblems: 1},
		{name: "bad-coordinator-post-target-blocks-retry", writes: []write{ping, targetWrite, badCoord}, target: target, want: []string{"PING"}, requests: 1, excluded: 1, problems: 2, readProblems: 1},
		{name: "other-connection", writes: []write{otherPing, otherVoid, otherGRV, value, targetWrite}, target: target, requests: 3, excluded: 2},
		{name: "post-target", writes: []write{targetWrite, ping, void, grv}, target: target, requests: 2, excluded: 2},
		{name: "storage-only", writes: []write{value, key, rangeRead, mapped, targetWrite}, target: target, requests: 5},
		{name: "target-location-is-not-its-own-confound", writes: []write{value, targetLocation}, target: target, requests: 2},
		{name: "missing-target", writes: []write{ping, value}, target: target, wantErr: "was not recorded", requests: 1, excluded: 1},
		{name: "zero-target", writes: []write{targetWrite}, wantErr: "zero target", requests: 1},
		{name: "duplicate-target", writes: []write{targetWrite, duplicate}, target: target, wantErr: "ambiguous target", requests: 2},
		{name: "malformed-body-is-not-a-retry-confound", writes: []write{ping, malformed, targetWrite}, target: target, wantErr: "does not re-encode", requests: 1, excluded: 1, problems: 1, readProblems: 1},
		{name: "unknown-body-is-not-a-retry-confound", writes: []write{unknown, targetWrite}, target: target, wantErr: "unexpected request kind", requests: 1, problems: 1, readProblems: 1},
		{name: "bad-framing", writes: []write{badFrame, targetWrite}, target: target, wantErr: "malformed frame: payload length 1", requests: 1, problems: 1, readProblems: 1},
		{name: "malformed-other-connection-stays-a-recorder-problem", writes: []write{otherMalformed, targetWrite}, target: target, requests: 1, problems: 1, readProblems: 1},
		{name: "malformed-post-target-stays-a-recorder-problem", writes: []write{targetWrite, malformed}, target: target, requests: 1, problems: 1, readProblems: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &sendRecorder{}
			first, removeFirst := rec.parser()
			defer removeFirst()
			second, removeSecond := rec.parser()
			defer removeSecond()
			parsers := []*recordingConn{first, second}
			rec.start()
			for _, w := range tc.writes {
				raw := w.raw
				if raw == nil {
					raw = recorderFrame(w.token, w.body)
				}
				parsers[w.conn].observe(raw)
			}
			check := func() {
				t.Helper()
				got, err := rec.priorNonStorageWrites(tc.target)
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) || len(got) != 0 {
						t.Fatalf("prior writes = %q, %v; want no retry evidence and error containing %q", got, err, tc.wantErr)
					}
				} else if err != nil || !slices.Equal(got, tc.want) {
					t.Fatalf("prior writes = %q, %v; want %q", got, err, tc.want)
				}
			}
			check()
			got := rec.stop()
			check() // retain the same census after recording stops
			if problems := got.readProblems(); len(problems) != tc.readProblems {
				t.Fatalf("actual read/retry validation returned %q, want %d blocking problems", problems, tc.readProblems)
			}
			if len(got.requests) != tc.requests || len(got.msgs) != tc.requests || got.excluded != tc.excluded || len(got.problems) != tc.problems {
				t.Fatalf("recorder semantics changed: requests=%d messages=%d excluded=%d problems=%q, want %d/%d/%d/%d", len(got.requests), len(got.msgs), got.excluded, got.problems, tc.requests, tc.requests, tc.excluded, tc.problems)
			}
			coordReply := transport.UID{First: 66, Second: 7}
			if got.replyKinds[coordReply] != "" || got.storageReplies[coordReply] {
				t.Fatal("history-only coordinator classification changed the original reply maps")
			}
			if len(got.writes) != len(tc.writes) {
				t.Fatalf("retained %d raw writes, want %d", len(got.writes), len(tc.writes))
			}
			for i, w := range tc.writes {
				body := w.body
				if w.raw != nil {
					body = w.raw
				}
				if got.writes[i].conn != parsers[w.conn] || got.writes[i].token != w.token || !bytes.Equal(got.writes[i].body, body) {
					t.Errorf("raw write %d lost connection, token, or bytes", i)
				}
			}
		})
	}
}

func TestSendRecorderPriorWritesStayWithinWindow(t *testing.T) {
	t.Parallel()
	rec := &sendRecorder{}
	parser, remove := rec.parser()
	defer remove()
	ping := recorderFrame(transport.UID{First: ^uint64(0), Second: 1}, transport.BuildPingRequest(transport.UID{First: 8}))
	target := transport.UID{First: 70, Second: 7}
	request := recorderFrame(transport.UID{First: 9}, (&types.GetKeyRequest{Reply: types.ReplyPromise{Token: wire.UIDFromParts(target.First, target.Second)}}).MarshalFDB())
	parser.observe(ping) // outside a recording window
	rec.start()
	parser.observe(request)
	rec.stop()
	parser.observe(ping) // after stop, also outside the window
	if got, err := rec.priorNonStorageWrites(target); err != nil || len(got) != 0 {
		t.Fatalf("out-of-window upkeep entered the census: %q, %v", got, err)
	}
	rec.start()
	parser.observe(ping)
	if got, err := rec.priorNonStorageWrites(target); err == nil || len(got) != 0 {
		t.Fatalf("previous window's target survived start: %q, %v", got, err)
	}
	parser.observe(request)
	if got, err := rec.priorNonStorageWrites(target); err != nil || !slices.Equal(got, []string{"PING"}) {
		t.Fatalf("new window's preceding upkeep was lost: %q, %v", got, err)
	}
	rec.stop()
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
