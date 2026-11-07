package otel

import (
	"crypto/rand"
	"testing"

	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func lgRandom(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// lgRequireLog compares every stored field through the canonical OTLP form.
func lgRequireLog(t *testing.T, label string, got, want LogRecord) {
	t.Helper()
	g, err := toResourceLogs(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := toResourceLogs(want)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(g, w) || got.EventID != want.EventID || got.EffectiveTime != want.EffectiveTime || got.TimeSource != want.TimeSource || got.TraceID != want.TraceID || got.SpanID != want.SpanID {
		t.Fatalf("%s\n got %+v\nwant %+v", label, got, want)
	}
}

func lgIDs(logs []LogRecord) []string {
	out := []string{}
	for _, l := range logs {
		out = append(out, l.EventID)
	}
	return out
}
