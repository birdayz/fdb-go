package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// ============================================================================
// extractPingReplyToken — defensive parser for inbound PING bodies.
// ============================================================================

func TestExtractPingReplyToken_EmptyBody(t *testing.T) {
	t.Parallel()
	if _, ok := extractPingReplyToken(nil); ok {
		t.Error("nil body must not parse to ok")
	}
	if _, ok := extractPingReplyToken([]byte{}); ok {
		t.Error("empty body must not parse to ok")
	}
}

func TestExtractPingReplyToken_ShortBodyBelowMinimum(t *testing.T) {
	t.Parallel()
	// Function checks len < 40 → returns false. Pin the boundary.
	for n := 1; n < 40; n++ {
		body := make([]byte, n)
		if _, ok := extractPingReplyToken(body); ok {
			t.Errorf("len=%d: short body must not parse to ok", n)
		}
	}
}

func TestExtractPingReplyToken_MalformedFlatBuffers(t *testing.T) {
	t.Parallel()
	// 40 bytes of zero is technically long enough but is not valid FlatBuffers.
	// wire.NewReader rejects this; extractPingReplyToken returns false.
	body := make([]byte, 40)
	if _, ok := extractPingReplyToken(body); ok {
		t.Error("all-zero 40-byte body must not parse to a valid token")
	}
}

func TestExtractPingReplyToken_RandomGarbage(t *testing.T) {
	t.Parallel()
	// Various 80-byte garbage payloads — function must not panic on any of them.
	patterns := [][]byte{
		bytes.Repeat([]byte{0xFF}, 80),
		bytes.Repeat([]byte{0xAA}, 80),
		[]byte("garbage garbage garbage garbage garbage garbage garbage garbage garbage garbage"),
	}
	for _, p := range patterns {
		// Defensive: just verify no panic. Result may or may not be ok.
		_, _ = extractPingReplyToken(p)
	}
}

func TestExtractPingReplyToken_RecoversAllZeroToken(t *testing.T) {
	t.Parallel()
	// {0,0} is a valid 128-bit UID and round-trips through Build/extract.
	zero := UID{}
	body := BuildPingRequest(zero)
	got, ok := extractPingReplyToken(body)
	if !ok || got != zero {
		t.Errorf("BuildPingRequest({0,0}) → extract failed: ok=%v got=%+v", ok, got)
	}
}

// ============================================================================
// splitmix64 / NewUID / newConnectionID — RNG sanity for reply routing.
// ============================================================================

func TestSplitmix64_AdjacentCallsDiffer(t *testing.T) {
	t.Parallel()
	a := splitmix64()
	b := splitmix64()
	if a == b {
		t.Errorf("two adjacent splitmix64 calls returned the same value: %#x — splitmix progression broken", a)
	}
}

func TestSplitmix64_NoAllZeroFixedPoint(t *testing.T) {
	t.Parallel()
	// SplitMix64 with our increment 0x9e3779b97f4a7c15 has full 2^64 period
	// from any seed and never produces 0 if the state never lands on a
	// specific bad seed. Run 1024 calls to verify none output 0 — tiny chance
	// false positive (~5e-17) but a real failure would mean state init broke.
	for i := 0; i < 1024; i++ {
		if v := splitmix64(); v == 0 {
			t.Errorf("splitmix64() returned 0 after %d calls — likely uninitialised state", i)
			return
		}
	}
}

func TestNewUID_DistinctConsecutiveCalls(t *testing.T) {
	t.Parallel()
	a := NewUID()
	b := NewUID()
	if a == b {
		t.Errorf("two consecutive NewUID() calls collided: %+v — random source broken", a)
	}
	// Both halves drawn from the same RNG; over 100 pairs we must get >0
	// distinct First halves AND >0 distinct Second halves (sanity, not
	// statistical strength).
	allFirstsEqual, allSecondsEqual := true, true
	prev := NewUID()
	for i := 0; i < 100; i++ {
		cur := NewUID()
		if cur.First != prev.First {
			allFirstsEqual = false
		}
		if cur.Second != prev.Second {
			allSecondsEqual = false
		}
		prev = cur
	}
	if allFirstsEqual {
		t.Error("NewUID().First constant across 100 calls — RNG dead")
	}
	if allSecondsEqual {
		t.Error("NewUID().Second constant across 100 calls — RNG dead")
	}
}

func TestNewConnectionID_NonZeroAndDistinct(t *testing.T) {
	t.Parallel()
	a := newConnectionID()
	b := newConnectionID()
	if a == 0 {
		t.Errorf("newConnectionID() returned 0 — crypto/rand probably failed")
	}
	if a == b {
		t.Errorf("two consecutive newConnectionID() calls collided: %#x — bad", a)
	}
}

// ============================================================================
// FrameReader.Read — uncovered failure modes.
// ============================================================================

func TestFrameReader_PayloadTooShort(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// Claim payloadLen = minPayloadSize - 1 (15). Below the minimum (16).
	binary.Write(&buf, binary.LittleEndian, uint32(minPayloadSize-1))
	_, _, err := ReadFrame(&buf, true) // tls=true → skip checksum
	if err == nil {
		t.Fatal("expected error for payloadLen < minPayloadSize, got nil")
	}
	if !strings.Contains(err.Error(), "payload too short") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFrameReader_TruncatedAtLengthHeader(t *testing.T) {
	t.Parallel()
	// Only 2 bytes — io.ReadFull on the 4-byte length prefix returns ErrUnexpectedEOF.
	_, _, err := ReadFrame(bytes.NewReader([]byte{0x10, 0x00}), true)
	if err == nil {
		t.Fatal("expected error on truncated length header, got nil")
	}
	if !strings.Contains(err.Error(), "read packet length") {
		t.Errorf("unexpected error: %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error must wrap io.ErrUnexpectedEOF for truncated header, got %v", err)
	}
}

func TestFrameReader_TruncatedAtChecksum(t *testing.T) {
	t.Parallel()
	// 4-byte length present, then truncate before the 8-byte checksum.
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(20)) // some valid payloadLen
	buf.Write([]byte{1, 2, 3})                          // < 8 bytes of checksum
	_, _, err := ReadFrame(&buf, false)                 // non-TLS path needs checksum
	if err == nil {
		t.Fatal("expected error on truncated checksum, got nil")
	}
	if !strings.Contains(err.Error(), "read checksum") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFrameReader_TruncatedAtPayload(t *testing.T) {
	t.Parallel()
	// Valid header + checksum, payload is 5 bytes when packetLen claims 20.
	var raw bytes.Buffer
	token := UID{First: 7, Second: 8}
	body := []byte("abcdefghijabcdefghij") // 20 bytes — payloadLen = 36 (16 token + 20)
	if err := WriteFrame(&raw, token, body, false); err != nil {
		t.Fatalf("setup WriteFrame: %v", err)
	}
	// Truncate the buffer mid-payload.
	complete := raw.Bytes()
	truncated := complete[:len(complete)-3]
	_, _, err := ReadFrame(bytes.NewReader(truncated), false)
	if err == nil {
		t.Fatal("expected error on truncated payload, got nil")
	}
	if !strings.Contains(err.Error(), "read payload") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFrameReader_PersistentHeaderAcrossMultipleFrames(t *testing.T) {
	t.Parallel()
	// FrameReader has a persistent hdr buffer to avoid per-frame heap alloc.
	// Verify several reads via the SAME FrameReader work correctly.
	var buf bytes.Buffer
	tokens := []UID{
		{First: 1, Second: 2},
		{First: 3, Second: 4},
		{First: 5, Second: 6},
	}
	bodies := [][]byte{
		[]byte("first"),
		[]byte("second"),
		[]byte(""), // empty body — exercises the minPayloadSize boundary
	}
	for i := range tokens {
		if err := WriteFrame(&buf, tokens[i], bodies[i], false); err != nil {
			t.Fatalf("setup WriteFrame[%d]: %v", i, err)
		}
	}

	var fr FrameReader
	for i := range tokens {
		gotToken, gotBody, err := fr.Read(&buf, false)
		if err != nil {
			t.Fatalf("Read[%d]: %v", i, err)
		}
		if gotToken != tokens[i] {
			t.Errorf("Read[%d] token: got %+v, want %+v", i, gotToken, tokens[i])
		}
		if !bytes.Equal(gotBody, bodies[i]) {
			t.Errorf("Read[%d] body: got %q, want %q", i, gotBody, bodies[i])
		}
	}
}

func TestFrameReader_ChecksumFlippedHeaderByteIsCaught(t *testing.T) {
	t.Parallel()
	// Write a valid frame, then flip a bit inside the 16-byte UID payload —
	// distinct from the existing TestFrameChecksumVerification (which flips
	// a body byte).
	var buf bytes.Buffer
	token := UID{First: 0xAAAA, Second: 0xBBBB}
	WriteFrame(&buf, token, []byte("hello"), false)
	data := buf.Bytes()

	// Layout: [4 len][8 checksum][16 token][body]. Flip token byte (offset 12).
	data[12] ^= 0x01
	_, _, err := ReadFrame(bytes.NewReader(data), false)
	if err == nil {
		t.Fatal("expected checksum error on flipped token byte, got nil")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteFrame_PropagatesWriterError(t *testing.T) {
	t.Parallel()
	w := &errWriter{err: errors.New("simulated write failure")}
	err := WriteFrame(w, UID{First: 1, Second: 2}, []byte("payload"), false)
	if err == nil {
		t.Fatal("expected writer error, got nil")
	}
	if !strings.Contains(err.Error(), "simulated write failure") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteFrame_LargeBodyRoundTrip(t *testing.T) {
	t.Parallel()
	// 1 MiB body — well below maxPayloadSize but exercises the pool grow-path
	// where the pooled 4 KiB buffer is too small.
	body := bytes.Repeat([]byte{0x42}, 1<<20)
	token := UID{First: 0xDEAD, Second: 0xBEEF}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, token, body, false); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	gotToken, gotBody, err := ReadFrame(&buf, false)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if gotToken != token {
		t.Errorf("token: got %+v, want %+v", gotToken, token)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body bytes diverge after large round-trip")
	}
}

// ============================================================================
// ConnectPacket — additional shape + error-path coverage.
// ============================================================================

func TestConnectPacket_UnmarshalShortBuffer(t *testing.T) {
	t.Parallel()
	for n := 0; n < ConnectPacketSize; n++ {
		var p ConnectPacket
		err := p.Unmarshal(make([]byte, n))
		if err == nil {
			t.Errorf("len=%d: expected error, got nil", n)
		}
	}
}

func TestConnectPacket_UnmarshalRejectsLengthAbove40(t *testing.T) {
	t.Parallel()
	buf := make([]byte, ConnectPacketSize)
	binary.LittleEndian.PutUint32(buf[0:], 41) // len > 40 — rejected
	var p ConnectPacket
	err := p.Unmarshal(buf)
	if err == nil {
		t.Fatal("expected error for length > 40, got nil")
	}
	if !strings.Contains(err.Error(), "length too large") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadConnectPacket_TruncatedReader(t *testing.T) {
	t.Parallel()
	// Reader supplies fewer than ConnectPacketSize bytes.
	short := bytes.NewReader(make([]byte, ConnectPacketSize-5))
	_, err := ReadConnectPacket(short)
	if err == nil {
		t.Fatal("expected error for truncated reader, got nil")
	}
	if !strings.Contains(err.Error(), "read connect packet") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteConnectPacket_PropagatesWriterError(t *testing.T) {
	t.Parallel()
	w := &errWriter{err: errors.New("connect write fail")}
	err := WriteConnectPacket(w, nil, 0xDEADBEEF)
	if err == nil {
		t.Fatal("expected writer error, got nil")
	}
}

func TestConnectPacket_RoundTripPreservesAllFields(t *testing.T) {
	t.Parallel()
	in := ConnectPacket{
		ProtocolVersion:     ProtocolVersion73,
		CanonicalRemotePort: 65535,
		ConnectionID:        0xCAFEBABEDEADBEEF,
		CanonicalRemoteIP4:  0xAABBCCDD,
		Flags:               FlagIPv6,
	}
	for i := range in.CanonicalRemoteIP6 {
		in.CanonicalRemoteIP6[i] = byte(i + 1)
	}
	data := in.Marshal()
	var out ConnectPacket
	if err := out.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// Marshal sets ObjectSerializerFlag — strip before comparison.
	outVer := out.ProtocolVersion & ^ObjectSerializerFlag
	if outVer != in.ProtocolVersion {
		t.Errorf("protocol version: got %#x, want %#x", outVer, in.ProtocolVersion)
	}
	if out.CanonicalRemotePort != in.CanonicalRemotePort {
		t.Errorf("port: got %d, want %d", out.CanonicalRemotePort, in.CanonicalRemotePort)
	}
	if out.ConnectionID != in.ConnectionID {
		t.Errorf("connID: got %#x, want %#x", out.ConnectionID, in.ConnectionID)
	}
	if out.CanonicalRemoteIP4 != in.CanonicalRemoteIP4 {
		t.Errorf("ip4: got %#x, want %#x", out.CanonicalRemoteIP4, in.CanonicalRemoteIP4)
	}
	if out.Flags != in.Flags {
		t.Errorf("flags: got %#x, want %#x", out.Flags, in.Flags)
	}
	if out.CanonicalRemoteIP6 != in.CanonicalRemoteIP6 {
		t.Errorf("ip6: got %v, want %v", out.CanonicalRemoteIP6, in.CanonicalRemoteIP6)
	}
}

// ============================================================================
// Test helpers.
// ============================================================================

// errWriter always fails Write with the configured error.
type errWriter struct{ err error }

func (e *errWriter) Write([]byte) (int, error) { return 0, e.err }
