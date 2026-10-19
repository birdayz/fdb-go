package wiregolden

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/wire/types"
	"github.com/zeebo/xxh3"
)

func marker(name string) Request {
	return Request{Kind: "GetValueRequest", Msg: &types.GetValueRequest{Key: []byte(MarkerPrefix + name)}}
}

func get(key string, version int64) Request {
	m := &types.GetValueRequest{Key: []byte(key), Version: version}
	m.Reply.Token[0] = 7     // a reply token differs per run
	m.SpanContext.SpanID = 9 // so does the span
	return Request{Kind: "GetValueRequest", Msg: m}
}

func commitReq(m *types.CommitTransactionRequest) Request {
	return Request{Kind: "CommitTransactionRequest", Msg: m}
}

func transcript(t *testing.T, reqs []Request, want []string) string {
	t.Helper()
	got, err := Transcript(reqs, "p/", want)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	return got
}

// Setup and requests after ~end are left out; each scenario numbers its
// versions from V1; the key prefix, reply tokens and spans are normalized.
func TestTranscriptSplitsAndNormalizes(t *testing.T) {
	reqs := []Request{
		get("p/setup", 100), // before the first marker
		marker("one"),
		get("p/a", 500), get("p/b", 500), get("p/c", 600),
		marker("two"),
		get("p/a", 900),
		marker("~end"),
		get("p/after", 1000),
	}
	got := transcript(t, reqs, []string{"one", "two"})
	want := `== one
GetValueRequest {Key='<p>a' Version=V1}
GetValueRequest {Key='<p>b' Version=V1}
GetValueRequest {Key='<p>c' Version=V2}
== two
GetValueRequest {Key='<p>a' Version=V1}
`
	if got != want {
		t.Fatalf("WIREGOLDEN_TRANSCRIPT:\n%s\nwant\n%s", got, want)
	}
}

func scKey(b byte) string { return selfConflictPrefix + strings.Repeat(string([]byte{b}), 16) }

// Self-conflicting keys keep their identity: the same random bytes get the same
// name, different bytes different names.
func TestTranscriptSelfConflictIdentity(t *testing.T) {
	same, other := scKey(0x9f), scKey(0x01)
	render := func(read, write string) string {
		m := &types.CommitTransactionRequest{}
		m.Transaction.ReadConflictRanges = []types.KeyRangeRef{{Begin: []byte(read), End: []byte(read + "\x00")}}
		m.Transaction.WriteConflictRanges = []types.KeyRangeRef{{Begin: []byte(write), End: []byte(write + "\x00")}}
		return transcript(t, []Request{marker("sc"), commitReq(m), marker("~end")}, []string{"sc"})
	}
	want := "== sc\nCommitTransactionRequest {Transaction={ReadConflictRanges=[{Begin='\\xff/SC/<R1:16>' End='\\xff/SC/<R1:16>\\x00'}] WriteConflictRanges=[{Begin='\\xff/SC/<R1:16>' End='\\xff/SC/<R1:16>\\x00'}]}}\n"
	if got := render(same, same); got != want {
		t.Fatalf("WIREGOLDEN_SC_SAME:\n%s\nwant\n%s", got, want)
	}
	if got := render(same, other); got == want || !strings.Contains(got, "<R2:16>") {
		t.Fatalf("WIREGOLDEN_SC_DIFFERENT: disjoint self-conflict keys render as overlapping:\n%s", got)
	}
}

// Values are literal, even when they look like a prefixed or self-conflicting
// key; a ClearRange's Param2 is its end key.
func TestTranscriptValuesStayLiteral(t *testing.T) {
	m := &types.CommitTransactionRequest{}
	m.Transaction.Mutations = []types.MutationRef{
		{Param1: []byte("p/k"), Param2: []byte("p/v")},
		{Param1: []byte("p/k2"), Param2: []byte(scKey(0x41))},
		{MutType: clearRange, Param1: []byte("p/a"), Param2: []byte("p/b")},
	}
	got := transcript(t, []Request{marker("v"), commitReq(m), marker("~end")}, []string{"v"})
	want := "== v\nCommitTransactionRequest {Transaction={Mutations=[{Param1='<p>k' Param2='p/v'} {Param1='<p>k2' Param2='\\xff/SC/AAAAAAAAAAAAAAAA'} {MutType=1 Param1='<p>a' Param2='<p>b'}]}}\n"
	if got != want {
		t.Fatalf("WIREGOLDEN_VALUES:\n%s\nwant\n%s", got, want)
	}
}

// An idempotency ID's presence, length and reuse are kept; only its bytes are
// normalized.
func TestTranscriptIdempotencyID(t *testing.T) {
	with := func(id []byte) Request {
		m := &types.CommitTransactionRequest{IdempotencyId: id}
		m.Transaction.ReadSnapshot = 5
		return commitReq(m)
	}
	got := transcript(t, []Request{marker("id"), with(nil), with([]byte("0123456789abcdef")), with([]byte("0123456789abcdef")), with([]byte("x")), marker("~end")}, []string{"id"})
	want := `== id
CommitTransactionRequest {Transaction={ReadSnapshot=V1}}
CommitTransactionRequest {Transaction={ReadSnapshot=V1} IdempotencyId=<R1:16>}
CommitTransactionRequest {Transaction={ReadSnapshot=V1} IdempotencyId=<R1:16>}
CommitTransactionRequest {Transaction={ReadSnapshot=V1} IdempotencyId=<R2:1>}
`
	if got != want {
		t.Fatalf("WIREGOLDEN_IDEMPOTENCY:\n%s\nwant\n%s", got, want)
	}
}

// Version sentinels keep their values (FDBTypes.h:575-576); a GRV's MaxVersion
// is a version.
func TestTranscriptVersions(t *testing.T) {
	grv := Request{Kind: "GetReadVersionRequest", Msg: &types.GetReadVersionRequest{MaxVersion: 777}}
	got := transcript(t, []Request{marker("v"), get("k", 1<<63-1), get("k", -1), get("k", -2), get("k", 777), grv, marker("~end")}, []string{"v"})
	want := "== v\nGetValueRequest {Key='k' Version=9223372036854775807}\nGetValueRequest {Key='k' Version=-1}\nGetValueRequest {Key='k' Version=-2}\nGetValueRequest {Key='k' Version=V1}\nGetReadVersionRequest {MaxVersion=V1}\n"
	if got != want {
		t.Fatalf("WIREGOLDEN_VERSIONS:\n%s\nwant\n%s", got, want)
	}
}

// The transcript fails unless the run is exactly the expected scenarios,
// each with a request, closed by ~end, with every request decoded.
func TestTranscriptBoundaries(t *testing.T) {
	ok := get("p/a", 1)
	for _, tc := range []struct {
		name string
		reqs []Request
		want []string
	}{
		{"no end", []Request{marker("one"), ok}, []string{"one"}},
		{"end only", []Request{marker("~end")}, []string{"one"}},
		{"missing scenario", []Request{marker("one"), ok, marker("~end")}, []string{"one", "two"}},
		{"wrong order", []Request{marker("two"), ok, marker("one"), ok, marker("~end")}, []string{"one", "two"}},
		{"empty scenario", []Request{marker("one"), marker("~end")}, []string{"one"}},
		{"undecoded request", []Request{marker("one"), ok, {Kind: "FileID(2828131)"}, marker("~end")}, []string{"one"}},
		{"marker after end", []Request{marker("one"), ok, marker("~end"), marker("two")}, []string{"one"}},
	} {
		if _, err := Transcript(tc.reqs, "p/", tc.want); err == nil {
			t.Errorf("WIREGOLDEN_BOUNDARY %s: accepted", tc.name)
		}
	}
	// An undecoded request in setup is allowed.
	if _, err := Transcript([]Request{{Kind: "FileID(2828131)"}, marker("one"), ok, marker("~end")}, "p/", []string{"one"}); err != nil {
		t.Fatalf("setup traffic: %v", err)
	}
}

func TestFileIDSkipsProtocolVersion(t *testing.T) {
	plain := []byte{0, 0, 0, 0, 0x2a, 0, 0, 0}
	versioned := append([]byte{1, 2, 3, 4, 5, 6, 0xDB, 0x0F}, plain...)
	for _, b := range [][]byte{plain, versioned} {
		if id, ok := fileID(b); !ok || id != 0x2a {
			t.Fatalf("fileID(%x) = %d, %v", b, id, ok)
		}
	}
	if _, ok := fileID([]byte{1, 2}); ok {
		t.Fatal("a short body has no file identifier")
	}
}

// pcap builds a classic nanosecond Ethernet/IPv4/TCP capture of one client
// connection to port 4721.
type pcap struct {
	bytes.Buffer
	seq uint32
}

const serverPort = 4721

func newPcap() *pcap {
	c := &pcap{seq: 101}
	var h [24]byte
	binary.LittleEndian.PutUint32(h[0:], 0xa1b23c4d)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 262144)
	binary.LittleEndian.PutUint32(h[20:], 1)
	c.Write(h[:])
	c.segment(1, 100, true, nil)
	return c
}

func (c *pcap) segment(ts int64, seq uint32, syn bool, payload []byte) {
	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:], 50000)
	binary.BigEndian.PutUint16(tcp[2:], serverPort)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	tcp[12] = 5 << 4
	tcp[13] = 0x10
	if syn {
		tcp[13] |= 0x02
	}
	copy(tcp[20:], payload)
	ip := make([]byte, 20+len(tcp))
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	ip[9] = 6
	copy(ip[12:], []byte{127, 0, 0, 1})
	copy(ip[16:], []byte{127, 0, 0, 1})
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

// send appends data to the stream at the next sequence number.
func (c *pcap) send(ts int64, data []byte) {
	c.segment(ts, c.seq, false, data)
	c.seq += uint32(len(data))
}

// connectPacket is a ConnectPacket's length-prefixed shape (44 bytes).
func connectPacket() []byte {
	b := make([]byte, 44)
	binary.LittleEndian.PutUint32(b, 40)
	return b
}

// frame is a plaintext FDB packet: length, XXH3-64 checksum, token, body.
func frame(body []byte) []byte {
	b := make([]byte, 12, 28+len(body))
	b = append(append(b, bytes.Repeat([]byte{7}, 16)...), body...)
	binary.LittleEndian.PutUint32(b, uint32(16+len(body)))
	binary.LittleEndian.PutUint64(b[4:], xxh3.Hash(b[12:]))
	return b
}

func getBody(key string) []byte {
	m := types.GetValueRequest{Key: []byte(key), Version: 42}
	return m.MarshalFDB()
}

func decode(t *testing.T, c *pcap, port uint16) *Capture {
	t.Helper()
	got, err := Decode(&c.Buffer, []uint16{port})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return got
}

// A complete capture decodes to the scenario's transcript.
func TestDecodeCapture(t *testing.T) {
	c := newPcap()
	c.send(10, append(connectPacket(), frame(getBody(MarkerPrefix+"one"))...))
	c.send(20, frame(getBody("p/a")))
	c.send(30, frame(getBody(MarkerPrefix+"~end")))
	got := decode(t, c, serverPort)
	if !got.Complete || len(got.Requests) != 3 {
		t.Fatalf("WIREGOLDEN_DECODE: complete=%v, %d requests", got.Complete, len(got.Requests))
	}
	if tr := transcript(t, got.Requests, []string{"one"}); tr != "== one\nGetValueRequest {Key='<p>a' Version=V1 SsLatestCommitVersions=''}\n" {
		t.Fatalf("transcript %q", tr)
	}
}

// Captures that miss requests, or hold a body without a file identifier, are
// not complete.
func TestDecodeIncomplete(t *testing.T) {
	marker := frame(getBody(MarkerPrefix + "one"))
	for _, tc := range []struct {
		name string
		port uint16
		make func(*pcap)
	}{
		{"wrong port", serverPort + 1, func(c *pcap) { c.send(10, append(connectPacket(), marker...)) }},
		{"empty", serverPort, func(c *pcap) {}},
		{"gap", serverPort, func(c *pcap) {
			c.send(10, append(connectPacket(), marker...))
			c.seq += 5 // 5 bytes never captured
			c.send(20, marker)
		}},
		{"truncated frame", serverPort, func(c *pcap) { c.send(10, append(connectPacket(), marker[:len(marker)-3]...)) }},
		{"no file identifier", serverPort, func(c *pcap) { c.send(10, append(connectPacket(), frame([]byte{1, 2, 3})...)) }},
	} {
		c := newPcap()
		tc.make(c)
		if got := decode(t, c, tc.port); got.Complete {
			t.Errorf("WIREGOLDEN_INCOMPLETE %s: reported complete (%d requests)", tc.name, len(got.Requests))
		}
	}
}
