package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel/collector"
)

// These tests swap the package-level runner, so they must not run in parallel.

func collectorBenchArgs(extra ...string) []string {
	return append([]string{"-mode=collector-bench", "-run=cli_bench_run", "-cluster-file=/nonexistent/fdb.cluster", "-revision=test-revision"}, extra...)
}

// benchDeadlines records the command deadline left when each stubbed run started.
var benchDeadlines []time.Duration

func stubCollectorBench(t *testing.T, err error) *[]collector.CollectorBenchConfig {
	t.Helper()
	var calls []collector.CollectorBenchConfig
	benchDeadlines = nil
	prev := runCollectorBenchmark
	runCollectorBenchmark = func(ctx context.Context, cfg collector.CollectorBenchConfig, ingestStart func()) (collector.CollectorBenchReport, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("collector bench ran without the command deadline")
		}
		benchDeadlines = append(benchDeadlines, time.Until(deadline))
		calls = append(calls, cfg)
		ingestStart()
		r := collector.CollectorBenchReport{Tool: collector.CollectorBenchTool, Mode: "collector-bench", Config: cfg.WithDefaults()}
		return r, err
	}
	t.Cleanup(func() { runCollectorBenchmark = prev })
	return &calls
}

func decodeCollectorReport(t *testing.T, data []byte) collector.CollectorBenchReport {
	t.Helper()
	var r collector.CollectorBenchReport
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode collector bench report: %v\n%s", err, data)
	}
	return r
}

func TestExecuteCollectorBenchDefaults(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	var stdout, stderr bytes.Buffer
	if code := execute(collectorBenchArgs(), &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	want := collector.DefaultCollectorBenchConfig()
	want.ClusterFile, want.Run, want.Revision = "/nonexistent/fdb.cluster", "cli_bench_run", "test-revision"
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("runner calls %+v, want %+v", *calls, want)
	}
	r := decodeCollectorReport(t, stdout.Bytes())
	if r.Tool != collector.CollectorBenchTool || r.Mode != "collector-bench" || r.Config != want || r.Error != "" {
		t.Fatalf("report %+v", r)
	}
	// A fixed-corpus run keeps the 10m default command deadline.
	if len(benchDeadlines) != 1 || benchDeadlines[0] > 10*time.Minute || benchDeadlines[0] < 9*time.Minute {
		t.Fatalf("deadline %v", benchDeadlines)
	}
}

func TestExecuteCollectorBenchFlags(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	path := filepath.Join(t.TempDir(), "bench.json")
	args := collectorBenchArgs("--conversations=7", "--turns=3", "--producers=2", "--enable-message-deltas=false", "--enable-message-fts=false",
		"--duration=10m", "--report-interval=5s", "--warmup=30s", "--query-workers=3", "--consumers=16",
		"-seed=9", "-tenant=1", "-iterations=4", "-batch=16", "-workers=2", "-record-compression=zlib-default", "-output="+path)
	var stdout, stderr bytes.Buffer
	if code := execute(args, &stdout, &stderr, nil); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	want := collector.DefaultCollectorBenchConfig()
	want.ClusterFile, want.Run, want.Revision = "/nonexistent/fdb.cluster", "cli_bench_run", "test-revision"
	want.Conversations, want.TurnsPerConversation, want.Producers = 7, 3, 2
	want.Duration, want.ReportInterval, want.Warmup, want.QueryWorkers, want.NumConsumers = 10*time.Minute, 5*time.Second, 30*time.Second, 3, 16
	want.EnableMessageDeltas, want.EnableMessageFullText = false, false
	want.Seed, want.Tenant, want.QueryIterations, want.ExporterBatchSize, want.ExporterWorkers = 9, 1, 4, 16, 2
	want.RecordCompression = "zlib-default"
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("runner calls %+v, want %+v", *calls, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeCollectorReport(t, data); r.Config != want {
		t.Fatalf("report config %+v, want %+v", r.Config, want)
	}
	// Without --timeout the deadline covers the duration plus the drain allowance.
	if len(benchDeadlines) != 1 || benchDeadlines[0] > 20*time.Minute || benchDeadlines[0] < 19*time.Minute {
		t.Fatalf("deadline %v", benchDeadlines)
	}
}

func TestExecuteCollectorBenchExplicitTimeout(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	var stdout, stderr bytes.Buffer
	if code := execute(collectorBenchArgs("--duration=2m", "--timeout=3m"), &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if len(*calls) != 1 || (*calls)[0].Duration != 2*time.Minute || len(benchDeadlines) != 1 || benchDeadlines[0] > 3*time.Minute || benchDeadlines[0] < 2*time.Minute {
		t.Fatalf("calls %+v deadlines %v", *calls, benchDeadlines)
	}
	stdout.Reset()
	if code := execute(collectorBenchArgs("--duration=5m", "--timeout=5m"), &stdout, &stderr, nil); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--timeout 5m0s must exceed --duration 5m0s") {
		t.Fatalf("timeout not above duration: exit=%d stderr=%q", code, &stderr)
	}
	if len(*calls) != 1 {
		t.Fatalf("rejected timeout reached the runner: %+v", *calls)
	}
}

func TestExecuteCollectorBenchFailureStillWritesReport(t *testing.T) {
	stubCollectorBench(t, errors.New("injected bench failure"))
	var stdout, stderr bytes.Buffer
	if code := execute(collectorBenchArgs(), &stdout, &stderr, nil); code != 1 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if r := decodeCollectorReport(t, stdout.Bytes()); r.Error != "injected bench failure" {
		t.Fatalf("report error %q", r.Error)
	}
}

func TestExecuteCollectorBenchRejections(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	for _, args := range [][]string{
		{"-backend=ch-map", "-mode=collector-bench", "-run=cli_bench_run", "-cluster-file=/nonexistent/fdb.cluster"},
		{"-mode=collector-bench", "-run=cli_bench_run"},
		{"-mode=collector-bench", "-cluster-file=/nonexistent/fdb.cluster"},
		// D2: the custom store's layout flags and Brotli have no Exporter setting.
		collectorBenchArgs("-order=trace"),
		collectorBenchArgs("-hash-bytes=4"),
		collectorBenchArgs("-posting-value-format=json"),
		collectorBenchArgs("-record-compression=brotli-4"),
		collectorBenchArgs("--conversations=-1"),
		collectorBenchArgs("--turns=-1"),
		collectorBenchArgs("--turns=201"),
		collectorBenchArgs("--producers=-2"),
		collectorBenchArgs("--duration=-1s"),
		collectorBenchArgs("--duration=2h"),
		collectorBenchArgs("--duration=30s"),
		collectorBenchArgs("--duration=bad"),
		collectorBenchArgs("--report-interval=1ms"),
		collectorBenchArgs("--warmup=-1s"),
		collectorBenchArgs("--duration=1m", "--warmup=1m"),
		collectorBenchArgs("--query-workers=-1"),
		collectorBenchArgs("--query-workers=65"),
		collectorBenchArgs("--consumers=-1"),
		collectorBenchArgs("--consumers=65"),
		collectorBenchArgs("--enable-message-fts=bad"),
		collectorBenchArgs("positional"),
	} {
		path := filepath.Join(t.TempDir(), "bench.json")
		var stdout, stderr bytes.Buffer
		if code := execute(append(args, "-output="+path), &stdout, &stderr, nil); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("%s: exit=%d stdout=%q stderr=%q", strings.Join(args, " "), code, &stdout, &stderr)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: invalid arguments created output: %v", strings.Join(args, " "), err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("invalid arguments reached the runner: %+v", *calls)
	}
	var stdout, stderr bytes.Buffer
	if code := execute(collectorBenchArgs("-backend=ch-attrs"), &stdout, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "explicit valid backend/mode required; see -help") {
		t.Fatalf("non-fdb collector-bench: exit=%d stderr=%q", code, &stderr)
	}
	stderr.Reset()
	if code := execute(collectorBenchArgs("-order=trace"), &stdout, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "--order applies to --backend fdb only") {
		t.Fatalf("layout flag: exit=%d stderr=%q", code, &stderr)
	}
}

// collector-bench drives otel.Exporter: no backend applies, and --backend
// fdb, the earlier receipts' spelling, is still accepted.
func TestExecuteCollectorBenchBackendFlag(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	var stdout, stderr bytes.Buffer
	if code := execute(collectorBenchArgs("-backend=fdb"), &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if len(*calls) != 1 || (*calls)[0].RecordCompression != "zlib-1" {
		t.Fatalf("runner calls %+v", *calls)
	}
}
