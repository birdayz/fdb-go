package wiretrace

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
	"github.com/zeebo/xxh3"
)

// vector is a C++-encoded message from //pkg/fdbgo/wire/types:testdata.json.
type vector struct {
	Name       string `json:"name"`
	Hex        string `json:"hex"`
	ReplyToken string `json:"reply_token"`
}

func loadVectors(t *testing.T) map[string]vector {
	t.Helper()
	data, err := os.ReadFile("../../../../pkg/fdbgo/wire/types/testdata.json")
	if err != nil {
		t.Fatalf("testdata.json (bazel data dependency): %v", err)
	}
	var list []vector
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	m := map[string]vector{}
	for _, v := range list {
		m[v.Name] = v
	}
	return m
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// capture builds a classic nanosecond pcap of Ethernet/IPv4/TCP packets.
type capture struct{ bytes.Buffer }

func newCapture() *capture {
	c := &capture{}
	var h [24]byte
	binary.LittleEndian.PutUint32(h[0:], pcapMagicNano)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 262144)
	binary.LittleEndian.PutUint32(h[20:], linkEthernet)
	c.Write(h[:])
	return c
}

func (c *capture) packet(ts int64, f Flow, seq uint32, syn bool, payload []byte) {
	var flags byte
	if syn {
		flags = 0x02
	}
	c.segment(ts, f, seq, flags, payload)
}

// fin captures a FIN segment.
func (c *capture) fin(ts int64, f Flow, seq uint32, payload []byte) {
	c.segment(ts, f, seq, 0x01, payload)
}

func (c *capture) segment(ts int64, f Flow, seq uint32, flags byte, payload []byte) {
	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:], f.Src.Port)
	binary.BigEndian.PutUint16(tcp[2:], f.Dst.Port)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	tcp[12] = 5 << 4
	tcp[13] = 0x10 | flags // ACK
	copy(tcp[20:], payload)
	ip := make([]byte, 20+len(tcp))
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	ip[9] = 6
	copy(ip[12:], f.Src.IP[:])
	copy(ip[16:], f.Dst.IP[:])
	copy(ip[20:], tcp)
	frame := append(make([]byte, 12), 0x08, 0x00)
	frame = append(frame, ip...)
	var rec [16]byte
	binary.LittleEndian.PutUint32(rec[0:], uint32(ts/1e9))
	binary.LittleEndian.PutUint32(rec[4:], uint32(ts%1e9))
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(frame)))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(frame)))
	c.Write(rec[:])
	c.Write(frame)
}

// connectPacket is a ConnectPacket's length-prefixed shape (44 bytes).
func connectPacket() []byte {
	b := make([]byte, 44)
	binary.LittleEndian.PutUint32(b, 40)
	return b
}

// fdbFrame is a plaintext FDB packet: length, XXH3-64 checksum, token, body.
func fdbFrame(token, body []byte) []byte {
	b := make([]byte, 12, 12+16+len(body))
	b = append(append(b, token...), body...)
	binary.LittleEndian.PutUint32(b, uint32(16+len(body)))
	binary.LittleEndian.PutUint64(b[4:], xxh3.Hash(b[12:]))
	return b
}

func TestDecodeMatchesRequestsWithReplies(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	grvReq, commitReq := v["GetReadVersionRequest_causal_risky"], v["CommitTransactionRequest_single_set"]
	other := v["GetKeyServerLocationsRequest_basic"]
	grvTok, commitTok := unhex(t, grvReq.ReplyToken), unhex(t, commitReq.ReplyToken)
	proxyTok := bytes.Repeat([]byte{7}, 16)

	lo := [4]byte{127, 0, 0, 1}
	client, server := Endpoint{lo, 50000}, Endpoint{lo, 4721}
	up, down := Flow{client, server}, Flow{server, client}
	c := newCapture()
	c.packet(10, up, 100, true, nil)
	c.packet(20, down, 900, true, nil)
	seq := uint32(101)
	first := append(connectPacket(), fdbFrame(proxyTok, unhex(t, grvReq.Hex))...)
	c.packet(1000, up, seq, false, first)
	seq += uint32(len(first))
	commit := fdbFrame(proxyTok, unhex(t, commitReq.Hex))
	half := len(commit) / 2
	c.packet(2500, up, seq+uint32(half), false, commit[half:]) // captured out of order
	c.packet(3000, up, seq, false, commit[:half])
	c.packet(3500, up, seq, false, commit[:half]) // retransmission
	seq += uint32(len(commit))
	c.packet(3600, up, seq, false, fdbFrame(proxyTok, unhex(t, other.Hex))) // not followed

	reply := append(connectPacket(), fdbFrame(grvTok, unhex(t, v["GetReadVersionReply_locked"].Hex))...)
	c.packet(4000, down, 901, false, reply)
	c.packet(5000, down, 901+uint32(len(reply)), false, fdbFrame(commitTok, unhex(t, v["CommitID_committed"].Hex)))
	// A connection whose SYN preceded the capture is ignored.
	c.packet(6000, Flow{Endpoint{lo, 50001}, server}, 7, false, fdbFrame(proxyTok, unhex(t, grvReq.Hex)))

	trace, done, err := Decode(&c.Buffer, []uint16{4721})
	if err != nil || !done.Complete() {
		t.Fatalf("Decode = %v, %+v", err, done)
	}
	if len(trace.Requests) != 2 {
		t.Fatalf("%d requests, want the GRV and the commit", len(trace.Requests))
	}
	r, err := wire.NewReader(unhex(t, grvReq.Hex))
	if err != nil {
		t.Fatal(err)
	}
	var want types.GetReadVersionRequest
	if err := want.UnmarshalFromReader(r); err != nil {
		t.Fatal(err)
	}
	grv, cm := trace.Requests[0], trace.Requests[1]
	if grv.Kind != KindGRV || grv.SentNS != 1000 || grv.ReplyNS != 4000 || grv.Version != 0x123456789a || grv.TxnCount != want.TransactionCount {
		t.Fatalf("GRV = %+v", grv)
	}
	// The commit is complete only once its first half arrives.
	if cm.Kind != KindCommit || cm.SentNS != 3000 || cm.ReplyNS != 5000 || cm.Version != 0x0abcdef012 {
		t.Fatalf("commit = %+v", cm)
	}
	s := trace.Summarize()
	g, k := s.Kinds[KindGRV], s.Kinds[KindCommit]
	if s.Undecodable != 0 || s.Flows != 1 || g.Replied != 1 || g.ServerBatches != 0 || k.Replied != 1 || k.ServerBatches != 1 || k.ClientSegments != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if k.LatencyNS[0] != 2000 {
		t.Fatalf("commit latency %d ns, want 2000", k.LatencyNS[0])
	}
	var events strings.Builder
	if err := trace.WriteEvents(&events); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(events.String(), "\n"); lines != 3 {
		t.Fatalf("%d event lines, want a header and two requests:\n%s", lines, events.String())
	}
}

// A storage range read is matched with its reply: the request's parameters and
// the reply's size and More flag are recorded.
func TestDecodeRangeReads(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	req, reply := v["GetKeyValuesRequest_basic"], v["GetKeyValuesReply_two_rows"]
	lo := [4]byte{127, 0, 0, 1}
	up := Flow{Endpoint{lo, 50002}, Endpoint{lo, 4723}}
	down := Flow{up.Dst, up.Src}
	c := newCapture()
	c.packet(10, up, 100, true, nil)
	c.packet(20, down, 900, true, nil)
	c.packet(1000, up, 101, false, append(connectPacket(), fdbFrame(bytes.Repeat([]byte{9}, 16), unhex(t, req.Hex))...))
	c.packet(2000, down, 901, false, append(connectPacket(), fdbFrame(unhex(t, req.ReplyToken), unhex(t, reply.Hex))...))
	trace, done, err := Decode(&c.Buffer, []uint16{4723})
	if err == nil && !done.Complete() {
		t.Fatalf("incomplete: %+v", done)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Requests) != 1 {
		t.Fatalf("%d requests, want the range read", len(trace.Requests))
	}
	r := trace.Requests[0]
	if r.Kind != KindRange || r.ReplyNS != 2000 || !r.More || r.ReplyBytes == 0 || !strings.HasPrefix(r.Params, "limit=") {
		t.Fatalf("range read = %+v", r)
	}
	k := trace.Summarize().Kinds[KindRange]
	if k.Requests != 1 || k.More != 1 || len(k.Params) != 1 || k.ServerBatches != 0 {
		t.Fatalf("range summary = %+v", k)
	}
}

// streamCapture builds one client connection to port 4721 whose client data,
// after the ConnectPacket, is the given frames in the given segments.
type streamCapture struct {
	*capture
	up  Flow
	seq uint32
}

func newStreamCapture(clientPort uint16, isn uint32) *streamCapture {
	lo := [4]byte{127, 0, 0, 1}
	c := &streamCapture{capture: newCapture(), up: Flow{Endpoint{lo, clientPort}, Endpoint{lo, 4721}}, seq: isn + 1}
	c.packet(1, c.up, isn, true, nil)
	return c
}

func grvFrames(t *testing.T, n int) [][]byte {
	t.Helper()
	v := loadVectors(t)
	var frames [][]byte
	for i := range n {
		body := unhex(t, v["GetReadVersionRequest_causal_risky"].Hex)
		r, err := wire.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		var m types.GetReadVersionRequest
		if err := m.UnmarshalFromReader(r); err != nil {
			t.Fatal(err)
		}
		m.Reply.Token[0] = byte(i + 1) // distinct reply tokens
		frames = append(frames, fdbFrame(bytes.Repeat([]byte{7}, 16), m.MarshalFDB()))
	}
	return frames
}

// A missing segment that is never retransmitted, and a frame cut off by the end
// of the stream, make the capture incomplete.
func TestDecodeReportsIncompleteStreams(t *testing.T) {
	t.Parallel()
	frames := grvFrames(t, 2)
	data := append(connectPacket(), frames[0]...)

	hole := newStreamCapture(50010, 100)
	hole.packet(10, hole.up, hole.seq, false, data)
	hole.packet(20, hole.up, hole.seq+uint32(len(data))+5, false, frames[1]) // 5 bytes never captured
	if _, done, err := Decode(&hole.Buffer, []uint16{4721}); err != nil || done.Gaps != 1 || done.Complete() {
		t.Fatalf("WIRETRACE_HOLE_UNREPORTED: Decode = %v, %+v", err, done)
	}

	cut := newStreamCapture(50011, 100)
	cut.packet(10, cut.up, cut.seq, false, append(data, frames[1][:20]...))
	if _, done, err := Decode(&cut.Buffer, []uint16{4721}); err != nil || len(done.Partial) != 1 || done.Complete() {
		t.Fatalf("WIRETRACE_PARTIAL_UNREPORTED: Decode = %v, %+v", err, done)
	}
}

// A FIN beyond the delivered data means the last segments were never
// captured, even when the delivered data ends at a frame boundary.
func TestDecodeReportsMissingTail(t *testing.T) {
	t.Parallel()
	frames := grvFrames(t, 2)
	data := append(connectPacket(), frames[0]...)
	for _, tc := range []struct {
		name    string
		finSeq  uint32
		payload []byte
		gaps    int
	}{
		{"missing tail", uint32(len(data) + len(frames[1])), nil, 1},
		{"clean close", uint32(len(data)), nil, 0},
		{"data with FIN", uint32(len(data)), frames[1], 0},
	} {
		c := newStreamCapture(50017, 100)
		c.packet(10, c.up, c.seq, false, data)
		c.fin(20, c.up, c.seq+tc.finSeq, tc.payload)
		c.fin(30, c.up, c.seq+tc.finSeq, tc.payload) // retransmitted FIN
		_, done, err := Decode(&c.Buffer, []uint16{4721})
		if err != nil || done.Gaps != tc.gaps || done.Complete() != (tc.gaps == 0) {
			t.Fatalf("WIRETRACE_TAIL_HOLE %s: Decode = %v, %+v", tc.name, err, done)
		}
	}
}

// A reply readable before its request (the request's first segment captured
// late) is still matched once the request decodes.
func TestDecodeMatchesEarlyReply(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	req := v["CommitTransactionRequest_single_set"]
	lo := [4]byte{127, 0, 0, 1}
	client, server := Endpoint{lo, 50019}, Endpoint{lo, 4721}
	up, down := Flow{client, server}, Flow{server, client}
	c := newCapture()
	c.packet(1, up, 100, true, nil)
	c.packet(2, down, 900, true, nil)
	data := append(connectPacket(), fdbFrame(bytes.Repeat([]byte{7}, 16), unhex(t, req.Hex))...)
	c.packet(10, up, 101+20, false, data[20:]) // behind a gap
	c.packet(20, down, 901, false, append(connectPacket(), fdbFrame(unhex(t, req.ReplyToken), unhex(t, v["CommitID_committed"].Hex))...))
	c.packet(30, up, 101, false, data[:20]) // fills the gap
	trace, done, err := Decode(&c.Buffer, []uint16{4721})
	if err != nil || !done.Complete() || len(trace.Requests) != 1 {
		t.Fatalf("Decode = %v, %+v, %d requests", err, done, len(trace.Requests))
	}
	if r := trace.Requests[0]; r.ReplyNS != 20 || r.Version != 0x0abcdef012 {
		t.Fatalf("WIRETRACE_EARLY_REPLY_LOST: request = %+v", r)
	}
	// Matched and counted, but its reply precedes its readable request, so it
	// is not a latency sample.
	if k := trace.Summarize().Kinds[KindCommit]; k.Replied != 1 || k.ServerBatches != 1 || k.ReorderedReplies != 1 || k.LatencyNS != [3]int64{} {
		t.Fatalf("WIRETRACE_EARLY_REPLY_LATENCY: summary = %+v", k)
	}
}

// A capture without a decoded request is not a measurement.
func TestDecodeRejectsEmptyCaptures(t *testing.T) {
	t.Parallel()
	if _, done, err := Decode(&newCapture().Buffer, []uint16{4721}); err != nil || done.Complete() {
		t.Fatalf("WIRETRACE_EMPTY_COMPLETE header only: Decode = %v, %+v", err, done)
	}
	c := newStreamCapture(50018, 100)
	c.packet(10, c.up, c.seq, false, append(connectPacket(), grvFrames(t, 1)[0]...))
	if _, done, err := Decode(&c.Buffer, []uint16{4722}); err != nil || done.Complete() {
		t.Fatalf("WIRETRACE_EMPTY_COMPLETE wrong port: Decode = %v, %+v", err, done)
	}
}

// Frames that become readable together carry the capture index of the segment
// that made them readable; a retransmission overlapping delivered data adds no
// frame.
func TestDecodeSegmentIdentity(t *testing.T) {
	t.Parallel()
	frames := grvFrames(t, 3)
	c := newStreamCapture(50012, 100)
	first := append(connectPacket(), frames[0]...)
	c.packet(10, c.up, c.seq, false, first[:30]) // packet 2
	gap := c.seq + 30
	rest := append(first[30:], frames[1]...)
	c.packet(20, c.up, gap+uint32(len(first)-30), false, frames[1]) // packet 3: arrives before the gap fills
	c.packet(30, c.up, gap, false, rest)                            // packet 4: fills the gap, completes both
	end := gap + uint32(len(rest))
	c.packet(40, c.up, end-10, false, append(rest[len(rest)-10:], frames[2]...)) // packet 5: overlaps 10 delivered bytes
	trace, done, err := Decode(&c.Buffer, []uint16{4721})
	if err != nil || !done.Complete() {
		t.Fatalf("Decode = %v, %+v", err, done)
	}
	if len(trace.Requests) != 3 {
		t.Fatalf("%d requests, want 3", len(trace.Requests))
	}
	if a, b, d := trace.Requests[0].Segment, trace.Requests[1].Segment, trace.Requests[2].Segment; a != 4 || b != 4 || d != 5 {
		t.Fatalf("WIRETRACE_SEGMENT_IDENTITY: segments %d, %d, %d, want 4, 4, 5", a, b, d)
	}
	if k := trace.Summarize().Kinds[KindGRV]; k.ClientSegments != 2 {
		t.Fatalf("%d client segments, want 2", k.ClientSegments)
	}
}

// A shorter retransmission of buffered data keeps the longer copy, so the
// stream still reassembles when the gap before it fills.
func TestDecodeShorterRetransmission(t *testing.T) {
	t.Parallel()
	frames := grvFrames(t, 1)
	c := newStreamCapture(50014, 100)
	data := append(connectPacket(), frames[0]...)
	c.packet(10, c.up, c.seq+10, false, data[10:])   // buffered behind a gap
	c.packet(20, c.up, c.seq+10, false, data[10:20]) // shorter retransmission
	c.packet(30, c.up, c.seq, false, data[:10])      // fills the gap
	trace, done, err := Decode(&c.Buffer, []uint16{4721})
	if err != nil || !done.Complete() || len(trace.Requests) != 1 {
		t.Fatalf("WIRETRACE_RETRANSMIT_LOSS: Decode = %v, %+v, %d requests", err, done, len(trace.Requests))
	}
}

// A connection that is not to a server port is not FDB traffic and must not
// break decoding.
func TestDecodeSkipsOtherConnections(t *testing.T) {
	t.Parallel()
	frames := grvFrames(t, 1)
	c := newStreamCapture(50015, 100)
	c.packet(10, c.up, c.seq, false, append(connectPacket(), frames[0]...))
	lo := [4]byte{127, 0, 0, 1}
	other := Flow{Endpoint{lo, 50016}, Endpoint{lo, 8080}}
	c.packet(20, other, 500, true, nil)
	c.packet(30, other, 501, false, []byte("GET / HTTP/1.1\r\n\r\n"))
	trace, done, err := Decode(&c.Buffer, []uint16{4721})
	if err != nil || !done.Complete() || len(trace.Requests) != 1 {
		t.Fatalf("WIRETRACE_OTHER_TRAFFIC: Decode = %v, %+v, %d requests", err, done, len(trace.Requests))
	}
}

// A retransmitted SYN keeps the connection; a new SYN on the same 4-tuple
// starts a new one, whose ConnectPacket is not mistaken for a frame.
func TestDecodeConnectionGenerations(t *testing.T) {
	t.Parallel()
	frames := grvFrames(t, 2)
	c := newStreamCapture(50013, 100)
	first := append(connectPacket(), frames[0]...)
	c.packet(10, c.up, c.seq, false, first[:20])
	c.packet(15, c.up, 100, true, nil) // retransmitted SYN
	c.packet(20, c.up, c.seq+20, false, first[20:])
	c.packet(30, c.up, 5000, true, nil) // new connection, same 4-tuple
	c.packet(40, c.up, 5001, false, append(connectPacket(), frames[1]...))
	trace, done, err := Decode(&c.Buffer, []uint16{4721})
	if err != nil || !done.Complete() {
		t.Fatalf("WIRETRACE_CONN_GENERATION: Decode = %v, %+v", err, done)
	}
	if len(trace.Requests) != 2 {
		t.Fatalf("%d requests, want one per connection", len(trace.Requests))
	}
}

// A commit reply of version -1 is an in-band conflict and a reply that does not
// decode is malformed; neither counts as a successful commit or forms a batch.
func TestDecodeFailedCommitReplies(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	commitReq := v["CommitTransactionRequest_single_set"]
	lo := [4]byte{127, 0, 0, 1}
	// A committed reply whose version lies past the buffer does not decode.
	pastVersion := unhex(t, v["CommitID_committed"].Hex)
	var r wire.Reader
	if err := wire.ReadErrorOrInto(pastVersion, &r); err != nil {
		t.Fatal(err)
	}
	vtable := r.ObjectPos() - int(int32(binary.LittleEndian.Uint32(pastVersion[r.ObjectPos():])))
	binary.LittleEndian.PutUint16(pastVersion[vtable+4+2*types.CommitIDSlotVersion:], 0xFFF0)
	for _, tc := range []struct {
		name  string
		reply []byte
	}{
		{"conflict", unhex(t, v["CommitID_conflict"].Hex)},
		{"malformed", []byte{1, 2, 3}},
		{"version past the buffer", pastVersion},
	} {
		up := Flow{Endpoint{lo, 50020}, Endpoint{lo, 4721}}
		down := Flow{up.Dst, up.Src}
		c := newCapture()
		c.packet(1, up, 100, true, nil)
		c.packet(2, down, 900, true, nil)
		c.packet(10, up, 101, false, append(connectPacket(), fdbFrame(bytes.Repeat([]byte{7}, 16), unhex(t, commitReq.Hex))...))
		c.packet(20, down, 901, false, append(connectPacket(), fdbFrame(unhex(t, commitReq.ReplyToken), tc.reply)...))
		trace, done, err := Decode(&c.Buffer, []uint16{4721})
		if err != nil || !done.Complete() {
			t.Fatalf("%s: Decode = %v, %+v", tc.name, err, done)
		}
		k := trace.Summarize().Kinds[KindCommit]
		if k.ServerBatches != 0 || k.Errors+k.Malformed != 1 || trace.CommitRounds(1).Whole != 0 {
			t.Fatalf("WIRETRACE_FAILED_COMMIT_COUNTED: %s reply summarized as %+v", tc.name, k)
		}
		// The events file marks the failure, so later tools can exclude it too.
		var events strings.Builder
		if err := trace.WriteEvents(&events); err != nil {
			t.Fatal(err)
		}
		row := strings.Split(strings.Split(events.String(), "\n")[1], ",")
		if failed := row[7] != "0" || row[8] == "true"; !failed {
			t.Fatalf("WIRETRACE_EVENT_UNMARKED: %s reply exported as %v", tc.name, row)
		}
	}
}

// Requests answered with one version form one server batch.
func TestSummarizeGroupsBatchesByVersion(t *testing.T) {
	t.Parallel()
	tr := NewTrace(nil)
	flow := Flow{Endpoint{Port: 1}, Endpoint{Port: 2}}
	for i, ver := range []int64{10, 10, 10, 20} {
		tr.Requests = append(tr.Requests, &Request{Kind: KindCommit, Flow: flow, SentNS: int64(100 * i), Segment: i / 2, ReplyNS: 1000, Version: ver, TxnCount: 1})
	}
	tr.Requests = append(tr.Requests, &Request{Kind: KindCommit, Flow: flow, Segment: 9, TxnCount: 1}) // unanswered
	k := tr.Summarize().Kinds[KindCommit]
	if k.Requests != 5 || k.Replied != 4 || k.ServerBatches != 2 || k.BatchSizes[3] != 1 || k.BatchSizes[1] != 1 || k.ClientSegments != 3 {
		t.Fatalf("summary = %+v", k)
	}
	if k.ArrivalSpreadNS[2] != 200 {
		t.Fatalf("largest arrival spread %d ns, want 200", k.ArrivalSpreadNS[2])
	}
}

// Batches in order of their first request: whole rounds, then a split that
// persists, as a closed loop shows once a round splits.
func TestCommitRounds(t *testing.T) {
	t.Parallel()
	tr := NewTrace(nil)
	sent := int64(0)
	for v, size := range []int{1, 4, 4, 3, 1, 3, 1, 4} { // setup commit, 2 whole, 2 split rounds, 1 whole
		for range size {
			sent++
			tr.Requests = append(tr.Requests, &Request{Kind: KindCommit, SentNS: sent, ReplyNS: sent + 100, Version: int64(v + 1)})
		}
	}
	r := tr.CommitRounds(4)
	if r.Whole != 3 || r.Split != 5 || r.FirstSplit != 3 || fmt.Sprint(r.Runs) != "[0 1 2 4 1]" {
		t.Fatalf("rounds = %+v", r)
	}
}

// Batches whose first requests share a capture time are ordered by version,
// so the classification does not depend on map order.
func TestCommitRoundsTiedTimes(t *testing.T) {
	t.Parallel()
	tr := NewTrace(nil)
	for v, size := range []int{2, 1, 2} { // whole, split, whole: all sent at once
		for range size {
			tr.Requests = append(tr.Requests, &Request{Kind: KindCommit, SentNS: 5, ReplyNS: 100, Version: int64(v + 1)})
		}
	}
	for range 64 {
		if r := tr.CommitRounds(2); r.FirstSplit != 1 || fmt.Sprint(r.Runs) != "[1 1 1]" {
			t.Fatalf("WIRETRACE_TIE_ORDER: rounds = %+v", r)
		}
	}
}

func TestReadPcapRejectsTruncatedPackets(t *testing.T) {
	t.Parallel()
	c := newCapture()
	c.packet(1, Flow{Endpoint{Port: 1}, Endpoint{Port: 2}}, 1, true, nil)
	b := c.Bytes()
	binary.LittleEndian.PutUint32(b[24+8:], 10) // captured length below the original
	if err := ReadPcap(bytes.NewReader(b[:24+16+10]), func(Segment) error { return nil }); err == nil || !strings.Contains(err.Error(), "-s 0") {
		t.Fatalf("ReadPcap = %v, want a truncation error", err)
	}
}

func TestFrameParserRejectsBadChecksum(t *testing.T) {
	t.Parallel()
	frame := fdbFrame(make([]byte, 16), []byte("body"))
	frame[len(frame)-1] ^= 1
	p := NewFrameParser(Flow{}, func(Frame) error { return nil })
	if err := p.Add(Chunk{Data: append(connectPacket(), frame...)}); err == nil {
		t.Fatal("WIRETRACE_CHECKSUM: a frame with a wrong checksum was accepted")
	}
}

func TestFrameParserRejectsCorruptLength(t *testing.T) {
	t.Parallel()
	p := NewFrameParser(Flow{}, func(Frame) error { return nil })
	bad := append(connectPacket(), make([]byte, 12)...) // a frame claiming 0 payload bytes
	if err := p.Add(Chunk{Data: bad}); err == nil {
		t.Fatal("a frame shorter than its token was accepted")
	}
}

// pastBuffer points slot of the message in d past the end of the buffer; reply
// selects an ErrorOr reply over a request.
func pastBuffer(t *testing.T, d []byte, reply bool, slot int) []byte {
	t.Helper()
	d = bytes.Clone(d)
	var obj int
	if reply {
		var r wire.Reader
		if err := wire.ReadErrorOrInto(d, &r); err != nil {
			t.Fatal(err)
		}
		obj = r.ObjectPos()
	} else {
		r, err := wire.NewReader(d)
		if err != nil {
			t.Fatal(err)
		}
		obj = r.ObjectPos()
	}
	vtable := obj - int(int32(binary.LittleEndian.Uint32(d[obj:])))
	binary.LittleEndian.PutUint16(d[vtable+4+2*slot:], 0xFFF0)
	return d
}

// A request or reply with a field past the buffer is undecodable; a request is
// dropped and a reply marks its request malformed.
func TestDecodeFieldsPastBuffer(t *testing.T) {
	t.Parallel()
	v := loadVectors(t)
	lo := [4]byte{127, 0, 0, 1}
	kinds := []struct {
		req, reply         string
		reqSlot, replySlot int
	}{
		{"GetReadVersionRequest_causal_risky", "GetReadVersionReply_locked", types.GetReadVersionRequestSlotTransactionCount, types.GetReadVersionReplySlotVersion},
		{"CommitTransactionRequest_single_set", "CommitID_committed", types.CommitTransactionRequestSlotTransaction, types.CommitIDSlotVersion},
		{"GetKeyValuesRequest_basic", "GetKeyValuesReply_two_rows", types.GetKeyValuesRequestSlotBegin, types.GetKeyValuesReplySlotVersion},
	}
	for i, k := range kinds {
		req, reply := unhex(t, v[k.req].Hex), unhex(t, v[k.reply].Hex)
		for _, badReply := range []bool{false, true} {
			sent, answer := pastBuffer(t, req, false, k.reqSlot), reply
			if badReply {
				sent, answer = req, pastBuffer(t, reply, true, k.replySlot)
			}
			up := Flow{Endpoint{lo, uint16(50100 + 2*i)}, Endpoint{lo, 4721}}
			down := Flow{up.Dst, up.Src}
			c := newCapture()
			c.packet(1, up, 100, true, nil)
			c.packet(2, down, 900, true, nil)
			c.packet(10, up, 101, false, append(connectPacket(), fdbFrame(bytes.Repeat([]byte{7}, 16), sent)...))
			c.packet(20, down, 901, false, append(connectPacket(), fdbFrame(unhex(t, v[k.req].ReplyToken), answer)...))
			trace, _, err := Decode(&c.Buffer, []uint16{4721})
			if err != nil {
				t.Fatal(err)
			}
			if trace.Undecodable != 1 {
				t.Errorf("%s, bad reply %t: %d undecodable, want 1", k.req, badReply, trace.Undecodable)
			}
			if badReply && (len(trace.Requests) != 1 || !trace.Requests[0].Malformed) {
				t.Errorf("%s: a reply past the buffer left %d requests, want one malformed", k.req, len(trace.Requests))
			}
			if !badReply && len(trace.Requests) != 0 {
				t.Errorf("%s: a request past the buffer was kept", k.req)
			}
		}
	}
}
