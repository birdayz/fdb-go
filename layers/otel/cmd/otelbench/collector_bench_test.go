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

	"fdb.dev/layers/otel/collector"
)

// These tests swap the package-level runner, so they must not run in parallel.

func collectorBenchArgs(extra ...string) []string {
	return append([]string{"-backend=fdb", "-mode=collector-bench", "-run=cli_bench_run", "-cluster-file=/nonexistent/fdb.cluster", "-revision=test-revision"}, extra...)
}

func stubCollectorBench(t *testing.T, err error) *[]collector.CollectorBenchConfig {
	t.Helper()
	var calls []collector.CollectorBenchConfig
	prev := runCollectorBenchmark
	runCollectorBenchmark = func(ctx context.Context, cfg collector.CollectorBenchConfig) (collector.CollectorBenchReport, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("collector bench ran without the command deadline")
		}
		calls = append(calls, cfg)
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
}

func TestExecuteCollectorBenchFlags(t *testing.T) {
	calls := stubCollectorBench(t, nil)
	path := filepath.Join(t.TempDir(), "bench.json")
	args := collectorBenchArgs("--conversations=7", "--turns=3", "--producers=2", "--enable-message-deltas=false", "--enable-message-fts=false",
		"-seed=9", "-tenant=1", "-iterations=4", "-batch=16", "-workers=2", "-order=trace", "-hash-bytes=4", "-record-compression=zlib-1", "-posting-value-format=json", "-output="+path)
	var stdout, stderr bytes.Buffer
	if code := execute(args, &stdout, &stderr, nil); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	want := collector.DefaultCollectorBenchConfig()
	want.ClusterFile, want.Run, want.Revision = "/nonexistent/fdb.cluster", "cli_bench_run", "test-revision"
	want.Conversations, want.TurnsPerConversation, want.Producers = 7, 3, 2
	want.EnableMessageDeltas, want.EnableMessageFullText = false, false
	want.Seed, want.Tenant, want.QueryIterations, want.ExporterBatchSize, want.ExporterWorkers = 9, 1, 4, 16, 2
	want.Order, want.HashBytes, want.RecordCompression, want.PostingValueFormat = "trace", 4, "zlib-1", "json"
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
		{"-backend=fdb", "-mode=collector-bench", "-run=cli_bench_run"},
		{"-backend=fdb", "-mode=collector-bench", "-cluster-file=/nonexistent/fdb.cluster"},
		collectorBenchArgs("--conversations=-1"),
		collectorBenchArgs("--turns=-1"),
		collectorBenchArgs("--turns=201"),
		collectorBenchArgs("--producers=-2"),
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
}
