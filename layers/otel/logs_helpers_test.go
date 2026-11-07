package otel

import (
	"crypto/rand"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func lgBackend() *FDBBackend {
	root := subspace.Sub("lg_root")
	keys := runKeys{legacy: root, data: root, logs: root.Sub(logRecordSubspace), logTrace: root.Sub(logTraceSubspace), logEq: root.Sub(logEqSubspace)}
	return &FDBBackend{cfg: Config{Run: "lg_run_0001", Tenant: 3, HashBytes: 8, K: 100}, keys: keys}
}

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

// lgRows is the KV layout saveLogBatch writes for one prepared log.
func lgRows(b *FDBBackend, p preparedLog) (subspace.Subspace, []fdb.KeyValue) {
	space := logRecordSpace(b.keys, b.cfg.Tenant, p.eventID)
	rows := []fdb.KeyValue{{Key: space.Pack(tuple.Tuple{int64(0)}), Value: p.header}}
	for i, c := range p.chunks {
		rows = append(rows, fdb.KeyValue{Key: space.Pack(tuple.Tuple{int64(i + 1)}), Value: c})
	}
	return space, rows
}

func lgPrepared(t *testing.T, b *FDBBackend, l LogRecord) (preparedLog, LogRecord) {
	t.Helper()
	p, err := b.prepareLog(l, 777)
	if err != nil {
		t.Fatal(err)
	}
	return p, l
}
