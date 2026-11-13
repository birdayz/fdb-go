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

func TestExecuteCollectorBenchQueryOnly(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	args := collectorBenchArgs("--query-only", "--query-classes=fts_single_token,trace_graph_traversal", "-iterations=500", "-output="+filepath.Join(t.TempDir(), "bench.json"))
	var stdout, stderr bytes.Buffer
	if code := execute(args, &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	want := collector.DefaultCollectorBenchConfig()
	want.ClusterFile, want.Run, want.Revision = "/nonexistent/fdb.cluster", "cli_bench_run", "test-revision"
	want.QueryOnly, want.QueryClasses, want.QueryIterations = true, "fts_single_token,trace_graph_traversal", 500
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("runner calls %+v, want %+v", *calls, want)
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
		// D2: the order and Brotli have no Exporter setting; the removed
		// custom store's layout flags and --backend fdb are refused.
		collectorBenchArgs("-order=trace"),
		collectorBenchArgs("-hash-bytes=4"),
		collectorBenchArgs("-posting-value-format=json"),
		collectorBenchArgs("-record-compression=brotli-4"),
		collectorBenchArgs("-backend=fdb"),
		// The --sql-* flags shape the sql backend only.
		collectorBenchArgs("-sql-query=direct"),
		collectorBenchArgs("-sql-pinned-conn"),
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
		collectorBenchArgs("--query-only", "--duration=10m"),
		collectorBenchArgs("--query-classes=fts_bogus"),
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
		t.Fatalf("collector-bench with a backend: exit=%d stderr=%q", code, &stderr)
	}
	stderr.Reset()
	if code := execute(collectorBenchArgs("-backend=fdb"), &stdout, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "explicit valid backend/mode required; see -help") {
		t.Fatalf("collector-bench with --backend fdb: exit=%d stderr=%q", code, &stderr)
	}
	stderr.Reset()
	if code := execute(collectorBenchArgs("-order=trace"), &stdout, &stderr, nil); code != 2 || !strings.Contains(stderr.String(), "--order applies to the ClickHouse backends only") {
		t.Fatalf("layout flag: exit=%d stderr=%q", code, &stderr)
	}
	// The shared flags collector-bench has no setting for are refused, and
	// so is an explicit zero its defaults would replace; its Validate bounds
	// the flags it reads.
	for _, tc := range []struct{ arg, want string }{
		{"-count=100", "--count does not apply to collector-bench"},
		{"-tenants=2", "--tenants does not apply to collector-bench"},
		{"-attrs=8", "--attrs does not apply to collector-bench"},
		{"-match-every=10", "--match-every does not apply to collector-bench"},
		{"-save-mode=batch", "--save-mode does not apply to collector-bench"},
		{"-bulk-index-conflicts=false", "--bulk-index-conflicts does not apply to collector-bench"},
		{"-k=10", "--k does not apply to collector-bench"},
		{"-max-candidates=100", "--max-candidates does not apply to collector-bench"},
		{"-max-bytes=1000", "--max-bytes does not apply to collector-bench"},
		{"-suite=trace", "--suite does not apply to collector-bench"},
		{"-ch-url=http://127.0.0.1:8123", "--ch-url does not apply to collector-bench"},
		{"-ch-batch=100", "--ch-batch does not apply to collector-bench"},
		{"-ch-batch-bytes=1000", "--ch-batch-bytes does not apply to collector-bench"},
		{"-ch-index-granularity=100", "--ch-index-granularity does not apply to collector-bench"},
		{"-ch-query-transport=native", "--ch-query-transport does not apply to collector-bench"},
		{"-ch-native-addr=127.0.0.1:9000", "--ch-native-addr does not apply to collector-bench"},
		{"-confirm-drop", "--confirm-drop does not apply to collector-bench"},
		{"-seed=0", "--seed=0 would run with collector-bench's default"},
		{"-record-compression=", "--record-compression= would run with collector-bench's default"},
		{"-iterations=0", "--iterations=0 would run with collector-bench's default"},
		{"-batch=0", "--batch=0 would run with collector-bench's default"},
		{"-workers=0", "--workers=0 would run with collector-bench's default"},
		{"-iterations=-1", "query_iterations must be 1..1000000"},
		{"-iterations=1000001", "query_iterations must be 1..1000000"},
		{"-batch=-1", "batch_size must be 1..64"},
		{"-batch=65", "batch_size must be 1..64"},
		{"-workers=-1", "workers must be 1..16"},
		{"-workers=17", "workers must be 1..16"},
		{"-tenant=-1", "tenant must be 0..49999999"},
		{"-tenant=50000000", "tenant must be 0..49999999"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := execute(collectorBenchArgs(tc.arg), &stdout, &stderr, nil); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: exit=%d stderr=%q, want %q", tc.arg, code, &stderr, tc.want)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("refused flags reached the runner: %+v", *calls)
	}
}

// collector-bench drives otel.Exporter, so it takes no backend.
func TestExecuteCollectorBenchBackendFlag(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	var stdout, stderr bytes.Buffer
	if code := execute(collectorBenchArgs(), &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if len(*calls) != 1 || (*calls)[0].RecordCompression != "zlib-1" {
		t.Fatalf("runner calls %+v", *calls)
	}
}
