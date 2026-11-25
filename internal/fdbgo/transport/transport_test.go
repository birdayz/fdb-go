package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()

	token := UID{First: 0x1234567890ABCDEF, Second: 0xFEDCBA0987654321}
	body := []byte("hello fdb")

	// Non-TLS (with checksum).
	t.Run("non-TLS", func(t *testing.T) {
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
			t.Errorf("body: got %q, want %q", gotBody, body)
		}
	})

	// TLS (no checksum).
	t.Run("TLS", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, token, body, true); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}

		gotToken, gotBody, err := ReadFrame(&buf, true)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if gotToken != token {
			t.Errorf("token: got %+v, want %+v", gotToken, token)
		}
		if !bytes.Equal(gotBody, body) {
			t.Errorf("body: got %q, want %q", gotBody, body)
		}
	})
}

func TestFrameChecksumVerification(t *testing.T) {
	t.Parallel()

	token := UID{First: 1, Second: 2}
	body := []byte("test data")

	var buf bytes.Buffer
	WriteFrame(&buf, token, body, false)

	// Corrupt a byte in the payload.
	data := buf.Bytes()
	data[len(data)-1] ^= 0xFF

	_, _, err := ReadFrame(bytes.NewReader(data), false)
	if err == nil {
		t.Error("expected checksum error, got nil")
	}
}

func TestFrameEmptyBody(t *testing.T) {
	t.Parallel()

	token := UID{First: 42, Second: 0}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, token, nil, false); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	gotToken, gotBody, err := ReadFrame(&buf, false)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if gotToken != token {
		t.Errorf("token: got %+v, want %+v", gotToken, token)
	}
	if len(gotBody) != 0 {
		t.Errorf("body: got %d bytes, want 0", len(gotBody))
	}
}

func TestConnectPacketRoundTrip(t *testing.T) {
	t.Parallel()

	pkt := ConnectPacket{
		ProtocolVersion:     ProtocolVersion73,
		CanonicalRemotePort: 4689,
		ConnectionID:        0xDEADBEEF,
		CanonicalRemoteIP4:  binary.LittleEndian.Uint32(net.IPv4(127, 0, 0, 1).To4()),
	}

	data := pkt.Marshal()
	if len(data) != ConnectPacketSize {
		t.Fatalf("marshal size: got %d, want %d", len(data), ConnectPacketSize)
	}

	var pkt2 ConnectPacket
	if err := pkt2.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Protocol version should have objectSerializerFlag set.
	if !pkt2.HasObjectSerializerFlag() {
		t.Error("objectSerializerFlag not set")
	}
	if !pkt2.IsCompatible(ProtocolVersion73) {
		t.Error("not compatible with our version")
	}
	if pkt2.CanonicalRemotePort != 4689 {
		t.Errorf("port: got %d, want 4689", pkt2.CanonicalRemotePort)
	}
	if pkt2.ConnectionID != 0xDEADBEEF {
		t.Errorf("connectionID: got %x, want DEADBEEF", pkt2.ConnectionID)
	}
	if pkt2.IsIPv6() {
		t.Error("should not be IPv6")
	}
}

func TestConnectPacketIPv6(t *testing.T) {
	t.Parallel()

	pkt := ConnectPacket{
		ProtocolVersion:     ProtocolVersion73,
		CanonicalRemotePort: 4689,
		Flags:               FlagIPv6,
	}
	copy(pkt.CanonicalRemoteIP6[:], net.IPv6loopback)

	data := pkt.Marshal()
	var pkt2 ConnectPacket
	pkt2.Unmarshal(data)

	if !pkt2.IsIPv6() {
		t.Error("should be IPv6")
	}
	if !bytes.Equal(pkt2.CanonicalRemoteIP6[:], net.IPv6loopback) {
		t.Errorf("IPv6 addr mismatch")
	}
}

func TestConnectPacketCompatibility(t *testing.T) {
	t.Parallel()

	pkt := ConnectPacket{ProtocolVersion: ProtocolVersion73 | ObjectSerializerFlag}

	// Same major.minor → compatible.
	if !pkt.IsCompatible(ProtocolVersion73) {
		t.Error("should be compatible with same version")
	}

	// Different major → incompatible.
	if pkt.IsCompatible(0x0FDB00B080000000) {
		t.Error("should NOT be compatible with 8.0")
	}
}

// TestBuildVoidReply verifies our dynamic construction matches the C++ ground truth.
func TestBuildVoidReply(t *testing.T) {
	t.Parallel()

	// C++ ObjectWriter ground truth for ErrorOr<EnsureTable<Void>> (FDB 7.3.77).
	expected, _ := hex.DecodeString(
		"200000004aad1e02" +
			"0000000000000400" +
			"0400060006000400" +
			"0800090008000400" +
			"0800000008000000" +
			"020000001e000000")

	got := buildVoidReply()
	if !bytes.Equal(got, expected) {
		t.Errorf("buildVoidReply mismatch:\n  got: %x\n  want: %x", got, expected)
	}
}

func TestFramePayloadTooLarge(t *testing.T) {
	t.Parallel()

	// Craft a frame header claiming a payload larger than maxPayloadSize.
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(maxPayloadSize+1))

	_, _, err := ReadFrame(&buf, true) // TLS = skip checksum, hit the size check directly
	if err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("payload too large")) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMultipleFrames(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	tokens := []UID{
		{First: 1, Second: 1},
		{First: 2, Second: 2},
		{First: 3, Second: 3},
	}
	bodies := [][]byte{
		[]byte("first"),
		[]byte("second"),
		[]byte("third"),
	}

	for i := range tokens {
		WriteFrame(&buf, tokens[i], bodies[i], false)
	}

	for i := range tokens {
		gotToken, gotBody, err := ReadFrame(&buf, false)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if gotToken != tokens[i] {
			t.Errorf("frame %d token: got %+v, want %+v", i, gotToken, tokens[i])
		}
		if !bytes.Equal(gotBody, bodies[i]) {
			t.Errorf("frame %d body: got %q, want %q", i, gotBody, bodies[i])
		}
	}
}
