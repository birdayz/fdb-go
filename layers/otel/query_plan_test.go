package otel

import (
	"bytes"
	"testing"
)

// TestContinuationCheckMAC drives the storage-free MAC check without any run
// bindings: a valid token passes, an absent or wrong key and a flipped MAC fail.
func TestContinuationCheckMAC(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{7}, continuationKeyBytes)
	token := signContinuationKind(secret, messageContinuationKind, "qa_run_0001", 3, "time", []byte("digest"), []byte("last"))
	parse := func(tok []byte) *continuation {
		t.Helper()
		c, err := parseContinuationKind(tok, messageContinuationKind)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := parse(token).checkMAC(secret); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	requireTyped[*CorruptionError](t, "absent secret", parse(token).checkMAC(nil))
	requireTyped[*CorruptionError](t, "wrong secret", parse(token).checkMAC(bytes.Repeat([]byte{8}, continuationKeyBytes)))
	flipped := append([]byte(nil), token...)
	flipped[len(flipped)-1] ^= 0x01
	requireTyped[*CorruptionError](t, "MAC flip", parse(flipped).checkMAC(secret))
}
