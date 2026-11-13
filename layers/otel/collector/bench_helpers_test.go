package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func benchUnitConfig() CollectorBenchConfig {
	return CollectorBenchConfig{
		ClusterFile: "/nonexistent/fdb.cluster", Run: "bench_unit_run", Tenant: 3, Seed: 11,
		Conversations: 6, TurnsPerConversation: 5, Producers: 3,
		NumConsumers: 1, QueueSize: 1000, BatchMinSize: 4, BatchMaxSize: 16, ExporterBatchSize: 8, ExporterWorkers: 4,
		EnableMessageDeltas: true, EnableMessageFullText: true, QueryIterations: 12,
	}
}

func benchAttr(t *testing.T, sp *tracepb.Span, key string) *commonpb.AnyValue {
	t.Helper()
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("span %x lacks %s", sp.SpanId, key)
	return nil
}

func benchMessageText(t *testing.T, v *commonpb.AnyValue) (string, string) {
	t.Helper()
	var role, text string
	for _, kv := range v.GetKvlistValue().GetValues() {
		switch kv.Key {
		case "role":
			role = kv.Value.GetStringValue()
		case "parts":
			text = kv.Value.GetArrayValue().GetValues()[0].GetKvlistValue().GetValues()[1].Value.GetStringValue()
		}
	}
	return role, text
}

func benchStringAttr(t *testing.T, kvs []*commonpb.KeyValue, key string) string {
	t.Helper()
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	t.Fatalf("attributes lack %s", key)
	return ""
}

func benchClusterFile(t *testing.T) string {
	t.Helper()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { foundationdbtc.TeardownT(t, container) })
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
}

// checkBenchSeries checks the windows tile the ingest, run load then drain,
// carry a monotone watermark and sum to the corpus totals.
func checkBenchSeries(t *testing.T, r CollectorBenchReport) {
	t.Helper()
	if len(r.Series) == 0 {
		t.Fatal("no time series windows")
	}
	var sum CollectorBenchWindow
	end, wm, phase := int64(0), int64(-1), benchPhaseLoad
	for i, w := range r.Series {
		if w.StartNs != end || w.EndNs <= w.StartNs || w.Watermark < wm || (w.Phase != phase && (phase != benchPhaseLoad || w.Phase != benchPhaseDrain)) ||
			w.ExportLatency.Population != int(w.ExportCalls) || w.SysBytes == 0 || w.HeapInuse == 0 {
			t.Fatalf("window %d %+v after end %d watermark %d phase %s", i, w, end, wm, phase)
		}
		end, wm, phase = w.EndNs, w.Watermark, w.Phase
		sum.ExportedSpans += w.ExportedSpans
		sum.Messages += w.Messages
		sum.RawInputBytes += w.RawInputBytes
		sum.Logs += w.Logs
		sum.ExportCalls += w.ExportCalls
		sum.FailedCalls += w.FailedCalls
		sum.Retries += w.Retries
	}
	last := r.Series[len(r.Series)-1]
	if last.Phase != benchPhaseDrain || last.Watermark != int64(r.Corpus.Conversations)-1 || last.QueuedSpans != 0 || end > r.Ingest.IngestWallNs+int64(time.Second) {
		t.Fatalf("final window %+v, ingest wall %d", last, r.Ingest.IngestWallNs)
	}
	s := r.Ingest.Collector
	if sum.ExportedSpans != int64(r.Corpus.Spans) || sum.Messages != r.Corpus.TotalMessageElements || sum.Logs != int64(r.Corpus.Logs) ||
		sum.ExportCalls != s.Exports || sum.FailedCalls != r.Ingest.FailedExportCalls || sum.Retries != s.Retries || sum.RawInputBytes <= 0 {
		t.Fatalf("window sums %+v, corpus %+v, collector %+v", sum, r.Corpus, s)
	}
	if w := r.Rates.WholeRun; w.Windows != len(r.Series) || w.Spans != int64(r.Corpus.Spans) || w.WallNs != end || w.SpansPerSec <= 0 {
		t.Fatalf("whole-run rate %+v", w)
	}
	if c := r.Rates.WholeRun.CPU; c.TotalNs <= 0 || c.UserNs <= 0 || c.CoresUsed <= 0 || c.MsPerSpan <= 0 || c.GOMAXPROCS < 1 || c.NumCPU < 1 {
		t.Fatalf("whole-run CPU %+v", c)
	}
	if r.Memory.PeakSysBytes == 0 || r.Memory.PeakHeapInuseBytes == 0 || r.Memory.PeakHeapInuseBytes > r.Memory.PeakSysBytes {
		t.Fatalf("memory %+v", r.Memory)
	}
}
