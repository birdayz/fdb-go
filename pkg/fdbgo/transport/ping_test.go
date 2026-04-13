package transport

import (
	"testing"
)

// TestPingRequest_RoundTrip verifies that buildPingRequest produces a body
// that extractPingReplyToken can parse back to the original token.
// This proves our outbound PINGs use valid FDB wire format.
func TestPingRequest_RoundTrip(t *testing.T) {
	t.Parallel()

	token := UID{First: 0xDEADBEEFCAFEBABE, Second: 0x1234567890ABCDEF}
	body := buildPingRequest(token)

	if len(body) == 0 {
		t.Fatal("buildPingRequest returned empty body")
	}

	extracted, ok := extractPingReplyToken(body)
	if !ok {
		t.Fatalf("extractPingReplyToken failed on buildPingRequest output (%d bytes: %x)", len(body), body)
	}

	if extracted != token {
		t.Fatalf("token mismatch: got {%#x, %#x}, want {%#x, %#x}",
			extracted.First, extracted.Second, token.First, token.Second)
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
		body := buildPingRequest(token)
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
