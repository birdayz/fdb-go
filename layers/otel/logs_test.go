package otel

import (
	"encoding/hex"
	"testing"
)

func TestNewEventID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 1000 {
		id, err := newEventID()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != id || raw[6]>>4 != 4 || raw[8]>>6 != 2 {
			t.Fatalf("not a lowercase hex v4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate event ID %s", id)
		}
		seen[id] = true
	}
}
