package otel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// BenchmarkLogExport measures the log store's ingest client CPU and stored
// bytes per log and its QueryLogsByTrace latency, two runs on fresh runs
// (RFC §6 G2B; receipts in docs/benchmarks/otel-relational-local-20261107.md,
// "Gate G2B"). Run it explicitly:
//
//	bazelisk test //layers/otel:otel_test --test_arg=-test.run=^$ \
//	  --test_arg=-test.bench=BenchmarkLogExport --test_arg=-test.benchtime=1x \
//	  --test_output=streamed --nocache_test_results
func BenchmarkLogExport(b *testing.B) {
	const (
		logs  = 20000
		call  = 500
		reads = 400
	)
	cluster := lrBenchCluster(b)
	ctx := context.Background()
	if err := fdb.APIVersion(730); err != nil {
		b.Fatal(err)
	}
	sqldriver.RegisterDomainIfNotExists(relDomain)
	raw, err := fdb.OpenDatabase(cluster)
	if err != nil {
		b.Fatal(err)
	}
	defer raw.Close()
	db := recordlayer.NewFDBDatabase(raw)
	corpus := lrBenchCorpus(logs)
	traces := map[string]bool{}
	for _, l := range corpus {
		if l.TraceID != "" {
			traces[l.TraceID] = true
		}
	}
	var traceIDs []string
	for tr := range traces {
		traceIDs = append(traceIDs, tr)
	}
	slices.Sort(traceIDs)
	traceIDs = traceIDs[:min(reads, len(traceIDs))]

	for range b.N {
		for runIdx := range 2 {
			cfg := lrConfig()
			cfg.Run, cfg.Tenant = lrBenchRun(b), 11
			path, schema := relTenantPaths(cfg.Run, cfg.Tenant)
			ts, err := otelsql.EnsureTenant(withExporterTxns(ctx, cfg.TransactionTimeout), db, path, schema, nil)
			if err != nil {
				b.Fatal(err)
			}
			if ts.Serializer, err = RecordSerializer(cfg.RecordCompression); err != nil {
				b.Fatal(err)
			}
			s, err := newRelLogStore(&tenantStore{db: db, ts: ts, databasePath: path, schema: schema}, cfg)
			if err != nil {
				b.Fatal(err)
			}
			slots := make(chan struct{}, cfg.MaxInFlightTransactions)
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				b.Fatal(err)
			}
			start := time.Now()
			var stats LogExportStats
			for lo := 0; lo < logs; lo += call {
				st, err := s.ExportLogs(withTxnLimiter(withExporterTxns(ctx, cfg.TransactionTimeout), slots), slices.Clone(corpus[lo:min(lo+call, logs)]))
				if err != nil {
					b.Fatalf("export: %v", err)
				}
				stats.Logs += st.Logs
				stats.Batches += st.Batches
				stats.StoredBytes += st.StoredBytes
				stats.LogicalBytes += st.LogicalBytes
			}
			wall := time.Since(start)
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
				b.Fatal(err)
			}
			cpu := lrRusage(after) - lrRusage(before)
			var lat []time.Duration
			got := 0
			for _, tr := range traceIDs {
				t0 := time.Now()
				l, err := s.QueryLogsByTrace(withExporterTxns(ctx, cfg.TransactionTimeout), tr, "")
				if err != nil {
					b.Fatalf("read: %v", err)
				}
				lat = append(lat, time.Since(t0))
				got += len(l)
			}
			slices.Sort(lat)
			sz, err := lrSize(ctx, db, ts.Subspace.Sub(recordlayer.RecordKey, s.rt.GetRecordTypeKey()))
			if err != nil {
				b.Fatal(err)
			}
			for _, ix := range s.index {
				isz, err := lrSize(ctx, db, ts.Subspace.Sub(recordlayer.IndexKey, ix.idx.SubspaceTupleKey()))
				if err != nil {
					b.Fatal(err)
				}
				sz = lrAddSize(sz, isz)
			}
			n := float64(stats.Logs)
			b.Logf("run=%d logs=%d batches=%d cpu_us_per_log=%.1f wall_s=%.2f kv_bytes_per_log=%.1f prefix_bytes_per_log=%.1f keys_per_log=%.2f stats_stored_per_log=%.1f logical_per_log=%.1f by_trace_reads=%d logs_read=%d p50_ms=%.3f p99_ms=%.3f",
				runIdx, stats.Logs, stats.Batches, float64(cpu.Microseconds())/n, wall.Seconds(),
				float64(sz.KeyBytes+sz.ValueBytes)/n, float64(sz.PrefixBytes)/n, float64(sz.Keys)/n,
				float64(stats.StoredBytes)/n, float64(stats.LogicalBytes)/n, len(lat), got,
				float64(lat[len(lat)/2].Microseconds())/1000, float64(lat[len(lat)*99/100].Microseconds())/1000)
			b.ReportMetric(float64(cpu.Microseconds())/n, "cpu-us/log")
			b.ReportMetric(float64(sz.KeyBytes+sz.ValueBytes)/n, "B/log")
		}
	}
}

func lrAddSize(a, b Size) Size {
	return Size{Keys: a.Keys + b.Keys, KeyBytes: a.KeyBytes + b.KeyBytes, ValueBytes: a.ValueBytes + b.ValueBytes, PrefixBytes: a.PrefixBytes + b.PrefixBytes}
}

func lrRusage(r syscall.Rusage) time.Duration {
	return time.Duration(r.Utime.Nano() + r.Stime.Nano())
}

func lrBenchRun(b *testing.B) string {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		b.Fatal(err)
	}
	return "bench_" + hex.EncodeToString(nonce[:])
}

// lrBenchCorpus is n GenAI evaluation and inference-detail logs, four per
// trace with a span each, every fifth uncorrelated.
func lrBenchCorpus(n int) []LogRecord {
	out := make([]LogRecord, n)
	models := []string{"gpt-4o", "claude-3", "gemini-1.5", "llama-3"}
	for i := range out {
		l := LogRecord{
			Timestamp:          int64(1_700_000_000_000_000_000 + i*1000),
			ObservedTimestamp:  int64(1_700_000_000_000_000_000 + i*1000 + 7),
			SeverityText:       "INFO",
			SeverityNumber:     9,
			EventName:          "gen_ai.evaluation.result",
			Body:               exStr(fmt.Sprintf("evaluation %d: the answer was relevant and grounded in the retrieved context", i)),
			ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr(fmt.Sprintf("agent-%d", i%4))), exKV("deployment.environment", exStr("prod"))},
			ScopeName:          "genai-eval",
			ScopeVersion:       "1.2.0",
			Attributes: []*commonpb.KeyValue{
				exKV("gen_ai.evaluation.name", exStr([]string{"relevance", "groundedness", "toxicity"}[i%3])),
				exKV("gen_ai.evaluation.score.value", exDouble(float64(i%100)/100)),
				exKV("gen_ai.evaluation.score.label", exStr([]string{"pass", "fail"}[i%2])),
				exKV("gen_ai.request.model", exStr(models[i%len(models)])),
				exKV("gen_ai.response.id", exStr(fmt.Sprintf("resp-%08d", i))),
				exKV("session.id", exStr(fmt.Sprintf("session-%05d", i/40))),
				exKV("user.tier", exInt(int64(i%3))),
			},
		}
		if i%2 == 1 {
			l.EventName = "gen_ai.client.inference.operation.details"
		}
		if i%5 != 4 {
			l.TraceID, l.SpanID = qaID(i/4, 16), qaID(i, 8)
		}
		out[i] = l
	}
	return out
}

// lrBenchCluster is startExporterCluster for a benchmark.
func lrBenchCluster(b *testing.B) string {
	b.Helper()
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			b.Skip("FDB not available (no Docker)")
		}
		b.Fatal(err)
	}
	b.Cleanup(func() { foundationdbtc.TeardownT(b, container) })
	wire, err := container.ClusterFile(setup)
	if err != nil {
		b.Fatal(err)
	}
	cluster := filepath.Join(b.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		b.Fatal(err)
	}
	return cluster
}
