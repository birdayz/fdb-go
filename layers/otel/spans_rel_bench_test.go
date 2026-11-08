package otel

import (
	"context"
	"runtime"
	"syscall"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// BenchmarkSpanExport measures the Exporter's span export client CPU and
// stored bytes per span through ExportResourceSpans (zlib-1, GenAI messages
// off), three runs on fresh runs (RFC §6 G2A; receipts in
// docs/benchmarks/otel-relational-local-20261107.md, "Gate G2A"). The corpus
// is otelbench's fixture V2 (seed 283, 8 attributes, match every 100) as
// single-span envelopes, exported in calls of 500 spans; CPU is the process's
// user+sys over the export calls. Run it explicitly:
//
//	bazelisk test //layers/otel:otel_test --test_arg=-test.run=^$ \
//	  --test_arg=-test.bench=BenchmarkSpanExport --test_arg=-test.benchtime=1x \
//	  --test_output=streamed --nocache_test_results
func BenchmarkSpanExport(b *testing.B) {
	const (
		spans = 20000
		call  = 500
	)
	cluster := lrBenchCluster(b)
	ctx := context.Background()
	c := DefaultConfig()
	c.Count, c.Tenants = spans, 1
	corpus := make([]*tracepb.ResourceSpans, spans)
	for i := range corpus {
		rs, _, err := FixtureOTLP(Generate(c, i))
		if err != nil {
			b.Fatal(err)
		}
		corpus[i] = rs
	}
	for range b.N {
		for runIdx := range 3 {
			cfg := lrConfig()
			cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, lrBenchRun(b), 11
			e, err := newExporter(ctx, cfg, relParts{})
			if err != nil {
				b.Fatal(err)
			}
			input := make([]*tracepb.ResourceSpans, spans)
			for i, rs := range corpus {
				input[i] = proto.Clone(rs).(*tracepb.ResourceSpans)
			}
			// The previous run's garbage is not this run's CPU.
			runtime.GC()
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				b.Fatal(err)
			}
			start := time.Now()
			var stats ExportStats
			for lo := 0; lo < spans; lo += call {
				st, err := e.ExportResourceSpans(ctx, input[lo:min(lo+call, spans)])
				if err != nil {
					b.Fatalf("export: %v", err)
				}
				stats.Spans += st.Spans
				stats.Batches += st.Batches
			}
			wall := time.Since(start)
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
				b.Fatal(err)
			}
			cpu := lrRusage(after) - lrRusage(before)
			if stats.Spans != spans {
				b.Fatalf("committed %d of %d spans", stats.Spans, spans)
			}
			sz, err := lrSize(ctx, e.store.db, e.store.ts.Subspace)
			if err != nil {
				b.Fatal(err)
			}
			_ = e.Shutdown(ctx)
			n := float64(stats.Spans)
			b.Logf("run=%d spans=%d batches=%d cpu_us_per_span=%.1f wall_s=%.2f kv_bytes_per_span=%.1f prefix_bytes_per_span=%.1f keys_per_span=%.2f",
				runIdx, stats.Spans, stats.Batches, float64(cpu.Microseconds())/n, wall.Seconds(),
				float64(sz.KeyBytes+sz.ValueBytes)/n, float64(sz.PrefixBytes)/n, float64(sz.Keys)/n)
			b.ReportMetric(float64(cpu.Microseconds())/n, "cpu-us/span")
			b.ReportMetric(float64(sz.KeyBytes+sz.ValueBytes)/n, "B/span")
		}
	}
}
