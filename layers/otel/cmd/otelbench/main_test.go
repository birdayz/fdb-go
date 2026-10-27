package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel"
)

type fakeBackend struct {
	t       *testing.T
	cfg     otel.Config
	ctx     context.Context
	calls   []string
	fail    string
	failure error
}

func (b *fakeBackend) call(ctx context.Context, name string) error {
	b.t.Helper()
	if ctx != b.ctx {
		b.t.Errorf("%s received a different context from the opener", name)
	}
	b.calls = append(b.calls, name)
	if b.fail == name {
		return b.failure
	}
	return nil
}

func (b *fakeBackend) Ingest(ctx context.Context) (otel.Statistics, error) {
	return otel.Statistics{Rows: 12, Attributes: 96}, b.call(ctx, "ingest")
}

func (b *fakeBackend) Audit(ctx context.Context) (otel.Statistics, error) {
	return otel.Statistics{Rows: 12, Attributes: 96}, b.call(ctx, "audit")
}

func (b *fakeBackend) Query(ctx context.Context, a otel.Attribute) (otel.QueryResult, error) {
	err := b.call(ctx, "query")
	return otel.QueryResult{Hits: otel.Expected(b.cfg, a.Scope, a.Key, a.Type, a.Value)}, err
}

func (b *fakeBackend) Trace(ctx context.Context, trace string) ([]otel.SpanHit, error) {
	return otel.ExpectedTrace(b.cfg, trace), b.call(ctx, "trace")
}
func (b *fakeBackend) Drop(ctx context.Context) error { return b.call(ctx, "drop") }
func (b *fakeBackend) Close()                         { b.calls = append(b.calls, "close") }
func (b *fakeBackend) open(ctx context.Context, c otel.Config, _, _ string) (otel.Backend, error) {
	b.ctx, b.cfg = ctx, c
	return b, nil
}

func cliArgs(mode string) []string {
	return []string{"-backend=fdb", "-mode=" + mode, "-run=cli_test_run", "-count=12", "-tenants=2", "-k=3", "-iterations=5", "-revision=test-revision"}
}

func decodeEvidence(t *testing.T, data []byte) otel.Evidence {
	t.Helper()
	var e otel.Evidence
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("decode evidence %q: %v", data, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("expected exactly one JSON receipt, got %v", err)
	}
	return e
}

func TestExecuteModes(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"fdb", "ch-map", "ch-attrs"} {
		for _, mode := range []string{"ingest", "query", "audit", "drop"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				t.Parallel()
				b := &fakeBackend{t: t}
				args := append(cliArgs(mode), "-backend="+backend)
				if mode == "drop" {
					args = append(args, "-confirm-drop")
				}
				var stdout, stderr bytes.Buffer
				if code := execute(args, &stdout, &stderr, b.open); code != 0 {
					t.Fatalf("exit=%d stderr=%s stdout=%s", code, &stderr, &stdout)
				}
				if stderr.Len() != 0 {
					t.Fatalf("unexpected stderr: %s", &stderr)
				}
				e := decodeEvidence(t, stdout.Bytes())
				if e.Tool != otel.ToolVersion || e.Backend != backend || e.Mode != mode || e.Revision != "test-revision" || e.Config != b.cfg || e.Config.Run != "cli_test_run" || e.Config.SaveMode != "pipelined" || e.Corpus == "" || e.Boundary == "" || e.Start == "" || e.Go == "" || e.Error != "" || e.Failures != 0 {
					t.Fatalf("wrong receipt: %+v", e)
				}
				var want []string
				switch mode {
				case "ingest":
					want = []string{"ingest", "close"}
				case "query":
					want = []string{"query", "query", "query", "query", "query", "close"}
					if len(e.Samples) != 5 {
						t.Fatalf("samples=%+v", e.Samples)
					}
				case "audit":
					want = []string{"audit", "query", "query", "query", "query", "query", "trace", "close"}
				case "drop":
					want = []string{"drop", "close"}
				}
				if !reflect.DeepEqual(b.calls, want) {
					t.Fatalf("calls=%v want=%v", b.calls, want)
				}
				if mode == "ingest" || mode == "audit" {
					if e.Stats.Rows != 12 || e.Stats.Attributes != 96 {
						t.Fatalf("stats=%+v", e.Stats)
					}
				}
				if !errors.Is(b.ctx.Err(), context.Canceled) {
					t.Fatalf("command context not released: %v", b.ctx.Err())
				}
			})
		}
	}
}

func TestExecuteQuerySuites(t *testing.T) {
	t.Parallel()
	for _, suite := range []string{"selective", "mixed", "rare", "trace"} {
		t.Run(suite, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{t: t}
			var stdout, stderr bytes.Buffer
			if code := execute(append(cliArgs("query"), "-suite="+suite), &stdout, &stderr, b.open); code != 0 {
				t.Fatalf("exit=%d stderr=%s stdout=%s", code, &stderr, &stdout)
			}
			e := decodeEvidence(t, stdout.Bytes())
			if e.Suite != suite || len(e.Samples) != 5 || len(b.calls) != 6 || b.calls[5] != "close" {
				t.Fatalf("receipt=%+v calls=%v", e, b.calls)
			}
			method := "query"
			if suite == "trace" {
				method = "trace"
			}
			for i, sample := range e.Samples {
				if b.calls[i] != method || sample.Iteration != i || sample.Error != "" || sample.Digest == "" {
					t.Fatalf("sample=%+v calls=%v", sample, b.calls)
				}
			}
		})
	}
}

func TestExecuteRejectsArgumentsBeforeOpeningOrCreatingOutput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-backend="},
		{"-backend=other"},
		{"-mode="},
		{"-mode=other"},
		{"-run="},
		{"-run=BAD_NAMESPACE"},
		{"-mode=drop"},
		{"-mode=drop", "-confirm-drop=false"},
		{"-timeout=0"},
		{"-timeout=-1s"},
		{"-timeout=bad"},
		{"-suite=other"},
		{"-count=0"},
		{"-count=bad"},
		{"-save-mode=other"},
		{"-save-mode="},
		{"-unknown"},
		{"-confirm-drop=bad"},
		{"positional"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "receipt.json")
			base := append(cliArgs("ingest"), "-output="+path)
			var stdout, stderr bytes.Buffer
			code := execute(append(base, args...), &stdout, &stderr, func(context.Context, otel.Config, string, string) (otel.Backend, error) {
				t.Fatal("invalid arguments reached backend opener")
				return nil, nil
			})
			if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments created output: %v", err)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := execute(nil, &stdout, &stderr, nil); code != 2 {
		t.Fatalf("missing required flags: exit=%d", code)
	}
}

func TestExecuteHelp(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"-help"}, &stdout, &stderr, nil); code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "confirm-drop") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

func TestExecuteFlagForwarding(t *testing.T) {
	t.Parallel()
	for _, saveMode := range []string{"serial", "pipelined"} {
		t.Run(saveMode, func(t *testing.T) {
			t.Parallel()
			args := []string{"-backend=ch-attrs", "-mode=drop", "-confirm-drop", "-run=flag_test_run", "-seed=42", "-count=24", "-tenants=3", "-attrs=12", "-match-every=2", "-batch=4", "-ch-batch=5", "-ch-batch-bytes=4096", "-tenant=1", "-k=2", "-iterations=3", "-max-candidates=99", "-max-bytes=12345", "-hash-bytes=1", "-order=time", "-cluster-file=owned.cluster", "-ch-url=http://owned.invalid:8123", "-timeout=37s", "-revision=flag-revision", "-suite=mixed", "-save-mode=" + saveMode}
			want := otel.Config{Run: "flag_test_run", Backend: "ch-attrs", Seed: 42, Count: 24, Tenants: 3, Attributes: 12, MatchEvery: 2, Batch: 4, CHBatch: 5, CHBatchBytes: 4096, Tenant: 1, K: 2, Iterations: 3, MaxCandidates: 99, MaxBytes: 12345, HashBytes: 1, Order: "time", SaveMode: saveMode}
			b := &fakeBackend{t: t}
			var stdout, stderr bytes.Buffer
			code := execute(args, &stdout, &stderr, func(ctx context.Context, c otel.Config, cluster, chURL string) (otel.Backend, error) {
				if c != want || cluster != "owned.cluster" || chURL != "http://owned.invalid:8123" {
					t.Fatalf("opener got config=%+v cluster=%q url=%q", c, cluster, chURL)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 37*time.Second {
					t.Fatalf("missing/wrong deadline: %v, %v", deadline, ok)
				}
				return b.open(ctx, c, cluster, chURL)
			})
			if code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, &stderr)
			}
			e := decodeEvidence(t, stdout.Bytes())
			if e.Config != want || e.Revision != "flag-revision" || e.Suite != "mixed" {
				t.Fatalf("receipt=%+v", e)
			}
		})
	}
}

func TestExecuteFailureReceipts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode, suite, fail string
	}{
		{"ingest", "selective", "open"},
		{"query", "selective", "open"},
		{"audit", "selective", "open"},
		{"drop", "selective", "open"},
		{"ingest", "selective", "ingest"},
		{"query", "selective", "query"},
		{"query", "trace", "trace"},
		{"audit", "selective", "audit"},
		{"audit", "selective", "query"},
		{"audit", "selective", "trace"},
		{"drop", "selective", "drop"},
	} {
		t.Run(tc.mode+"/"+tc.suite+"/"+tc.fail, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{t: t, fail: tc.fail, failure: errors.New("injected backend failure")}
			args := append(cliArgs(tc.mode), "-suite="+tc.suite, "-confirm-drop")
			var stdout, stderr bytes.Buffer
			code := execute(args, &stdout, &stderr, func(ctx context.Context, c otel.Config, cluster, chURL string) (otel.Backend, error) {
				if tc.fail == "open" {
					return nil, b.failure
				}
				return b.open(ctx, c, cluster, chURL)
			})
			if code != 1 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q stdout=%q", code, &stderr, &stdout)
			}
			e := decodeEvidence(t, stdout.Bytes())
			if e.Error == "" || e.Mode != tc.mode || e.Config.Run != "cli_test_run" {
				t.Fatalf("missing failure receipt: %+v", e)
			}
			if tc.fail == "open" {
				if e.Error != b.failure.Error() || len(b.calls) != 0 || len(e.Samples) != 0 {
					t.Fatalf("open failure receipt=%+v calls=%v", e, b.calls)
				}
			} else {
				if b.calls[len(b.calls)-1] != "close" {
					t.Fatalf("backend not closed: %v", b.calls)
				}
				if tc.mode == "query" {
					if len(e.Samples) != 5 || e.Failures != 5 || len(e.Histograms) != 0 {
						t.Fatalf("query failure receipt=%+v", e)
					}
					for _, sample := range e.Samples {
						if sample.Error != b.failure.Error() {
							t.Fatalf("failure sample=%+v", sample)
						}
					}
				} else if e.Error != b.failure.Error() {
					t.Fatalf("wrong backend error: %+v", e)
				}
				if tc.fail == "audit" && !reflect.DeepEqual(b.calls, []string{"audit", "close"}) {
					t.Fatalf("failed source audit continued querying: %v", b.calls)
				}
			}
		})
	}
}

func TestExecuteOutputFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "receipt.json")
	b := &fakeBackend{t: t}
	var stdout, stderr bytes.Buffer
	args := append(cliArgs("ingest"), "-output="+path)
	if code := execute(args, &stdout, &stderr, b.open); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt permissions=%o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if e := decodeEvidence(t, data); e.Mode != "ingest" || e.Stats.Rows != 12 {
		t.Fatalf("receipt=%+v", e)
	}
	stdout.Reset()
	stderr.Reset()
	if code := execute(args, &stdout, &stderr, nil); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("existing output exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("existing output changed: %q, %v", after, err)
	}
}

func TestExecuteOutputCreationFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "receipt.json")
	var stdout, stderr bytes.Buffer
	if code := execute(append(cliArgs("drop"), "-confirm-drop", "-output="+path), &stdout, &stderr, nil); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestExecuteOutputWriteFailureClosesBackend(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{t: t}
	var stderr bytes.Buffer
	if code := execute(cliArgs("ingest"), failingWriter{errors.New("injected write failure")}, &stderr, b.open); code != 1 || !strings.Contains(stderr.String(), "injected write failure") {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if !reflect.DeepEqual(b.calls, []string{"ingest", "close"}) || !errors.Is(b.ctx.Err(), context.Canceled) {
		t.Fatalf("calls=%v context=%v", b.calls, b.ctx.Err())
	}
}

func TestExecuteTimeoutIncludesBackendOpen(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := execute(append(cliArgs("ingest"), "-timeout=5ms"), &stdout, &stderr, func(ctx context.Context, _ otel.Config, _, _ string) (otel.Backend, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("opener has no deadline")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			t.Fatal("whole-command timeout did not reach opener")
			return nil, nil
		}
	})
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if e := decodeEvidence(t, stdout.Bytes()); e.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("timeout receipt=%+v", e)
	}
}

func TestExecuteParentCancellationReachesBackendOpen(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := executeContext(parent, cliArgs("ingest"), &stdout, &stderr, func(ctx context.Context, _ otel.Config, _, _ string) (otel.Backend, error) {
		cancel()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			t.Fatal("parent cancellation did not reach opener")
			return nil, nil
		}
	})
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if e := decodeEvidence(t, stdout.Bytes()); e.Error != context.Canceled.Error() {
		t.Fatalf("cancellation receipt=%+v", e)
	}
}
