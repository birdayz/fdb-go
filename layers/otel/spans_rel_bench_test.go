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

// BenchmarkRelSpansVsFDB measures span export client CPU and stored bytes per
// span through ExportResourceSpans, GenAI messages off, for the Exporter
// (zlib-1), three runs on fresh runs (RFC §6 G2A). Its arm of the custom
// store's Exporter left with it at the cutover (phase 3A); the gate's
// receipts of both are in docs/benchmarks/otel-relational-local-20261107.md.
// The corpus is otelbench's fixture V2 (seed 283, 8 attributes, match every
// 100) as single-span envelopes, exported in calls of 500 spans; CPU is the
// process's user+sys over the export calls. Run it explicitly:
//
//	bazelisk test //layers/otel:otel_test --test_arg=-test.run=^$ \
//	  --test_arg=-test.bench=BenchmarkRelSpansVsFDB --test_arg=-test.benchtime=1x \
//	  --test_output=streamed --nocache_test_results
func BenchmarkRelSpansVsFDB(b *testing.B) {
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

	type arm struct {
		name   string
		export func(context.Context, []*tracepb.ResourceSpans) (ExportStats, error)
		size   func() (Size, error)
		close  func()
	}
	relArm := func(run string) arm {
		cfg := lrConfig()
		cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, run, 11
		e, err := newExporter(ctx, cfg, relParts{})
		if err != nil {
			b.Fatal(err)
		}
		return arm{
			name:   "rel",
			export: e.ExportResourceSpans,
			size:   func() (Size, error) { return lrSize(ctx, e.store.db, e.store.ts.Subspace) },
			close:  func() { _ = e.Shutdown(ctx) },
		}
	}
	for range b.N {
		for runIdx := range 3 {
			for _, mk := range []func(string) arm{relArm} {
				a := mk(lrBenchRun(b))
				input := make([]*tracepb.ResourceSpans, spans)
				for i, rs := range corpus {
					input[i] = proto.Clone(rs).(*tracepb.ResourceSpans)
				}
				// The previous arm's garbage is not this arm's CPU.
				runtime.GC()
				var before, after syscall.Rusage
				if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
					b.Fatal(err)
				}
				start := time.Now()
				var stats ExportStats
				for lo := 0; lo < spans; lo += call {
					st, err := a.export(ctx, input[lo:min(lo+call, spans)])
					if err != nil {
						b.Fatalf("%s export: %v", a.name, err)
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
					b.Fatalf("%s committed %d of %d spans", a.name, stats.Spans, spans)
				}
				sz, err := a.size()
				if err != nil {
					b.Fatal(err)
				}
				a.close()
				n := float64(stats.Spans)
				b.Logf("arm=%s run=%d spans=%d batches=%d cpu_us_per_span=%.1f wall_s=%.2f kv_bytes_per_span=%.1f prefix_bytes_per_span=%.1f keys_per_span=%.2f",
					a.name, runIdx, stats.Spans, stats.Batches, float64(cpu.Microseconds())/n, wall.Seconds(),
					float64(sz.KeyBytes+sz.ValueBytes)/n, float64(sz.PrefixBytes)/n, float64(sz.Keys)/n)
				b.ReportMetric(float64(cpu.Microseconds())/n, a.name+"-cpu-us/span")
				b.ReportMetric(float64(sz.KeyBytes+sz.ValueBytes)/n, a.name+"-B/span")
			}
		}
	}
}
