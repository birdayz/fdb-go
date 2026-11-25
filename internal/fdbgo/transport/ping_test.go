package transport

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestPingRequest_RoundTrip verifies that BuildPingRequest produces a body
// that extractPingReplyToken can parse back to the original token.
func TestPingRequest_RoundTrip(t *testing.T) {
	t.Parallel()

	token := UID{First: 0xDEADBEEFCAFEBABE, Second: 0x1234567890ABCDEF}
	body := BuildPingRequest(token)

	if len(body) == 0 {
		t.Fatal("BuildPingRequest returned empty body")
	}

	extracted, ok := extractPingReplyToken(body)
	if !ok {
		t.Fatalf("extractPingReplyToken failed on BuildPingRequest output (%d bytes: %x)", len(body), body)
	}

	if extracted != token {
		t.Fatalf("token mismatch: got {%#x, %#x}, want {%#x, %#x}",
			extracted.First, extracted.Second, token.First, token.Second)
	}
}

// Captured from libfdb_c 7.3.77 (3ea44ce1d9003ad095e408039e1f755c319c4dfb)
// talking to fdbserver 7.3.77; the ReplyPromise is a nested table, not a byte string.
const cppPingRequestHex = "14000000c7d247000600080004000600140004000c00000004000000140000000400000016000000d89e50573e31458e1b000000480ea5c5"

func TestPingRequest_CPPEncoding(t *testing.T) {
	t.Parallel()
	want, err := hex.DecodeString(cppPingRequestHex)
	if err != nil {
		t.Fatal(err)
	}
	token := UID{First: 0x8e45313e57509ed8, Second: 0xc5a50e480000001b}
	if got := BuildPingRequest(token); !bytes.Equal(got, want) {
		t.Fatalf("PingRequest differs from libfdb_c 7.3.77:\n got %x\nwant %x", got, want)
	}
}

func TestExtractPingReplyToken_CPPEncoding(t *testing.T) {
	t.Parallel()
	body, err := hex.DecodeString(cppPingRequestHex)
	if err != nil {
		t.Fatal(err)
	}
	want := UID{First: 0x8e45313e57509ed8, Second: 0xc5a50e480000001b}
	if got, ok := extractPingReplyToken(body); !ok || got != want {
		t.Fatalf("libfdb_c 7.3.77 PingRequest: got %+v, %v; want %+v", got, ok, want)
	}
}

// TestPingRequest_MultipleTokens verifies round-trip for several different tokens.
func TestPingRequest_MultipleTokens(t *testing.T) {
	t.Parallel()

	tokens := []UID{
		{First: 0, Second: 0},
		{First: 1, Second: 1},
		{First: ^uint64(0), Second: ^uint64(0)},
		NewUID(), // random
		NewUID(),
	}

	for _, token := range tokens {
		body := BuildPingRequest(token)
		extracted, ok := extractPingReplyToken(body)
		if !ok {
			t.Errorf("extractPingReplyToken failed for token {%#x, %#x}", token.First, token.Second)
			continue
		}
		if extracted != token {
			t.Errorf("token {%#x, %#x}: got {%#x, %#x}",
				token.First, token.Second, extracted.First, extracted.Second)
		}
	}
}
