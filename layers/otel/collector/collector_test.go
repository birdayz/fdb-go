package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

var (
	errTransient = errors.New("transient: fdb 1020 not_committed")
	errOpen      = errors.New("open failed")
	errClose     = errors.New("close failed")
)

func mapLookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got: %v", want, err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func unknownField(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}

// testSpan returns a span with a unique, valid identity derived from n.
func testSpan(prefix string, n int) *tracepb.Span {
	return &tracepb.Span{
		TraceId:           []byte{0xa1, byte(n >> 16), byte(n >> 8), byte(n), 15: 1},
		SpanId:            []byte{0xb2, byte(n >> 16), byte(n >> 8), byte(n), 7: 1},
		Name:              fmt.Sprintf("%s%04d", prefix, n),
		StartTimeUnixNano: uint64(1_000_000 + n),
		EndTimeUnixNano:   uint64(1_000_500 + n),
	}
}

// testRequest is one ResourceSpans with one scope holding spans first..first+n-1.
func testRequest(prefix string, first, n int) []*tracepb.ResourceSpans {
	ss := &tracepb.ScopeSpans{Scope: &commonpb.InstrumentationScope{Name: "unit"}}
	for i := range n {
		ss.Spans = append(ss.Spans, testSpan(prefix, first+i))
	}
	return []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{ss}}}
}

func spanNames(in []*tracepb.ResourceSpans) []string {
	var out []string
	for _, rs := range in {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if sp != nil {
					out = append(out, sp.Name)
				}
			}
		}
	}
	return out
}

func rangeNames(prefix string, first, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("%s%04d", prefix, first+i)
	}
	return out
}

func okStats(n int) otel.ExportStats {
	return otel.ExportStats{ReceivedSpans: int64(n), Spans: int64(n), Batches: 1}
}

type sinkCall struct {
	at    time.Time
	names []string
}

// fakeSink records every export call; export, when set, decides the outcome.
type fakeSink struct {
	mu          sync.Mutex
	calls       []sinkCall
	export      func(ctx context.Context, call int, names []string) (otel.ExportStats, error)
	shutdowns   int
	shutdownErr error
}

func (s *fakeSink) ExportResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans) (otel.ExportStats, error) {
	names := spanNames(spans)
	s.mu.Lock()
	call := len(s.calls)
	s.calls = append(s.calls, sinkCall{at: time.Now(), names: names})
	fn := s.export
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, call, names)
	}
	return okStats(len(names)), nil
}

func (s *fakeSink) Shutdown(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdowns++
	return s.shutdownErr
}

func (s *fakeSink) snapshot() []sinkCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *fakeSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *fakeSink) shutdownCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdowns
}

func (s *fakeSink) exportedNames() []string {
	var out []string
	for _, c := range s.snapshot() {
		out = append(out, c.names...)
	}
	return out
}

// gated blocks every export until release closes (or the attempt ends).
func gated(release <-chan struct{}) func(context.Context, int, []string) (otel.ExportStats, error) {
	return func(ctx context.Context, _ int, names []string) (otel.ExportStats, error) {
		select {
		case <-release:
			return okStats(len(names)), nil
		case <-ctx.Done():
			return otel.ExportStats{}, ctx.Err()
		}
	}
}

func unitConfig(mut func(*Config)) *Config {
	cfg := NewFactory().CreateDefaultConfig()
	cfg.Exporter.ClusterFile = "/unit/fdb.cluster"
	cfg.Exporter.Run = "collector_unit"
	cfg.Timeout = 5 * time.Second
	cfg.SendingQueue = QueueConfig{Enabled: true, NumConsumers: 1, QueueSize: 1000, Batch: BatchConfig{FlushTimeout: time.Hour, MinSize: 1, MaxSize: 1000}}
	cfg.RetryOnFailure = RetryConfig{Enabled: true, InitialInterval: time.Millisecond, MaxInterval: 4 * time.Millisecond}
	if mut != nil {
		mut(cfg)
	}
	return cfg
}

func newUnitExporter(t *testing.T, cfg *Config, sink Sink) *TracesExporter {
	t.Helper()
	x, err := NewFactory().CreateTraces(Settings{ID: ComponentID{Type: ComponentType, Name: "unit"}, Open: func(context.Context, otel.ExporterConfig) (Sink, error) { return sink, nil }}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// startUnitExporter starts an exporter on sink and shuts it down at cleanup.
func startUnitExporter(t *testing.T, cfg *Config, sink Sink) *TracesExporter {
	t.Helper()
	x := newUnitExporter(t, cfg, sink)
	if err := x.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = x.Shutdown(ctx)
	})
	return x
}

func flush(t *testing.T, x *TracesExporter) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return x.Flush(ctx)
}

// ---------------------------------------------------------------------------
// Configuration: ParseComponentID, ParseConfig, LoadPipelineConfig,
// ParseExporterConfig, Config.Validate, environment expansion
// ---------------------------------------------------------------------------

func TestParseComponentID(t *testing.T) {
	t.Parallel()
	long := "a" + strings.Repeat("b", 62)
	for _, tc := range []struct {
		in   string
		want ComponentID
	}{
		{"otlp", ComponentID{Type: "otlp"}},
		{"fdb_otel", ComponentID{Type: "fdb_otel"}},
		{"fdb_otel/secondary", ComponentID{Type: "fdb_otel", Name: "secondary"}},
		{"otlp/agents-eu.1", ComponentID{Type: "otlp", Name: "agents-eu.1"}},
		{"forward/a/b", ComponentID{Type: "forward", Name: "a/b"}},
		{"X9_", ComponentID{Type: "X9_"}},
		{long, ComponentID{Type: long}},
		{"otlp/" + strings.Repeat("n", 1024), ComponentID{Type: "otlp", Name: strings.Repeat("n", 1024)}},
	} {
		got, err := ParseComponentID(tc.in)
		if err != nil {
			t.Fatalf("ParseComponentID(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseComponentID(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.in {
			t.Fatalf("String() = %q, want %q", got.String(), tc.in)
		}
	}
	for _, in := range []string{
		"", "/x", "1otlp", "_otlp", "fdb-otel", "fdb.otel", "otlp/", "otlp/a b", "otlp/a\tb", "otlp/a\x7f",
		long + "c", "otlp/" + strings.Repeat("n", 1025), "ünicode",
	} {
		if id, err := ParseComponentID(in); err == nil {
			t.Fatalf("ParseComponentID(%q) = %+v, want error", in, id)
		} else if !strings.Contains(err.Error(), "invalid component id") {
			t.Fatalf("ParseComponentID(%q): unexpected error %v", in, err)
		}
	}
}

func TestExpandString(t *testing.T) {
	t.Parallel()
	lookup := mapLookup(map[string]string{"A": "a", "B": "bee", "EMPTY": "", "PORT": "4317"})
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"${env:A}", "a"},
		{"${A}", "a"},
		{"x-${env:A}-${B}-y", "x-a-bee-y"},
		{"${env:MISSING}", ""},
		{"${MISSING}", ""},
		{"${env:MISSING:-fallback}", "fallback"},
		{"${MISSING:-fallback}", "fallback"},
		{"${env:A:-fallback}", "a"},
		{"${env:EMPTY:-fallback}", ""}, // set-but-empty wins over the default
		{"${env:MISSING:-}", ""},
		{"0.0.0.0:${env:PORT}", "0.0.0.0:4317"},
		{"${env:MISSING:-0.0.0.0:4317}", "0.0.0.0:4317"},
		{"cost$$5", "cost$5"},
		{"$${env:A}", "${env:A}"},
		{"price $5", "price $5"},
		{"trailing$", "trailing$"},
	} {
		got, err := expandString(tc.in, lookup)
		if err != nil {
			t.Fatalf("expandString(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("expandString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"${env:A", "unterminated reference"},
		{"x ${A", "unterminated reference"},
		{"${env:1A}", "invalid environment reference"},
		{"${}", "invalid environment reference"},
		{"${env:}", "invalid environment reference"},
		{"${env:A-B}", "invalid environment reference"},
		{"${env:A B}", "invalid environment reference"},
	} {
		_, err := expandString(tc.in, lookup)
		requireErrContains(t, err, tc.want)
	}
}

const fullCollectorYAML = `
extensions:
  health_check:
    endpoint: 0.0.0.0:13133
  pprof: {}
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: ${env:OTLP_GRPC_ENDPOINT:-0.0.0.0:4317}
      http:
        endpoint: 0.0.0.0:4318
  otlp/agents:
    protocols:
      grpc:
        endpoint: 0.0.0.0:14317
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
  batch:
    send_batch_size: 8192
    timeout: 200ms
  attributes/cost:
    actions:
      - key: billing.note
        value: cost$$5
        action: insert
connectors:
  forward/fdb: {}
exporters:
  debug:
    verbosity: detailed
  fdb_otel:
    cluster_file: ${env:FDB_CLUSTER_FILE}
    run: ${FDB_RUN}
    tenant: ${env:FDB_TENANT:-42}
    order: trace
    hash_bytes: 16
    posting_value_format: json
    record_compression: zlib-1
    bulk_index_conflicts: false
    batch_size: 32
    workers: 6
    max_candidates: 5000
    max_bytes: 1048576
    k: 25
    timeout: 12s
    sending_queue:
      enabled: true
      num_consumers: ${env:FDB_CONSUMERS}
      queue_size: 50000
      block_on_overflow: ${env:FDB_BLOCK:-true}
      batch:
        flush_timeout: ${env:FDB_FLUSH:-150ms}
        min_size: 256
        max_size: 2048
    retry_on_failure:
      enabled: true
      initial_interval: 250ms
      max_interval: 10s
      max_elapsed_time: 2m
  fdb_otel/secondary:
    cluster_file: ${env:FDB_CLUSTER_FILE}
    run: ${FDB_RUN}_secondary
service:
  extensions: [health_check, pprof]
  telemetry:
    logs:
      level: info
  pipelines:
    traces:
      receivers: [otlp, otlp/agents]
      processors: [memory_limiter, batch]
      exporters: [fdb_otel, debug, forward/fdb]
    traces/archive:
      receivers: [forward/fdb]
      processors: [attributes/cost]
      exporters: [fdb_otel/secondary]
    metrics:
      receivers: [otlp]
      exporters: [debug]
    logs/debug:
      receivers: [otlp]
      exporters: [debug]
`

var fullCollectorEnv = map[string]string{
	"FDB_CLUSTER_FILE": "/etc/foundationdb/fdb.cluster",
	"FDB_RUN":          "prod_traces_01",
	"FDB_CONSUMERS":    "8",
}

func fullPrimaryConfig() Config {
	return Config{
		Exporter: otel.ExporterConfig{
			ClusterFile: "/etc/foundationdb/fdb.cluster", Run: "prod_traces_01", Tenant: 42, Order: "trace", HashBytes: 16,
			PostingValueFormat: "json", RecordCompression: "zlib-1", BulkIndexConflicts: false, BatchSize: 32, Workers: 6,
			MaxCandidates: 5000, MaxBytes: 1 << 20, K: 25,
		},
		Timeout: 12 * time.Second,
		SendingQueue: QueueConfig{
			Enabled: true, NumConsumers: 8, QueueSize: 50000, BlockOnOverflow: true,
			Batch: BatchConfig{FlushTimeout: 150 * time.Millisecond, MinSize: 256, MaxSize: 2048},
		},
		RetryOnFailure: RetryConfig{Enabled: true, InitialInterval: 250 * time.Millisecond, MaxInterval: 10 * time.Second, MaxElapsedTime: 2 * time.Minute},
	}
}

func fullSecondaryConfig() Config {
	c := *NewFactory().CreateDefaultConfig()
	c.Exporter.ClusterFile = "/etc/foundationdb/fdb.cluster"
	c.Exporter.Run = "prod_traces_01_secondary"
	return c
}

func checkFullCollectorConfig(t *testing.T, cc *CollectorConfig) {
	t.Helper()
	id := func(s string) ComponentID {
		t.Helper()
		v, err := ParseComponentID(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	keys := func(m map[ComponentID]*Config) []string {
		var out []string
		for k := range m {
			out = append(out, k.String())
		}
		slices.Sort(out)
		return out
	}
	nodeKeys := func(m map[ComponentID]*yaml.Node) []string {
		var out []string
		for k := range m {
			out = append(out, k.String())
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"receivers", nodeKeys(cc.Receivers), []string{"otlp", "otlp/agents"}},
		{"processors", nodeKeys(cc.Processors), []string{"attributes/cost", "batch", "memory_limiter"}},
		{"exporters", nodeKeys(cc.Exporters), []string{"debug", "fdb_otel", "fdb_otel/secondary"}},
		{"connectors", nodeKeys(cc.Connectors), []string{"forward/fdb"}},
		{"extensions", nodeKeys(cc.Extensions), []string{"health_check", "pprof"}},
		{"fdb exporters", keys(cc.FDBExporters), []string{"fdb_otel", "fdb_otel/secondary"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if got, want := cc.ServiceExtensions, []ComponentID{id("health_check"), id("pprof")}; !slices.Equal(got, want) {
		t.Fatalf("service extensions = %v, want %v", got, want)
	}
	if got := *cc.FDBExporters[id("fdb_otel")]; got != fullPrimaryConfig() {
		t.Fatalf("fdb_otel config:\n got %+v\nwant %+v", got, fullPrimaryConfig())
	}
	if got := *cc.FDBExporters[id("fdb_otel/secondary")]; got != fullSecondaryConfig() {
		t.Fatalf("fdb_otel/secondary config:\n got %+v\nwant %+v", got, fullSecondaryConfig())
	}
	for _, cfg := range cc.FDBExporters {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("parsed config does not validate: %v", err)
		}
	}
	wantPipelines := map[string]Pipeline{
		"traces":         {Receivers: []ComponentID{id("otlp"), id("otlp/agents")}, Processors: []ComponentID{id("memory_limiter"), id("batch")}, Exporters: []ComponentID{id("fdb_otel"), id("debug"), id("forward/fdb")}},
		"traces/archive": {Receivers: []ComponentID{id("forward/fdb")}, Processors: []ComponentID{id("attributes/cost")}, Exporters: []ComponentID{id("fdb_otel/secondary")}},
		"metrics":        {Receivers: []ComponentID{id("otlp")}, Processors: []ComponentID{}, Exporters: []ComponentID{id("debug")}},
		"logs/debug":     {Receivers: []ComponentID{id("otlp")}, Processors: []ComponentID{}, Exporters: []ComponentID{id("debug")}},
	}
	if len(cc.Pipelines) != len(wantPipelines) {
		t.Fatalf("pipelines = %v, want %d", cc.Pipelines, len(wantPipelines))
	}
	for name, want := range wantPipelines {
		got := cc.Pipelines[id(name)]
		if got == nil {
			t.Fatalf("pipeline %s missing", name)
		}
		if !slices.Equal(got.Receivers, want.Receivers) || !slices.Equal(got.Processors, want.Processors) || !slices.Equal(got.Exporters, want.Exporters) {
			t.Fatalf("pipeline %s = %+v, want %+v", name, *got, want)
		}
	}
	if got, want := cc.TracesExporters(), []ComponentID{id("fdb_otel"), id("fdb_otel/secondary")}; !slices.Equal(got, want) {
		t.Fatalf("TracesExporters() = %v, want %v", got, want)
	}

	// Raw component bodies keep their content, with references expanded.
	var otlp struct {
		Protocols map[string]struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"protocols"`
	}
	if err := cc.Receivers[id("otlp")].Decode(&otlp); err != nil {
		t.Fatal(err)
	}
	if otlp.Protocols["grpc"].Endpoint != "0.0.0.0:4317" || otlp.Protocols["http"].Endpoint != "0.0.0.0:4318" {
		t.Fatalf("otlp receiver = %+v", otlp)
	}
	var attrs struct {
		Actions []struct {
			Key   string `yaml:"key"`
			Value string `yaml:"value"`
		} `yaml:"actions"`
	}
	if err := cc.Processors[id("attributes/cost")].Decode(&attrs); err != nil {
		t.Fatal(err)
	}
	if len(attrs.Actions) != 1 || attrs.Actions[0].Value != "cost$5" {
		t.Fatalf("$$ escape not applied: %+v", attrs)
	}
	var debug map[string]string
	if err := cc.Exporters[id("debug")].Decode(&debug); err != nil {
		t.Fatal(err)
	}
	if debug["verbosity"] != "detailed" {
		t.Fatalf("debug exporter = %v", debug)
	}
}

func TestParseConfigFullCollectorYAML(t *testing.T) {
	t.Parallel()
	cc, err := parseConfig([]byte(fullCollectorYAML), mapLookup(fullCollectorEnv))
	if err != nil {
		t.Fatal(err)
	}
	checkFullCollectorConfig(t, cc)

	// Explicit environment values override every ${...:-default}.
	env := map[string]string{"FDB_TENANT": "3", "FDB_BLOCK": "false", "FDB_FLUSH": "75ms", "OTLP_GRPC_ENDPOINT": "127.0.0.1:9999"}
	for k, v := range fullCollectorEnv {
		env[k] = v
	}
	cc, err = parseConfig([]byte(fullCollectorYAML), mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	got := cc.FDBExporters[ComponentID{Type: ComponentType}]
	if got.Exporter.Tenant != 3 || got.SendingQueue.BlockOnOverflow || got.SendingQueue.Batch.FlushTimeout != 75*time.Millisecond {
		t.Fatalf("environment overrides not applied: %+v", *got)
	}
}

// TestLoadPipelineConfigFromProcessEnvironment reads a Collector YAML file and
// resolves references against the real process environment (not parallel:
// it uses t.Setenv).
func TestLoadPipelineConfigFromProcessEnvironment(t *testing.T) {
	for k, v := range fullCollectorEnv {
		t.Setenv(k, v)
	}
	for _, unset := range []string{"FDB_TENANT", "FDB_BLOCK", "FDB_FLUSH", "OTLP_GRPC_ENDPOINT"} {
		if _, ok := os.LookupEnv(unset); ok {
			t.Skipf("%s is set in the test environment", unset)
		}
	}
	path := filepath.Join(t.TempDir(), "otelcol.yaml")
	if err := os.WriteFile(path, []byte(fullCollectorYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadPipelineConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	checkFullCollectorConfig(t, cc)

	direct, err := ParseConfig([]byte(fullCollectorYAML))
	if err != nil {
		t.Fatal(err)
	}
	for id, cfg := range cc.FDBExporters {
		if *direct.FDBExporters[id] != *cfg {
			t.Fatalf("ParseConfig and LoadPipelineConfig disagree on %s", id)
		}
	}

	_, err = LoadPipelineConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	requireErrContains(t, err, "collector config")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error should wrap os.ErrNotExist: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("bogus: 1\n"+fullCollectorYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadPipelineConfig(bad)
	requireErrContains(t, err, `unknown key "bogus"`)
}

func TestParseExporterConfig(t *testing.T) {
	t.Setenv("FDB_UNIT_CLUSTER", "/var/fdb/unit.cluster")
	t.Setenv("FDB_UNIT_QUEUE", "777")
	if _, ok := os.LookupEnv("FDB_UNIT_UNSET_TIMEOUT"); ok {
		t.Skip("FDB_UNIT_UNSET_TIMEOUT is set in the test environment")
	}

	cfg, err := ParseExporterConfig([]byte("cluster_file: /x.cluster\nrun: unit_run_01\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := *NewFactory().CreateDefaultConfig()
	want.Exporter.ClusterFile, want.Exporter.Run = "/x.cluster", "unit_run_01"
	if *cfg != want {
		t.Fatalf("minimal body:\n got %+v\nwant %+v", *cfg, want)
	}

	cfg, err = ParseExporterConfig([]byte(`
cluster_file: ${env:FDB_UNIT_CLUSTER}
run: unit_run_${env:FDB_UNIT_QUEUE}
timeout: ${env:FDB_UNIT_UNSET_TIMEOUT:-7s}
sending_queue:
  queue_size: ${FDB_UNIT_QUEUE}
  batch:
    min_size: 7
    max_size: 70
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exporter.ClusterFile != "/var/fdb/unit.cluster" || cfg.Exporter.Run != "unit_run_777" || cfg.Timeout != 7*time.Second ||
		cfg.SendingQueue.QueueSize != 777 || cfg.SendingQueue.Batch.MinSize != 7 || cfg.SendingQueue.Batch.MaxSize != 70 ||
		cfg.SendingQueue.NumConsumers != 4 || cfg.SendingQueue.Batch.FlushTimeout != 200*time.Millisecond {
		t.Fatalf("env-expanded body = %+v", *cfg)
	}

	// Disabled queue and retry sections skip their bounds.
	cfg, err = ParseExporterConfig([]byte("cluster_file: /x\nrun: unit_run_01\nsending_queue:\n  enabled: false\n  num_consumers: 0\n  queue_size: 0\nretry_on_failure:\n  enabled: false\n  initial_interval: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SendingQueue.Enabled || cfg.RetryOnFailure.Enabled {
		t.Fatalf("disabled sections = %+v", *cfg)
	}

	for _, tc := range []struct{ body, want string }{
		{"", "empty document"},
		{"~", "explicit owned FDB cluster file required"},
		{"[1, 2]", "exporter config must be a mapping"},
		{"cluster_file: /x\nrun: unit_run_01\ncluster_fil: y\n", `unknown key "cluster_fil"`},
		{"cluster_file: /x\nrun: unit_run_01\nexporter:\n  run: y\n", `unknown key "exporter"`},
		{"cluster_file: /x\nrun: unit_run_01\nsending_queue:\n  batch:\n    min_sz: 1\n", `fdb_otel.sending_queue.batch: unknown key "min_sz"`},
		{"cluster_file: /x\nrun: unit_run_01\nretry_on_failure:\n  multiplier: 2\n", `fdb_otel.retry_on_failure: unknown key "multiplier"`},
		{"cluster_file: /x\nrun: BAD\n", "run must be an explicit fresh"},
		{"cluster_file: /x\nrun: unit_run_01\ntimeout: 30\n", "time.Duration"},
		{"cluster_file: /x\nrun: unit_run_01\ntenant: \"${env:FDB_UNIT_QUEUE}\"\n", "cannot unmarshal !!str"},
		{"cluster_file: /x\nrun: ${env:FDB_UNIT_CLUSTER\n", "unterminated reference"},
	} {
		_, err := ParseExporterConfig([]byte(tc.body))
		requireErrContains(t, err, tc.want)
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	if err := unitConfig(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"cluster_file", func(c *Config) { c.Exporter.ClusterFile = "" }, "explicit owned FDB cluster file required"},
		{"run", func(c *Config) { c.Exporter.Run = "x" }, "run must be an explicit fresh"},
		{"batch_size", func(c *Config) { c.Exporter.BatchSize = 65 }, "batch_size must be 1..64"},
		{"workers", func(c *Config) { c.Exporter.Workers = 0 }, "workers must be 1..16"},
		{"tenant", func(c *Config) { c.Exporter.Tenant = -1 }, "tenant must be 0.."},
		{"timeout", func(c *Config) { c.Timeout = 0 }, "timeout must be positive"},
		{"num_consumers low", func(c *Config) { c.SendingQueue.NumConsumers = 0 }, "sending_queue.num_consumers must be 1..64"},
		{"num_consumers high", func(c *Config) { c.SendingQueue.NumConsumers = 65 }, "sending_queue.num_consumers must be 1..64"},
		{"queue_size low", func(c *Config) { c.SendingQueue.QueueSize = 0 }, "sending_queue.queue_size must be 1..10000000 spans"},
		{"queue_size high", func(c *Config) { c.SendingQueue.QueueSize = 10_000_001 }, "sending_queue.queue_size must be 1..10000000 spans"},
		{"flush_timeout", func(c *Config) { c.SendingQueue.Batch.FlushTimeout = 0 }, "sending_queue.batch.flush_timeout must be positive"},
		{"min_size low", func(c *Config) { c.SendingQueue.Batch.MinSize = 0 }, "sending_queue.batch.min_size must be 1..queue_size"},
		{"min_size above queue", func(c *Config) { c.SendingQueue.Batch.MinSize = 1001; c.SendingQueue.Batch.MaxSize = 2000 }, "sending_queue.batch.min_size must be 1..queue_size"},
		{"max_size", func(c *Config) { c.SendingQueue.Batch.MinSize = 10; c.SendingQueue.Batch.MaxSize = 9 }, "sending_queue.batch.max_size must be at least min_size"},
		{"initial_interval", func(c *Config) { c.RetryOnFailure.InitialInterval = 0 }, "retry_on_failure.initial_interval must be positive"},
		{"max_interval", func(c *Config) { c.RetryOnFailure.MaxInterval = c.RetryOnFailure.InitialInterval / 2 }, "retry_on_failure.max_interval must be at least initial_interval"},
		{"max_elapsed_time", func(c *Config) { c.RetryOnFailure.MaxElapsedTime = -time.Second }, "retry_on_failure.max_elapsed_time must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unitConfig(tc.mut).Validate()
			requireErrContains(t, err, tc.want)
			if !strings.HasPrefix(err.Error(), "fdb_otel config: ") {
				t.Fatalf("missing component prefix: %v", err)
			}
		})
	}
	// Several violations are all reported.
	err := unitConfig(func(c *Config) { c.Timeout = 0; c.SendingQueue.NumConsumers = 0; c.RetryOnFailure.InitialInterval = 0 }).Validate()
	for _, want := range []string{"timeout must be positive", "num_consumers", "initial_interval"} {
		requireErrContains(t, err, want)
	}
	// Disabled sections are not bounds-checked.
	if err := unitConfig(func(c *Config) {
		c.SendingQueue = QueueConfig{Enabled: false}
		c.RetryOnFailure = RetryConfig{Enabled: false}
	}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func collectorDoc(exporters, service string) string {
	return "receivers:\n  otlp: {}\nprocessors:\n  batch: {}\nexporters:\n" + exporters + "service:\n" + service
}

const (
	okExporter = "  fdb_otel:\n    cluster_file: /fdb.cluster\n    run: unit_run_01\n"
	okService  = "  pipelines:\n    traces:\n      receivers: [otlp]\n      processors: [batch]\n      exporters: [fdb_otel]\n"
)

func TestParseConfigRejections(t *testing.T) {
	t.Parallel()
	lookup := mapLookup(map[string]string{"FDB_TENANT": "4"})
	base := collectorDoc(okExporter, okService)
	cc, err := parseConfig([]byte(base), lookup)
	if err != nil {
		t.Fatalf("base document must parse: %v", err)
	}
	if got := cc.TracesExporters(); !slices.Equal(got, []ComponentID{{Type: ComponentType}}) {
		t.Fatalf("TracesExporters() = %v", got)
	}
	pipelines := func(body string) string { return collectorDoc(okExporter, "  pipelines:\n"+body) }
	exporter := func(extra string) string { return collectorDoc(okExporter+extra, okService) }
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		// Unknown keys at every level.
		{"unknown top-level key", "bogus: 1\n" + base, `config: unknown key "bogus"`},
		{"misspelled section", strings.Replace(base, "processors:", "processor:", 1), `unknown key "processor"`},
		{"unknown exporter key", exporter("    cluster_fil: /x\n"), `fdb_otel: unknown key "cluster_fil"`},
		{"nested exporter mapping", exporter("    exporter:\n      run: x\n"), `unknown key "exporter"`},
		{"unknown queue key", exporter("    sending_queue:\n      size: 10\n"), `fdb_otel.sending_queue: unknown key "size"`},
		{"unknown batch key", exporter("    sending_queue:\n      batch:\n        min_sz: 1\n"), `fdb_otel.sending_queue.batch: unknown key "min_sz"`},
		{"unknown retry key", exporter("    retry_on_failure:\n      multiplier: 2\n"), `fdb_otel.retry_on_failure: unknown key "multiplier"`},
		{"unknown key in named fdb exporter", exporter("  fdb_otel/secondary:\n    cluster_file: /x\n    run: unit_run_02\n    bogus: 1\n"), `fdb_otel/secondary: fdb_otel: unknown key "bogus"`},
		{"unknown service key", collectorDoc(okExporter, "  pipeline:\n    traces: {}\n"), `config.service: unknown key "pipeline"`},
		{"unknown pipeline key", pipelines("    traces:\n      receiver: [otlp]\n      exporters: [fdb_otel]\n"), `config.service.pipelines.traces: unknown key "receiver"`},
		{"duplicate mapping key", exporter(okExporter), "already defined"},

		// Invalid component identities.
		{"receiver id digit", strings.Replace(base, "  otlp: {}", "  1otlp: {}", 1), `receivers: invalid component id "1otlp"`},
		{"receiver empty name", strings.Replace(base, "  otlp: {}", "  otlp/: {}", 1), `receivers: invalid component id "otlp/"`},
		{"receiver name with space", strings.Replace(base, "  otlp: {}", "  \"otlp/a b\": {}", 1), `receivers: invalid component id "otlp/a b"`},
		{"exporter id hyphen", strings.Replace(base, "  fdb_otel:\n", "  fdb-otel:\n", 1), `exporters: invalid component id "fdb-otel"`},
		{"processor id", strings.Replace(base, "  batch: {}", "  _batch: {}", 1), `processors: invalid component id "_batch"`},
		{"pipeline id", pipelines("    traces/:\n      receivers: [otlp]\n      exporters: [fdb_otel]\n"), `service.pipelines: invalid component id "traces/"`},
		{"pipeline reference id", pipelines("    traces:\n      receivers: [otlp, 9bad]\n      exporters: [fdb_otel]\n"), `service.pipelines.traces.receivers: invalid component id "9bad"`},

		// Pipeline structure.
		{"unconfigured exporter", pipelines("    traces:\n      receivers: [otlp]\n      exporters: [fdb_otel, otlphttp]\n"), "service.pipelines.traces.exporters: otlphttp is not configured"},
		{"unconfigured processor", pipelines("    traces:\n      receivers: [otlp]\n      processors: [filter]\n      exporters: [fdb_otel]\n"), "service.pipelines.traces.processors: filter is not configured"},
		{"receiver used as exporter", pipelines("    traces:\n      receivers: [otlp]\n      exporters: [otlp]\n"), "service.pipelines.traces.exporters: otlp is not configured"},
		{"duplicate reference", pipelines("    traces:\n      receivers: [otlp, otlp]\n      exporters: [fdb_otel]\n"), "otlp referenced more than once"},
		{"no receivers", pipelines("    traces:\n      receivers: []\n      exporters: [fdb_otel]\n"), "service.pipelines.traces: must have at least one receiver"},
		{"no exporters", pipelines("    traces:\n      receivers: [otlp]\n"), "service.pipelines.traces: must have at least one exporter"},
		{"no pipelines", collectorDoc(okExporter, "  extensions: []\n"), "service.pipelines must define at least one pipeline"},
		{"unknown signal", pipelines("    events:\n      receivers: [otlp]\n      exporters: [fdb_otel]\n"), `service.pipelines.events: unknown signal "events"`},
		{"unconfigured extension", collectorDoc(okExporter, "  extensions: [zpages]\n"+okService), "service.extensions: zpages is not configured"},

		// fdb_otel supports only traces.
		{"metrics pipeline", pipelines("    metrics:\n      receivers: [otlp]\n      exporters: [fdb_otel]\n"), "service.pipelines.metrics: exporter fdb_otel supports only the traces signal"},
		{"named logs pipeline", pipelines("    traces:\n      receivers: [otlp]\n      exporters: [fdb_otel]\n    logs/audit:\n      receivers: [otlp]\n      exporters: [fdb_otel]\n"), "service.pipelines.logs/audit: exporter fdb_otel supports only the traces signal"},
		{"profiles pipeline", pipelines("    profiles:\n      receivers: [otlp]\n      exporters: [fdb_otel]\n"), "service.pipelines.profiles: exporter fdb_otel supports only the traces signal"},

		// Invalid fdb_otel bodies.
		{"null exporter body", collectorDoc("  fdb_otel:\n", okService), "explicit owned FDB cluster file required"},
		{"sequence exporter body", collectorDoc("  fdb_otel: [a, b]\n", okService), "exporter config must be a mapping"},
		{"missing cluster_file", collectorDoc("  fdb_otel:\n    run: unit_run_01\n", okService), "explicit owned FDB cluster file required"},
		{"batch_size", exporter("    batch_size: 65\n"), "batch_size must be 1..64"},
		{"queue min above max", exporter("    sending_queue:\n      batch:\n        min_size: 100\n        max_size: 10\n"), "max_size must be at least min_size"},
		{"num_consumers", exporter("    sending_queue:\n      num_consumers: 0\n"), "num_consumers must be 1..64"},
		{"retry intervals", exporter("    retry_on_failure:\n      initial_interval: 10s\n      max_interval: 1s\n"), "max_interval must be at least initial_interval"},
		{"integer duration", exporter("    timeout: 30\n"), "time.Duration"},
		{"non-integer queue size", exporter("    sending_queue:\n      queue_size: lots\n"), "cannot unmarshal"},
		{"quoted env keeps string type", exporter("    tenant: \"${env:FDB_TENANT}\"\n"), "cannot unmarshal !!str"},
		{"unterminated env", exporter("    order: ${env:FDB_TENANT\n"), "unterminated reference"},
		{"invalid env name", exporter("    order: ${env:FDB-TENANT}\n"), "invalid environment reference"},

		// Documents.
		{"empty document", "", "empty document"},
		{"root sequence", "- a\n", "cannot unmarshal"},
		{"malformed yaml", "receivers: [\n", "collector config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cc, err := parseConfig([]byte(tc.doc), lookup)
			requireErrContains(t, err, tc.want)
			if cc != nil {
				t.Fatalf("rejected config returned %+v", cc)
			}
		})
	}

	// An invalid fdb_otel body is a classified, permanent configuration error.
	_, err = parseConfig([]byte(collectorDoc("  fdb_otel:\n    run: unit_run_01\n", okService)), lookup)
	var oe *otel.OtelError
	if !errors.As(err, &oe) || oe.Kind != "config" || !IsPermanent(err) {
		t.Fatalf("want permanent otel config error, got %T: %v", err, err)
	}

	// A plain env reference retypes the scalar, so it can supply an integer.
	cc, err = parseConfig([]byte(exporter("    tenant: ${env:FDB_TENANT}\n    workers: ${env:FDB_WORKERS:-3}\n")), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got := cc.FDBExporters[ComponentID{Type: ComponentType}].Exporter; got.Tenant != 4 || got.Workers != 3 {
		t.Fatalf("plain env integers not decoded: %+v", got)
	}

	// Connectors satisfy both receiver and exporter references.
	cc, err = parseConfig([]byte("receivers:\n  otlp: {}\nconnectors:\n  forward: {}\nexporters:\n"+okExporter+
		"service:\n  pipelines:\n    traces/in:\n      receivers: [otlp]\n      exporters: [forward]\n    traces/out:\n      receivers: [forward]\n      exporters: [fdb_otel]\n"), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got := cc.TracesExporters(); !slices.Equal(got, []ComponentID{{Type: ComponentType}}) {
		t.Fatalf("TracesExporters() through connector = %v", got)
	}
}

// ---------------------------------------------------------------------------
// Factory, component identity, errors
// ---------------------------------------------------------------------------

func TestFactory(t *testing.T) {
	t.Parallel()
	f := NewFactory()
	if f.Type() != "fdb_otel" || f.TracesStability() != "alpha" {
		t.Fatalf("factory = %q/%q", f.Type(), f.TracesStability())
	}
	def := f.CreateDefaultConfig()
	want := Config{
		Exporter:       otel.DefaultExporterConfig(),
		Timeout:        30 * time.Second,
		SendingQueue:   QueueConfig{Enabled: true, NumConsumers: 4, QueueSize: 10000, Batch: BatchConfig{FlushTimeout: 200 * time.Millisecond, MinSize: 512, MaxSize: 4096}},
		RetryOnFailure: RetryConfig{Enabled: true, InitialInterval: 5 * time.Second, MaxInterval: 30 * time.Second, MaxElapsedTime: 300 * time.Second},
	}
	if *def != want {
		t.Fatalf("default config:\n got %+v\nwant %+v", *def, want)
	}
	if f.CreateDefaultConfig() == def {
		t.Fatal("CreateDefaultConfig must return a fresh config")
	}
	requireErrContains(t, def.Validate(), "explicit owned FDB cluster file required")

	_, err := f.CreateTraces(Settings{}, nil)
	requireErrContains(t, err, "nil config")
	_, err = f.CreateTraces(Settings{ID: ComponentID{Type: "otlp"}}, unitConfig(nil))
	requireErrContains(t, err, "component id otlp has the wrong type")
	_, err = f.CreateTraces(Settings{}, def)
	requireErrContains(t, err, "explicit owned FDB cluster file required")

	cfg := unitConfig(nil)
	x, err := f.CreateTraces(Settings{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if x.ID() != (ComponentID{Type: ComponentType}) {
		t.Fatalf("default id = %v", x.ID())
	}
	if x.Capabilities().MutatesData {
		t.Fatal("exporter must not claim to mutate data")
	}
	cfg.Timeout = time.Minute
	if x.Config().Timeout != 5*time.Second {
		t.Fatal("exporter must copy its config")
	}
	if x.Sink() != nil {
		t.Fatal("sink must be nil before Start")
	}
	named, err := f.CreateTraces(Settings{ID: ComponentID{Type: ComponentType, Name: "secondary"}}, unitConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	if named.ID().String() != "fdb_otel/secondary" {
		t.Fatalf("named id = %v", named.ID())
	}
}

func TestIsPermanent(t *testing.T) {
	t.Parallel()
	oe := func(kind string) *otel.OtelError { return &otel.OtelError{Kind: kind, Op: "unit", Message: kind} }
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errTransient, false},
		{context.DeadlineExceeded, false},
		{ErrQueueFull, false},
		{ErrNotRunning, false},
		{oe("budget"), false},
		{oe("canceled"), false},
		{&otel.QueryBudgetError{OtelError: oe("budget")}, false},
		{oe("config"), true},
		{oe("admission"), true},
		{oe("ownership"), true},
		{oe("corruption"), true},
		{oe("operation"), true},
		{&otel.ConfigError{OtelError: oe("config")}, true},
		{&otel.AdmissionError{OtelError: oe("admission")}, true},
		{fmt.Errorf("wrapped: %w", error(&otel.CorruptionError{OtelError: oe("corruption")})), true},
		{errors.Join(errTransient, oe("ownership")), true},
		{permanent("span %d bad", 1), true},
		{fmt.Errorf("outer: %w", permanent("inner")), true},
	} {
		if got := IsPermanent(tc.err); got != tc.want {
			t.Fatalf("IsPermanent(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	if p := permanent("x %d", 7); p.Error() != "fdb_otel: x 7" {
		t.Fatalf("permanent message = %q", p.Error())
	}
}

// ---------------------------------------------------------------------------
// cutBatch / splitSpans
// ---------------------------------------------------------------------------

func TestCutBatchSplitsAtSpanBoundaries(t *testing.T) {
	t.Parallel()
	// Request 1 spans two resources and three scopes.
	r1 := append(testRequest("s", 3, 2), testRequest("s", 5, 3)...)
	r1[1].ScopeSpans = append(r1[1].ScopeSpans, &tracepb.ScopeSpans{Scope: &commonpb.InstrumentationScope{Name: "other"}})
	r1[1].ScopeSpans[1].Spans = append(r1[1].ScopeSpans[1].Spans, r1[1].ScopeSpans[0].Spans[2])
	r1[1].ScopeSpans[0].Spans = r1[1].ScopeSpans[0].Spans[:2]
	cur := []request{
		{seq: 10, spans: testRequest("s", 0, 3), count: 3},
		{seq: 11, spans: r1, count: 5},
		{seq: 12, spans: testRequest("s", 8, 2), count: 2},
	}
	type part struct {
		seq   uint64
		count int
		names []string
	}
	parts := func(b batch) []part {
		var out []part
		for _, p := range b.parts {
			out = append(out, part{p.seq, p.count, spanNames(p.spans)})
		}
		return out
	}
	equal := func(a, b []part) bool {
		return slices.EqualFunc(a, b, func(x, y part) bool { return x.seq == y.seq && x.count == y.count && slices.Equal(x.names, y.names) })
	}

	b, cur, count := cutBatch(cur, 10, 4)
	if want := []part{{10, 3, rangeNames("s", 0, 3)}, {11, 1, rangeNames("s", 3, 1)}}; b.count != 4 || count != 6 || !equal(parts(b), want) {
		t.Fatalf("first cut = %d %+v (rest %d), want %+v", b.count, parts(b), count, want)
	}
	if len(cur) != 2 || cur[0].seq != 11 || cur[0].count != 4 || !slices.Equal(spanNames(cur[0].spans), rangeNames("s", 4, 4)) {
		t.Fatalf("remainder after first cut = %+v", cur)
	}
	if got := spanNames(b.resourceSpans()); !slices.Equal(got, rangeNames("s", 0, 4)) {
		t.Fatalf("batch resource spans = %v", got)
	}

	b, cur, count = cutBatch(cur, count, 4)
	if want := []part{{11, 4, rangeNames("s", 4, 4)}}; b.count != 4 || count != 2 || !equal(parts(b), want) {
		t.Fatalf("second cut = %d %+v (rest %d), want %+v", b.count, parts(b), count, want)
	}
	b, cur, count = cutBatch(cur, count, 4)
	if want := []part{{12, 2, rangeNames("s", 8, 2)}}; b.count != 2 || count != 0 || len(cur) != 0 || !equal(parts(b), want) {
		t.Fatalf("final cut = %d %+v (rest %d, %d requests), want %+v", b.count, parts(b), count, len(cur), want)
	}

	// A max covering the whole queue takes every request unsplit.
	all := []request{{seq: 1, spans: testRequest("t", 0, 3), count: 3}, {seq: 2, spans: testRequest("t", 3, 3), count: 3}}
	b, rest, n := cutBatch(all, 6, 100)
	if b.count != 6 || n != 0 || len(rest) != 0 || len(b.parts) != 2 || !slices.Equal(spanNames(b.resourceSpans()), rangeNames("t", 0, 6)) {
		t.Fatalf("whole cut = %+v rest=%v n=%d", b, rest, n)
	}

	// One huge request is cut into max-sized pieces of the same sequence.
	cur = []request{{seq: 5, spans: testRequest("u", 0, 10), count: 10}}
	count = 10
	var sizes []int
	var names []string
	for count > 0 {
		b, cur, count = cutBatch(cur, count, 3)
		if len(b.parts) != 1 || b.parts[0].seq != 5 {
			t.Fatalf("piece parts = %+v", b.parts)
		}
		sizes = append(sizes, b.count)
		names = append(names, spanNames(b.resourceSpans())...)
	}
	if !slices.Equal(sizes, []int{3, 3, 3, 1}) || !slices.Equal(names, rangeNames("u", 0, 10)) {
		t.Fatalf("pieces = %v names=%v", sizes, names)
	}
}

func TestSplitSpansPreservesWrappers(t *testing.T) {
	t.Parallel()
	resA := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}}}}}
	resB := &resourcepb.Resource{DroppedAttributesCount: 2}
	scopeA := &commonpb.InstrumentationScope{Name: "http", Version: "1"}
	scopeB := &commonpb.InstrumentationScope{Name: "db"}
	scopeC := &commonpb.InstrumentationScope{Name: "rpc"}
	s := func(n int) *tracepb.Span { return testSpan("w", n) }
	ssA := &tracepb.ScopeSpans{Scope: scopeA, SchemaUrl: "scope-a", Spans: []*tracepb.Span{s(0), nil, s(1), s(2)}}
	ssA.ProtoReflect().SetUnknown(unknownField(900, 1))
	rsA := &tracepb.ResourceSpans{Resource: resA, SchemaUrl: "res-a", ScopeSpans: []*tracepb.ScopeSpans{ssA, nil, {Scope: scopeB, SchemaUrl: "scope-b", Spans: []*tracepb.Span{s(3)}}}}
	rsA.ProtoReflect().SetUnknown(unknownField(901, 2))
	rsB := &tracepb.ResourceSpans{Resource: resB, ScopeSpans: []*tracepb.ScopeSpans{{Scope: scopeC, Spans: []*tracepb.Span{s(4), s(5)}}}}
	in := []*tracepb.ResourceSpans{nil, rsA, rsB}
	beforeA, beforeB := proto.Clone(rsA), proto.Clone(rsB)

	for n := 0; n <= 7; n++ {
		head, tail := splitSpans(in, n)
		k := min(n, 6)
		if got := spanNames(head); !slices.Equal(got, rangeNames("w", 0, k)) {
			t.Fatalf("n=%d head = %v", n, got)
		}
		if got := spanNames(tail); !slices.Equal(got, rangeNames("w", k, 6-k)) {
			t.Fatalf("n=%d tail = %v", n, got)
		}
		for _, side := range [][]*tracepb.ResourceSpans{head, tail} {
			for _, rs := range side {
				if rs == nil || len(rs.ScopeSpans) == 0 {
					t.Fatalf("n=%d: empty wrapper emitted: %v", n, side)
				}
				for _, ss := range rs.ScopeSpans {
					if len(ss.Spans) == 0 || slices.Contains(ss.Spans, nil) {
						t.Fatalf("n=%d: empty scope or nil span emitted", n)
					}
				}
			}
		}
	}
	if n := 0; true {
		head, _ := splitSpans(in, n)
		if head != nil {
			t.Fatalf("n=0 head = %v", head)
		}
		_, tail := splitSpans(in, 6)
		if tail != nil {
			t.Fatalf("n=6 tail = %v", tail)
		}
	}

	// Split inside scope A: both sides keep the resource, scope, schema URLs and
	// unknown fields of their wrappers (shared, not copied).
	head, tail := splitSpans(in, 2)
	if len(head) != 1 || len(tail) != 2 {
		t.Fatalf("split = %d/%d resources", len(head), len(tail))
	}
	h, t0, t1 := head[0], tail[0], tail[1]
	for _, rs := range []*tracepb.ResourceSpans{h, t0} {
		if rs.Resource != resA || rs.SchemaUrl != "res-a" || string(rs.ProtoReflect().GetUnknown()) != string(unknownField(901, 2)) {
			t.Fatalf("resource wrapper not preserved: %v", rs)
		}
	}
	if h.ScopeSpans[0].Scope != scopeA || t0.ScopeSpans[0].Scope != scopeA || h.ScopeSpans[0].SchemaUrl != "scope-a" || t0.ScopeSpans[0].SchemaUrl != "scope-a" {
		t.Fatal("split scope identity not preserved")
	}
	for _, ss := range []*tracepb.ScopeSpans{h.ScopeSpans[0], t0.ScopeSpans[0]} {
		if string(ss.ProtoReflect().GetUnknown()) != string(unknownField(900, 1)) {
			t.Fatal("scope unknown fields not preserved on both sides")
		}
	}
	if len(t0.ScopeSpans) != 2 || t0.ScopeSpans[1].Scope != scopeB || t0.ScopeSpans[1].SchemaUrl != "scope-b" {
		t.Fatalf("tail scopes = %v", t0.ScopeSpans)
	}
	if t1.Resource != resB || t1.ScopeSpans[0].Scope != scopeC {
		t.Fatalf("untouched resource = %v", t1)
	}
	if h.ScopeSpans[0].Spans[0] != ssA.Spans[0] {
		t.Fatal("spans must be moved, not copied")
	}
	// The input is never modified.
	if !proto.Equal(rsA, beforeA) || !proto.Equal(rsB, beforeB) || len(ssA.Spans) != 4 || len(rsA.ScopeSpans) != 3 {
		t.Fatal("splitSpans mutated its input")
	}
}

// ---------------------------------------------------------------------------
// Sending queue: batching, min/max size, flush_timeout, overflow
// ---------------------------------------------------------------------------

func TestQueueMergesConcurrentSourcesIntoMaxSizeBatches(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	x := startUnitExporter(t, unitConfig(func(c *Config) {
		c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 8, MaxSize: 8}
	}), sink)
	const agents, perAgent = 8, 3
	start := make(chan struct{})
	var wg sync.WaitGroup
	for a := range agents {
		wg.Go(func() {
			<-start
			if err := x.ConsumeTraces(t.Context(), testRequest(fmt.Sprintf("a%d-", a), 0, perAgent)); err != nil {
				t.Errorf("agent %d: %v", a, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if err := flush(t, x); err != nil {
		t.Fatal(err)
	}
	calls := sink.snapshot()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3 batches of 8", len(calls))
	}
	var all []string
	for _, c := range calls {
		if len(c.names) != 8 {
			t.Fatalf("batch size = %d, want 8 (%v)", len(c.names), c.names)
		}
		all = append(all, c.names...)
	}
	// Every span exported once; each source's spans keep FIFO order even when
	// its request is split across batches.
	seen := map[string]int{}
	for i, n := range all {
		if _, dup := seen[n]; dup {
			t.Fatalf("span %s exported twice", n)
		}
		seen[n] = i
	}
	for a := range agents {
		names := rangeNames(fmt.Sprintf("a%d-", a), 0, perAgent)
		for i, n := range names {
			pos, ok := seen[n]
			if !ok {
				t.Fatalf("span %s never exported", n)
			}
			if i > 0 && pos < seen[names[i-1]] {
				t.Fatalf("agent %d spans out of order", a)
			}
		}
	}
	st := x.Stats()
	if st.AcceptedRequests != agents || st.AcceptedSpans != 24 || st.ExportedSpans != 24 || st.StoredSpans != 24 || st.Flushes != 3 || st.Exports != 3 || st.QueuedSpans != 0 || st.FailedSpans != 0 || st.Retries != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestQueueMinSizeAndFlushTimeout(t *testing.T) {
	t.Parallel()
	t.Run("held_below_min_size_until_flush", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{}
		x := startUnitExporter(t, unitConfig(func(c *Config) {
			c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 10, MaxSize: 100}
		}), sink)
		if err := x.ConsumeTraces(t.Context(), testRequest("m", 0, 4)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		if n := sink.callCount(); n != 0 {
			t.Fatalf("partial batch below min_size exported early (%d calls)", n)
		}
		if q := x.Stats().QueuedSpans; q != 4 {
			t.Fatalf("queued = %d, want 4", q)
		}
		if err := flush(t, x); err != nil {
			t.Fatal(err)
		}
		if calls := sink.snapshot(); len(calls) != 1 || !slices.Equal(calls[0].names, rangeNames("m", 0, 4)) {
			t.Fatalf("flush calls = %+v", calls)
		}
		// Reaching min_size dispatches without a flush or timer.
		if err := x.ConsumeTraces(t.Context(), testRequest("m", 4, 6)); err != nil {
			t.Fatal(err)
		}
		if err := x.ConsumeTraces(t.Context(), testRequest("m", 10, 4)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "min_size batch", func() bool { return sink.callCount() == 2 })
		if calls := sink.snapshot(); !slices.Equal(calls[1].names, rangeNames("m", 4, 10)) {
			t.Fatalf("min_size batch = %v", calls[1].names)
		}
		if st := x.Stats(); st.Flushes != 2 || st.QueuedSpans != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})
	t.Run("flush_timeout_dispatches_partial_batch", func(t *testing.T) {
		t.Parallel()
		const timeout = 30 * time.Millisecond
		sink := &fakeSink{}
		x := startUnitExporter(t, unitConfig(func(c *Config) {
			c.SendingQueue.Batch = BatchConfig{FlushTimeout: timeout, MinSize: 100, MaxSize: 1000}
		}), sink)
		for round := range 2 {
			begin := time.Now()
			if err := x.ConsumeTraces(t.Context(), testRequest("f", round*4, 4)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "flush_timeout dispatch", func() bool { return sink.callCount() == round+1 })
			c := sink.snapshot()[round]
			if waited := c.at.Sub(begin); waited < timeout {
				t.Fatalf("round %d exported after %v, before flush_timeout %v", round, waited, timeout)
			}
			if !slices.Equal(c.names, rangeNames("f", round*4, 4)) {
				t.Fatalf("round %d names = %v", round, c.names)
			}
		}
		if st := x.Stats(); st.Flushes != 2 || st.ExportedSpans != 8 {
			t.Fatalf("stats = %+v", st)
		}
	})
}

func TestQueueMaxSizeSplitsRequestsAndFlushWaitsForAllParts(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	sink := &fakeSink{export: gated(release)}
	x := startUnitExporter(t, unitConfig(func(c *Config) {
		c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 1, MaxSize: 4}
	}), sink)
	if err := x.ConsumeTraces(t.Context(), testRequest("x", 0, 10)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first piece in flight", func() bool { return sink.callCount() == 1 })

	// A bounded Flush gives up while pieces are still in flight.
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err := x.Flush(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded flush = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- flush(t, x) }()
	select {
	case err := <-done:
		t.Fatalf("Flush returned before the split request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	calls := sink.snapshot()
	var sizes []int
	var names []string
	for _, c := range calls {
		sizes = append(sizes, len(c.names))
		names = append(names, c.names...)
	}
	if !slices.Equal(sizes, []int{4, 4, 2}) || !slices.Equal(names, rangeNames("x", 0, 10)) {
		t.Fatalf("pieces = %v names = %v", sizes, names)
	}
	if st := x.Stats(); st.AcceptedRequests != 1 || st.ExportedSpans != 10 || st.Flushes != 3 || st.QueuedSpans != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestQueueNumConsumersBoundsConcurrentExports(t *testing.T) {
	t.Parallel()
	const consumers = 4
	var mu sync.Mutex
	inflight, peak := 0, 0
	full := make(chan struct{})
	sink := &fakeSink{export: func(ctx context.Context, _ int, names []string) (otel.ExportStats, error) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		if inflight == consumers {
			select {
			case <-full:
			default:
				close(full)
			}
		}
		mu.Unlock()
		defer func() { mu.Lock(); inflight--; mu.Unlock() }()
		select {
		case <-full:
		case <-ctx.Done():
			return otel.ExportStats{}, ctx.Err()
		}
		return okStats(len(names)), nil
	}}
	x := startUnitExporter(t, unitConfig(func(c *Config) {
		c.SendingQueue.NumConsumers = consumers
		c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 1, MaxSize: 1}
	}), sink)
	for i := range 20 {
		if err := x.ConsumeTraces(t.Context(), testRequest("c", i, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := flush(t, x); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != consumers {
		t.Fatalf("peak concurrent exports = %d, want %d", peak, consumers)
	}
	if st := x.Stats(); st.ExportedSpans != 20 || st.Exports != 20 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestQueueFullWithoutBlockOnOverflow(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	sink := &fakeSink{export: gated(release)}
	x := startUnitExporter(t, unitConfig(func(c *Config) {
		c.SendingQueue.QueueSize = 6
		c.SendingQueue.BlockOnOverflow = false
		c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 1, MaxSize: 100}
	}), sink)
	ctx := t.Context()
	if err := x.ConsumeTraces(ctx, testRequest("q", 0, 4)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "export in flight", func() bool { return sink.callCount() == 1 })
	err := x.ConsumeTraces(ctx, testRequest("q", 4, 3))
	if !errors.Is(err, ErrQueueFull) || IsPermanent(err) {
		t.Fatalf("overflow = %v, want retryable ErrQueueFull", err)
	}
	if err := x.ConsumeTraces(ctx, testRequest("q", 7, 2)); err != nil {
		t.Fatalf("request fitting the remaining capacity: %v", err)
	}
	if err := x.ConsumeTraces(ctx, testRequest("q", 9, 1)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue = %v", err)
	}
	err = x.ConsumeTraces(ctx, testRequest("q", 10, 7))
	if errors.Is(err, ErrQueueFull) || !IsPermanent(err) {
		t.Fatalf("oversized request = %v, want permanent rejection", err)
	}
	requireErrContains(t, err, "request of 7 spans exceeds sending_queue.queue_size 6")
	if st := x.Stats(); st.RejectedSpans != 11 || st.AcceptedSpans != 6 || st.AcceptedRequests != 2 || st.QueuedSpans != 6 {
		t.Fatalf("stats while full = %+v", st)
	}
	close(release)
	if err := flush(t, x); err != nil {
		t.Fatal(err)
	}
	if err := x.ConsumeTraces(ctx, testRequest("q", 20, 6)); err != nil {
		t.Fatalf("capacity not released after export: %v", err)
	}
	if err := flush(t, x); err != nil {
		t.Fatal(err)
	}
	got := sink.exportedNames()
	slices.Sort(got)
	want := append(append(rangeNames("q", 0, 4), rangeNames("q", 7, 2)...), rangeNames("q", 20, 6)...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("exported = %v, want %v", got, want)
	}
	if st := x.Stats(); st.ExportedSpans != 12 || st.QueuedSpans != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestQueueBlockOnOverflowWaitsForCapacity(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	sink := &fakeSink{export: gated(release)}
	x := startUnitExporter(t, unitConfig(func(c *Config) {
		c.SendingQueue.QueueSize = 6
		c.SendingQueue.BlockOnOverflow = true
		c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 1, MaxSize: 100}
	}), sink)
	if err := x.ConsumeTraces(t.Context(), testRequest("b", 0, 4)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "export in flight", func() bool { return sink.callCount() == 1 })

	// The caller's context bounds the wait.
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err := x.ConsumeTraces(short, testRequest("b", 100, 4))
	cancel()
	if !errors.Is(err, ErrQueueFull) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded blocking enqueue = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- x.ConsumeTraces(t.Context(), testRequest("b", 4, 4)) }()
	select {
	case err := <-done:
		t.Fatalf("enqueue did not block on a full queue: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked enqueue never admitted")
	}
	if err := flush(t, x); err != nil {
		t.Fatal(err)
	}
	if got := sink.exportedNames(); !slices.Equal(got, rangeNames("b", 0, 8)) {
		t.Fatalf("exported = %v", got)
	}
	if st := x.Stats(); st.ExportedSpans != 8 || st.RejectedSpans != 4 || st.AcceptedRequests != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

func syncConfig(retry RetryConfig) *Config {
	return unitConfig(func(c *Config) {
		c.SendingQueue = QueueConfig{Enabled: false}
		c.RetryOnFailure = retry
	})
}

func TestRetryTransientErrorsWithExponentialBackoff(t *testing.T) {
	t.Parallel()
	transient := []error{
		errTransient,
		&otel.QueryBudgetError{OtelError: &otel.OtelError{Kind: "budget", Op: "export", Message: "transaction too large"}},
		&otel.OtelError{Kind: "canceled", Op: "export", Cause: context.DeadlineExceeded},
		fmt.Errorf("fdb: %w", errTransient),
	}
	sink := &fakeSink{export: func(_ context.Context, call int, names []string) (otel.ExportStats, error) {
		if call < len(transient) {
			return otel.ExportStats{}, transient[call]
		}
		return okStats(len(names)), nil
	}}
	x := startUnitExporter(t, syncConfig(RetryConfig{Enabled: true, InitialInterval: 10 * time.Millisecond, MaxInterval: 40 * time.Millisecond}), sink)
	if err := x.ConsumeTraces(t.Context(), testRequest("r", 0, 2)); err != nil {
		t.Fatalf("export should succeed after transient failures: %v", err)
	}
	calls := sink.snapshot()
	if len(calls) != 5 {
		t.Fatalf("calls = %d, want 5", len(calls))
	}
	// Backoff doubles from initial_interval and is capped by max_interval.
	for i, want := range []time.Duration{10, 20, 40, 40} {
		want *= time.Millisecond
		if gap := calls[i+1].at.Sub(calls[i].at); gap < want {
			t.Fatalf("retry %d after %v, want >= %v", i+1, gap, want)
		}
	}
	for _, c := range calls {
		if !slices.Equal(c.names, rangeNames("r", 0, 2)) {
			t.Fatalf("retry exported %v", c.names)
		}
	}
	if st := x.Stats(); st.Exports != 5 || st.Retries != 4 || st.ExportedSpans != 2 || st.FailedSpans != 0 || st.Flushes != 0 || st.AcceptedRequests != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestRetryStopsImmediatelyOnPermanentErrors(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"config", "admission", "ownership", "corruption", "operation"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			perm := &otel.OtelError{Kind: kind, Op: "export", Message: "permanent " + kind}
			sink := &fakeSink{export: func(context.Context, int, []string) (otel.ExportStats, error) { return otel.ExportStats{}, perm }}
			x := startUnitExporter(t, syncConfig(RetryConfig{Enabled: true, InitialInterval: time.Millisecond, MaxInterval: time.Millisecond}), sink)
			err := x.ConsumeTraces(t.Context(), testRequest("p", 0, 3))
			if !errors.Is(err, perm) {
				t.Fatalf("err = %v, want %v", err, perm)
			}
			if n := sink.callCount(); n != 1 {
				t.Fatalf("permanent error retried: %d calls", n)
			}
			if st := x.Stats(); st.Retries != 0 || st.Exports != 1 || st.FailedSpans != 3 || st.ExportedSpans != 0 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

func TestRetryBounds(t *testing.T) {
	t.Parallel()
	alwaysTransient := func(context.Context, int, []string) (otel.ExportStats, error) {
		return otel.ExportStats{}, errTransient
	}

	t.Run("max_elapsed_time", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{export: alwaysTransient}
		x := startUnitExporter(t, syncConfig(RetryConfig{Enabled: true, InitialInterval: 10 * time.Millisecond, MaxInterval: time.Second, MaxElapsedTime: 100 * time.Millisecond}), sink)
		begin := time.Now()
		err := x.ConsumeTraces(t.Context(), testRequest("e", 0, 1))
		requireErrContains(t, err, "retries exhausted")
		if !errors.Is(err, errTransient) {
			t.Fatalf("exhausted error must wrap the last failure: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > 5*time.Second {
			t.Fatalf("retries ran %v past max_elapsed_time", elapsed)
		}
		st := x.Stats()
		if st.Exports < 1 || st.Retries != st.Exports-1 || st.FailedSpans != 1 {
			t.Fatalf("stats = %+v", st)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{export: alwaysTransient}
		x := startUnitExporter(t, syncConfig(RetryConfig{Enabled: false}), sink)
		if err := x.ConsumeTraces(t.Context(), testRequest("d", 0, 1)); !errors.Is(err, errTransient) {
			t.Fatalf("err = %v", err)
		}
		if n := sink.callCount(); n != 1 {
			t.Fatalf("disabled retry made %d calls", n)
		}
	})
	t.Run("per_attempt_timeout_is_retryable", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{export: func(ctx context.Context, call int, names []string) (otel.ExportStats, error) {
			if _, ok := ctx.Deadline(); !ok {
				return otel.ExportStats{}, errors.New("attempt has no deadline")
			}
			if call == 0 {
				<-ctx.Done()
				return otel.ExportStats{}, ctx.Err()
			}
			return okStats(len(names)), nil
		}}
		cfg := syncConfig(RetryConfig{Enabled: true, InitialInterval: time.Millisecond, MaxInterval: time.Millisecond})
		cfg.Timeout = 20 * time.Millisecond
		x := startUnitExporter(t, cfg, sink)
		if err := x.ConsumeTraces(t.Context(), testRequest("t", 0, 1)); err != nil {
			t.Fatal(err)
		}
		if st := x.Stats(); st.Exports != 2 || st.Retries != 1 || st.ExportedSpans != 1 {
			t.Fatalf("stats = %+v", st)
		}
	})
	t.Run("caller_cancellation_stops_retries", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sink := &fakeSink{export: func(_ context.Context, call int, _ []string) (otel.ExportStats, error) {
			if call == 1 {
				cancel()
			}
			return otel.ExportStats{}, errTransient
		}}
		x := startUnitExporter(t, syncConfig(RetryConfig{Enabled: true, InitialInterval: time.Millisecond, MaxInterval: time.Millisecond}), sink)
		if err := x.ConsumeTraces(ctx, testRequest("k", 0, 1)); !errors.Is(err, errTransient) {
			t.Fatalf("err = %v", err)
		}
		if n := sink.callCount(); n != 2 {
			t.Fatalf("calls after cancellation = %d, want 2", n)
		}
	})
	t.Run("queued_transient_then_success", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{export: func(_ context.Context, call int, names []string) (otel.ExportStats, error) {
			if call < 2 {
				return otel.ExportStats{}, errTransient
			}
			return okStats(len(names)), nil
		}}
		x := startUnitExporter(t, unitConfig(nil), sink)
		if err := x.ConsumeTraces(t.Context(), testRequest("g", 0, 3)); err != nil {
			t.Fatal(err)
		}
		if err := flush(t, x); err != nil {
			t.Fatalf("flush after successful retry = %v", err)
		}
		if st := x.Stats(); st.Exports != 3 || st.Retries != 2 || st.ExportedSpans != 3 || st.FailedSpans != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})
}

// A permanent error on a merged batch is re-exported per source request, so
// only the offending request fails; a transient error retries the whole batch.
func TestMergedBatchPermanentErrorIsolatesRequests(t *testing.T) {
	t.Parallel()
	poison := &otel.OtelError{Kind: "admission", Op: "export", Message: "poisoned span"}
	newSink := func() *fakeSink {
		return &fakeSink{export: func(_ context.Context, _ int, names []string) (otel.ExportStats, error) {
			if slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "poison") }) {
				return otel.ExportStats{}, poison
			}
			return okStats(len(names)), nil
		}}
	}
	cfg := unitConfig(func(c *Config) {
		c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 9, MaxSize: 9}
		c.RetryOnFailure = RetryConfig{Enabled: true, InitialInterval: time.Millisecond, MaxInterval: time.Millisecond}
	})

	sink := newSink()
	x := startUnitExporter(t, cfg, sink)
	var wg sync.WaitGroup
	for _, prefix := range []string{"good-a", "poison", "good-c"} {
		wg.Go(func() {
			if err := x.ConsumeTraces(t.Context(), testRequest(prefix, 0, 3)); err != nil {
				t.Errorf("%s: %v", prefix, err)
			}
		})
	}
	wg.Wait()
	err := flush(t, x)
	if !errors.Is(err, poison) || !IsPermanent(err) {
		t.Fatalf("flush = %v, want the poisoned request's permanent error", err)
	}
	requireErrContains(t, err, "3 spans:")
	calls := sink.snapshot()
	if len(calls) != 4 || len(calls[0].names) != 9 {
		t.Fatalf("calls = %+v, want one merged batch of 9 then 3 per-request exports", calls)
	}
	var good []string
	for _, c := range calls[1:] {
		if len(c.names) != 3 {
			t.Fatalf("isolated export size = %d", len(c.names))
		}
		if !strings.HasPrefix(c.names[0], "poison") {
			good = append(good, c.names...)
		}
	}
	slices.Sort(good)
	if want := append(rangeNames("good-a", 0, 3), rangeNames("good-c", 0, 3)...); !slices.Equal(good, want) {
		t.Fatalf("healthy requests exported = %v", good)
	}
	if st := x.Stats(); st.ExportedSpans != 6 || st.FailedSpans != 3 || st.StoredSpans != 6 || st.Retries != 0 || st.QueuedSpans != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if err := flush(t, x); err != nil {
		t.Fatalf("failures must be reported once: %v", err)
	}

	// A single-request batch is not re-exported.
	single := newSink()
	y := startUnitExporter(t, unitConfig(func(c *Config) { c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 3, MaxSize: 3} }), single)
	if err := y.ConsumeTraces(t.Context(), testRequest("poison", 0, 3)); err != nil {
		t.Fatal(err)
	}
	if err := flush(t, y); !errors.Is(err, poison) {
		t.Fatalf("flush = %v", err)
	}
	if n := single.callCount(); n != 1 {
		t.Fatalf("single-request batch exported %d times", n)
	}

	// A transient failure retries the merged batch as a whole.
	merged := &fakeSink{export: func(_ context.Context, call int, names []string) (otel.ExportStats, error) {
		if call == 0 {
			return otel.ExportStats{}, errTransient
		}
		return okStats(len(names)), nil
	}}
	z := startUnitExporter(t, cfg, merged)
	for _, prefix := range []string{"m1-", "m2-", "m3-"} {
		if err := z.ConsumeTraces(t.Context(), testRequest(prefix, 0, 3)); err != nil {
			t.Fatal(err)
		}
	}
	if err := flush(t, z); err != nil {
		t.Fatal(err)
	}
	if calls := merged.snapshot(); len(calls) != 2 || len(calls[0].names) != 9 || !slices.Equal(calls[0].names, calls[1].names) {
		t.Fatalf("transient merged retry calls = %+v", calls)
	}
	if st := z.Stats(); st.Retries != 1 || st.ExportedSpans != 9 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestFlushReportsBoundedFailures(t *testing.T) {
	t.Parallel()
	perm := &otel.OtelError{Kind: "corruption", Op: "export", Message: "always"}
	sink := &fakeSink{export: func(context.Context, int, []string) (otel.ExportStats, error) { return otel.ExportStats{}, perm }}
	x := startUnitExporter(t, unitConfig(func(c *Config) { c.SendingQueue.NumConsumers = 4 }), sink)
	const requests = maxFailures + 6
	for i := range requests {
		if err := x.ConsumeTraces(t.Context(), testRequest("z", i, 1)); err != nil {
			t.Fatal(err)
		}
	}
	err := flush(t, x)
	requireErrContains(t, err, "6 further export failures omitted")
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != maxFailures+1 {
		t.Fatalf("flush error = %T with %d parts", err, len(joined.Unwrap()))
	}
	if st := x.Stats(); st.FailedSpans != requests || st.ExportedSpans != 0 || st.QueuedSpans != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if err := flush(t, x); err != nil {
		t.Fatalf("second flush = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Admission and lifecycle
// ---------------------------------------------------------------------------

func TestConsumeTracesRejectsInvalidIdentitiesSynchronously(t *testing.T) {
	t.Parallel()
	for _, queued := range []bool{true, false} {
		t.Run(fmt.Sprintf("queued=%v", queued), func(t *testing.T) {
			t.Parallel()
			sink := &fakeSink{}
			x := startUnitExporter(t, unitConfig(func(c *Config) { c.SendingQueue.Enabled = queued }), sink)
			for _, tc := range []struct {
				name string
				mut  func(*tracepb.Span)
				want string
			}{
				{"zero trace", func(s *tracepb.Span) { s.TraceId = make([]byte, 16) }, "trace and 8-byte span identifiers"},
				{"short trace", func(s *tracepb.Span) { s.TraceId = s.TraceId[:15] }, "trace and 8-byte span identifiers"},
				{"zero span", func(s *tracepb.Span) { s.SpanId = make([]byte, 8) }, "trace and 8-byte span identifiers"},
				{"long span", func(s *tracepb.Span) { s.SpanId = append(s.SpanId, 1) }, "trace and 8-byte span identifiers"},
				{"start overflow", func(s *tracepb.Span) { s.StartTimeUnixNano = math.MaxInt64 + 1 }, "start_time_unix_nano exceeds int64"},
			} {
				req := testRequest(tc.name, 0, 3)
				spans := req[0].ScopeSpans[0].Spans
				tc.mut(spans[1])
				req[0].ScopeSpans[0].Spans = []*tracepb.Span{spans[0], nil, spans[1], spans[2]}
				err := x.ConsumeTraces(t.Context(), req)
				requireErrContains(t, err, tc.want)
				requireErrContains(t, err, "span 2:")
				if !IsPermanent(err) {
					t.Fatalf("%s: want permanent error, got %v", tc.name, err)
				}
			}
			// Empty requests are accepted without admission.
			for _, empty := range [][]*tracepb.ResourceSpans{nil, {}, {nil}, {{}}, {{ScopeSpans: []*tracepb.ScopeSpans{nil, {Spans: []*tracepb.Span{nil}}}}}} {
				if err := x.ConsumeTraces(t.Context(), empty); err != nil {
					t.Fatalf("empty request = %v", err)
				}
			}
			if err := flush(t, x); err != nil {
				t.Fatal(err)
			}
			if n := sink.callCount(); n != 0 {
				t.Fatalf("rejected requests reached the sink: %d calls", n)
			}
			if st := x.Stats(); st.RejectedSpans != 15 || st.AcceptedRequests != 0 || st.AcceptedSpans != 0 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

func TestTracesExporterLifecycle(t *testing.T) {
	t.Parallel()
	for _, queued := range []bool{true, false} {
		t.Run(fmt.Sprintf("queued=%v", queued), func(t *testing.T) {
			t.Parallel()
			sink := &fakeSink{}
			opens := 0
			var opened otel.ExporterConfig
			cfg := unitConfig(func(c *Config) {
				c.SendingQueue.Enabled = queued
				c.SendingQueue.Batch = BatchConfig{FlushTimeout: time.Hour, MinSize: 100, MaxSize: 1000}
			})
			x, err := NewFactory().CreateTraces(Settings{Open: func(_ context.Context, c otel.ExporterConfig) (Sink, error) {
				opens++
				opened = c
				if opens == 1 {
					return nil, errOpen
				}
				return sink, nil
			}}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if err := x.ConsumeTraces(ctx, testRequest("l", 0, 1)); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("consume before Start = %v", err)
			}
			if err := x.Flush(ctx); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("flush before Start = %v", err)
			}
			if err := x.Start(ctx); !errors.Is(err, errOpen) {
				t.Fatalf("Start with failing open = %v", err)
			}
			if x.Sink() != nil {
				t.Fatal("failed Start must not install a sink")
			}
			if err := x.Start(ctx); err != nil {
				t.Fatalf("Start retry: %v", err)
			}
			if opened != cfg.Exporter {
				t.Fatalf("open received %+v, want %+v", opened, cfg.Exporter)
			}
			if err := x.Start(ctx); err == nil || !strings.Contains(err.Error(), "already started") {
				t.Fatalf("second Start = %v", err)
			}
			if x.Sink() != Sink(sink) {
				t.Fatal("Sink() does not return the opened sink")
			}
			if err := x.ConsumeTraces(ctx, testRequest("l", 1, 3)); err != nil {
				t.Fatal(err)
			}
			if queued && sink.callCount() != 0 {
				t.Fatal("partial batch exported before Shutdown")
			}
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := x.Shutdown(sctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			// Shutdown drains the queued partial batch before closing the sink.
			if got := sink.exportedNames(); !slices.Equal(got, rangeNames("l", 1, 3)) {
				t.Fatalf("drained = %v", got)
			}
			if n := sink.shutdownCount(); n != 1 {
				t.Fatalf("sink shutdowns = %d", n)
			}
			if err := x.Shutdown(sctx); err != nil {
				t.Fatalf("second Shutdown: %v", err)
			}
			if n := sink.shutdownCount(); n != 1 {
				t.Fatalf("sink shut down %d times", n)
			}
			if err := x.ConsumeTraces(ctx, testRequest("l", 4, 2)); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("consume after Shutdown = %v", err)
			}
			if err := x.Flush(ctx); err != nil {
				t.Fatalf("flush after Shutdown = %v", err)
			}
			if st := x.Stats(); st.ExportedSpans != 3 || st.RejectedSpans != 3 || st.AcceptedRequests != 1 || st.QueuedSpans != 0 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}

	t.Run("never_started", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{}
		x := newUnitExporter(t, unitConfig(nil), sink)
		if err := x.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := x.Start(t.Context()); err == nil {
			t.Fatal("Start after Shutdown must fail")
		}
		if err := x.ConsumeTraces(t.Context(), testRequest("n", 0, 1)); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("consume = %v", err)
		}
		if sink.shutdownCount() != 0 {
			t.Fatal("unopened sink was shut down")
		}
	})

	t.Run("sink_shutdown_error", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{shutdownErr: errClose}
		x := newUnitExporter(t, unitConfig(nil), sink)
		if err := x.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := x.Shutdown(t.Context()); !errors.Is(err, errClose) {
			t.Fatalf("Shutdown = %v", err)
		}
	})

	// With max_elapsed_time 0 a transient failure retries until Shutdown's
	// context expires; the in-flight export is then abandoned and still fails.
	t.Run("shutdown_deadline_cancels_retries", func(t *testing.T) {
		t.Parallel()
		sink := &fakeSink{export: func(context.Context, int, []string) (otel.ExportStats, error) {
			return otel.ExportStats{}, errTransient
		}}
		x := newUnitExporter(t, unitConfig(func(c *Config) {
			c.RetryOnFailure = RetryConfig{Enabled: true, InitialInterval: 2 * time.Millisecond, MaxInterval: 5 * time.Millisecond}
		}), sink)
		if err := x.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := x.ConsumeTraces(t.Context(), testRequest("s", 0, 2)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "retries", func() bool { return sink.callCount() >= 3 })
		short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		err := x.Shutdown(short)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown = %v, want deadline", err)
		}
		requireErrContains(t, err, "fdb_otel shutdown")
		waitFor(t, "sink close", func() bool { return sink.shutdownCount() == 1 })
		err = flush(t, x)
		if !errors.Is(err, errTransient) || !errors.Is(err, context.Canceled) {
			t.Fatalf("abandoned export failure = %v", err)
		}
		if st := x.Stats(); st.FailedSpans != 2 || st.QueuedSpans != 0 || st.Retries < 2 {
			t.Fatalf("stats = %+v", st)
		}
	})
}

// ---------------------------------------------------------------------------
// Real FDB: Collector YAML -> TracesExporter -> *otel.Exporter
// ---------------------------------------------------------------------------

const (
	collectorAgents   = 8
	collectorRequests = 5
	collectorSpans    = 6
	collectorBase     = int64(1_700_000_000_000_000_000)
)

const realCollectorYAML = `
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
  batch:
    send_batch_size: 1024
exporters:
  debug:
    verbosity: basic
  fdb_otel:
    cluster_file: ${env:FDB_OTEL_COLLECTOR_CLUSTER_FILE}
    run: ${env:FDB_OTEL_COLLECTOR_RUN}
    tenant: ${env:FDB_OTEL_COLLECTOR_TEST_UNSET_TENANT:-7}
    order: time
    posting_value_format: tuple
    record_compression: brotli-1
    batch_size: 16
    workers: 4
    k: 100
    timeout: 60s
    sending_queue:
      enabled: true
      num_consumers: 4
      queue_size: ${env:FDB_OTEL_COLLECTOR_TEST_UNSET_QUEUE:-2000}
      block_on_overflow: true
      batch:
        flush_timeout: 25ms
        min_size: 16
        max_size: 37
    retry_on_failure:
      enabled: true
      initial_interval: 50ms
      max_interval: 400ms
      max_elapsed_time: 60s
  fdb_otel/secondary:
    cluster_file: ${env:FDB_OTEL_COLLECTOR_CLUSTER_FILE}
    run: ${env:FDB_OTEL_COLLECTOR_RUN}_b
    tenant: 0
    order: trace
    batch_size: 8
    workers: 2
    sending_queue:
      enabled: false
    retry_on_failure:
      enabled: true
      initial_interval: 50ms
      max_interval: 200ms
      max_elapsed_time: 60s
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [fdb_otel, fdb_otel/secondary, debug]
`

func freshCollectorRun(t *testing.T) string {
	t.Helper()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return "coll_" + hex.EncodeToString(nonce[:])
}

func agentTraceID(agent, req int) []byte {
	return []byte{0xc0, byte(agent), byte(req), 15: 0x01}
}

func agentSpanID(agent, req, i int) []byte {
	return []byte{0x5a, byte(agent), byte(req), byte(i), 7: 0x01}
}

func agentStart(agent, req, i int) int64 {
	return collectorBase + int64(agent)*1e9 + int64(req)*1e7 + int64(i)*1e3
}

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func boolKV(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}

func doubleKV(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

// agentRequest is one upstream export: a trace of six spans from one service
// across two instrumentation scopes, with non-canonical attribute types
// (bool, double, duplicate keys) and wrapper unknown fields.
func agentRequest(agent, req int) []*tracepb.ResourceSpans {
	rs := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			strKV("service.name", fmt.Sprintf("svc-%d", agent)),
			strKV("service.instance.id", fmt.Sprintf("agent-%d", agent)),
			intKV("host.cores", int64(agent+1)),
			boolKV("deployment.canary", agent%2 == 0),
		}},
		SchemaUrl: "https://opentelemetry.io/schemas/1.26.0",
	}
	rs.ProtoReflect().SetUnknown(unknownField(1001, uint64(agent+1)))
	for i, tier := range []string{"http", "db"} {
		ss := &tracepb.ScopeSpans{
			Scope:     &commonpb.InstrumentationScope{Name: "io.example." + tier, Version: "1.2.3", Attributes: []*commonpb.KeyValue{strKV("library.tier", tier)}},
			SchemaUrl: "scope-" + tier,
		}
		ss.ProtoReflect().SetUnknown(unknownField(1002, uint64(i+1)))
		rs.ScopeSpans = append(rs.ScopeSpans, ss)
	}
	for i := range collectorSpans {
		start := agentStart(agent, req, i)
		sp := &tracepb.Span{
			TraceId:           agentTraceID(agent, req),
			SpanId:            agentSpanID(agent, req, i),
			Name:              fmt.Sprintf("op-%d", i),
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(start),
			EndTimeUnixNano:   uint64(start + 500),
			Attributes: []*commonpb.KeyValue{
				strKV("http.route", fmt.Sprintf("/api/v1/r%d", req)),
				intKV("request.index", int64(req)),
				boolKV("error", req%2 == 1 && i == collectorSpans-1),
				doubleKV("latency.ms", float64(i)+0.25),
				strKV("dup", "first"),
				strKV("dup", "second"),
			},
			Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		}
		if i > 0 {
			sp.ParentSpanId = agentSpanID(agent, req, 0)
			sp.Kind = tracepb.Span_SPAN_KIND_CLIENT
		} else {
			sp.Events = []*tracepb.Span_Event{{Name: "request.received", TimeUnixNano: uint64(start + 10), Attributes: []*commonpb.KeyValue{intKV("bytes", 512)}}}
		}
		scope := 0
		if i >= 4 {
			scope = 1
		}
		rs.ScopeSpans[scope].Spans = append(rs.ScopeSpans[scope].Spans, sp)
	}
	return []*tracepb.ResourceSpans{rs}
}

type modelSpan struct {
	trace, id string
	start     int64
	attrs     map[otel.Attribute]bool
	stored    *tracepb.ResourceSpans // the LoadSpanOTLP answer
}

func canonicalSet(scope string, kvs []*commonpb.KeyValue) map[string]otel.Attribute {
	out := map[string]otel.Attribute{}
	for _, kv := range kvs {
		out[kv.Key] = otel.CanonicalAttribute(scope, kv.Key, kv.Value) // last value wins
	}
	return out
}

func collectorModel() []modelSpan {
	var out []modelSpan
	for agent := range collectorAgents {
		for req := range collectorRequests {
			for _, rs := range agentRequest(agent, req) {
				res := canonicalSet("resource", rs.Resource.Attributes)
				for _, ss := range rs.ScopeSpans {
					scope := canonicalSet("scope", ss.Scope.Attributes)
					for _, sp := range ss.Spans {
						attrs := map[otel.Attribute]bool{}
						for _, set := range []map[string]otel.Attribute{res, scope, canonicalSet("span", sp.Attributes)} {
							for _, a := range set {
								attrs[a] = true
							}
						}
						stored := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{sp}}}}
						stored.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
						stored.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
						out = append(out, modelSpan{trace: hex.EncodeToString(sp.TraceId), id: hex.EncodeToString(sp.SpanId), start: int64(sp.StartTimeUnixNano), attrs: attrs, stored: stored})
					}
				}
			}
		}
	}
	return out
}

// modelHits mirrors the exporter answer: one hit per trace at its latest
// matching start, newest first, at most k.
func modelHits(model []modelSpan, k int, match func(modelSpan) bool) []otel.Hit {
	latest := map[string]int64{}
	for _, s := range model {
		if match(s) {
			if prev, ok := latest[s.trace]; !ok || s.start > prev {
				latest[s.trace] = s.start
			}
		}
	}
	out := []otel.Hit{}
	for tr, at := range latest {
		out = append(out, otel.Hit{Trace: tr, Latest: at})
	}
	slices.SortFunc(out, func(a, b otel.Hit) int {
		if a.Latest != b.Latest {
			if a.Latest > b.Latest {
				return -1
			}
			return 1
		}
		return -strings.Compare(a.Trace, b.Trace)
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func verifyCollectorStore(t *testing.T, ctx context.Context, name string, exp *otel.Exporter, model []modelSpan) {
	t.Helper()
	k := exp.Config().K
	anyBool := func(b bool) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
	}
	queries := []otel.Attribute{
		otel.CanonicalAttribute("resource", "deployment.canary", anyBool(true)),
		otel.CanonicalAttribute("resource", "deployment.canary", anyBool(false)),
		{Scope: "resource", Key: "host.cores", Type: "int", Value: "4"},
		{Scope: "resource", Key: "service.name", Type: "string", Value: "svc-99"},
		{Scope: "scope", Key: "library.tier", Type: "string", Value: "db"},
		{Scope: "scope", Key: "library.tier", Type: "string", Value: "http"},
		{Scope: "span", Key: "request.index", Type: "int", Value: "3"},
		{Scope: "span", Key: "request.index", Type: "string", Value: "3"},
		{Scope: "span", Key: "http.route", Type: "string", Value: "/api/v1/r4"},
		otel.CanonicalAttribute("span", "error", anyBool(true)),
		otel.CanonicalAttribute("span", "latency.ms", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 5.25}}),
		{Scope: "span", Key: "dup", Type: "string", Value: "second"},
		{Scope: "span", Key: "dup", Type: "string", Value: "first"},
		{Scope: "resource", Key: "dup", Type: "string", Value: "second"},
	}
	for agent := range collectorAgents {
		queries = append(queries, otel.Attribute{Scope: "resource", Key: "service.name", Type: "string", Value: fmt.Sprintf("svc-%d", agent)})
	}
	nonEmpty := 0
	for _, a := range queries {
		got, err := exp.Query(ctx, a)
		if err != nil {
			t.Fatalf("%s: Query(%+v): %v", name, a, err)
		}
		want := modelHits(model, k, func(s modelSpan) bool { return s.attrs[a] })
		if !slices.Equal(got.Hits, want) {
			t.Fatalf("%s: Query(%+v)\n got %v\nwant %v", name, a, got.Hits, want)
		}
		if len(want) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty < len(queries)-4 {
		t.Fatalf("%s: only %d of %d queries had answers; the model is too sparse", name, nonEmpty, len(queries))
	}

	traces := map[string][]otel.SpanHit{}
	for _, s := range model {
		traces[s.trace] = append(traces[s.trace], otel.SpanHit{ID: s.id, Start: s.start})
	}
	for trace, want := range traces {
		slices.SortFunc(want, func(a, b otel.SpanHit) int { return int(a.Start - b.Start) })
		got, err := exp.Trace(ctx, trace)
		if err != nil {
			t.Fatalf("%s: Trace(%s): %v", name, trace, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: Trace(%s)\n got %v\nwant %v", name, trace, got, want)
		}
	}
	if got, err := exp.Trace(ctx, hex.EncodeToString(agentTraceID(99, 0))); err != nil || len(got) != 0 {
		t.Fatalf("%s: Trace(unknown) = %v, %v", name, got, err)
	}

	for _, s := range model {
		got, err := exp.Span(ctx, s.id)
		if err != nil {
			t.Fatalf("%s: Span(%s): %v", name, s.id, err)
		}
		if want := []otel.SpanMatch{{Trace: s.trace, ID: s.id, Start: s.start}}; !slices.Equal(got, want) {
			t.Fatalf("%s: Span(%s) = %v, want %v", name, s.id, got, want)
		}
		rs, err := exp.LoadSpanOTLP(ctx, s.trace, s.id)
		if err != nil {
			t.Fatalf("%s: LoadSpanOTLP(%s, %s): %v", name, s.trace, s.id, err)
		}
		if !proto.Equal(rs, s.stored) {
			t.Fatalf("%s: LoadSpanOTLP(%s, %s) lost fidelity\n got %v\nwant %v", name, s.trace, s.id, rs, s.stored)
		}
	}
	missingTrace, missingSpan := hex.EncodeToString(agentTraceID(0, 0)), hex.EncodeToString(agentSpanID(0, 0, 99))
	var notFound *otel.SpanNotFoundError
	if _, err := exp.LoadSpanOTLP(ctx, missingTrace, missingSpan); !errors.As(err, &notFound) || notFound.Trace != missingTrace || notFound.Span != missingSpan {
		t.Fatalf("%s: LoadSpanOTLP(missing) = %T %v", name, err, err)
	}
	if _, err := exp.LoadSpanOTLP(ctx, "XYZ", missingSpan); err == nil {
		t.Fatalf("%s: LoadSpanOTLP accepted a malformed trace id", name)
	}
	if got, err := exp.Span(ctx, missingSpan); err != nil || len(got) != 0 {
		t.Fatalf("%s: Span(missing) = %v, %v", name, got, err)
	}

	windows := []otel.TimeRange{
		{Start: collectorBase - 1e9, End: collectorBase},
		{Start: model[0].start, End: model[0].start},
		{Start: model[7].start, End: model[7].start + 1},
		{Start: collectorBase, End: collectorBase + collectorAgents*1e9},
	}
	for agent := range collectorAgents {
		lo := collectorBase + int64(agent)*1e9
		windows = append(windows, otel.TimeRange{Start: lo, End: lo + 1e9}, otel.TimeRange{Start: lo + 1e7 + 2e3, End: lo + 3e7 + 3e3})
	}
	for _, w := range windows {
		got, err := exp.Time(ctx, w)
		if err != nil {
			t.Fatalf("%s: Time(%+v): %v", name, w, err)
		}
		want := modelHits(model, k, func(s modelSpan) bool { return s.start >= w.Start && s.start < w.End })
		if !slices.Equal(got.Hits, want) {
			t.Fatalf("%s: Time(%+v)\n got %v\nwant %v", name, w, got.Hits, want)
		}
	}
	if _, err := exp.Time(ctx, otel.TimeRange{Start: 2, End: 1}); err == nil {
		t.Fatalf("%s: Time accepted an inverted range", name)
	}
}

// TestCollectorTracesExporterRealFDB runs the Collector exporter end to end:
// Collector YAML (with ${env:...} references) -> factory -> Start -> eight
// concurrent upstream agents fanned out to two fdb_otel exporters (one queued,
// one synchronous) -> Flush -> query verification through *otel.Exporter ->
// Shutdown. Not parallel: it uses t.Setenv.
func TestCollectorTracesExporterRealFDB(t *testing.T) {
	for _, unset := range []string{"FDB_OTEL_COLLECTOR_TEST_UNSET_TENANT", "FDB_OTEL_COLLECTOR_TEST_UNSET_QUEUE"} {
		if _, ok := os.LookupEnv(unset); ok {
			t.Fatalf("%s must be unset", unset)
		}
	}
	setup, cancelSetup := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancelSetup()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cluster := filepath.Join(dir, "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	run := freshCollectorRun(t)
	t.Setenv("FDB_OTEL_COLLECTOR_CLUSTER_FILE", cluster)
	t.Setenv("FDB_OTEL_COLLECTOR_RUN", run)
	configPath := filepath.Join(dir, "otelcol.yaml")
	if err := os.WriteFile(configPath, []byte(realCollectorYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cc, err := LoadPipelineConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := cc.TracesExporters()
	primaryID, secondaryID := ComponentID{Type: ComponentType}, ComponentID{Type: ComponentType, Name: "secondary"}
	if !slices.Equal(ids, []ComponentID{primaryID, secondaryID}) {
		t.Fatalf("TracesExporters() = %v", ids)
	}
	pcfg, scfg := cc.FDBExporters[primaryID], cc.FDBExporters[secondaryID]
	if pcfg.Exporter.ClusterFile != cluster || pcfg.Exporter.Run != run || pcfg.Exporter.Tenant != 7 || pcfg.SendingQueue.QueueSize != 2000 ||
		pcfg.SendingQueue.Batch != (BatchConfig{FlushTimeout: 25 * time.Millisecond, MinSize: 16, MaxSize: 37}) || !pcfg.SendingQueue.BlockOnOverflow {
		t.Fatalf("primary config = %+v", *pcfg)
	}
	if scfg.Exporter.Run != run+"_b" || scfg.Exporter.Tenant != 0 || scfg.Exporter.Order != "trace" || scfg.SendingQueue.Enabled {
		t.Fatalf("secondary config = %+v", *scfg)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	factory := NewFactory()
	var exporters []*TracesExporter
	var stores []*otel.Exporter
	for _, id := range ids {
		x, err := factory.CreateTraces(Settings{ID: id}, cc.FDBExporters[id])
		if err != nil {
			t.Fatal(err)
		}
		if x.Sink() != nil {
			t.Fatal("sink opened before Start")
		}
		if err := x.Start(ctx); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
		t.Cleanup(func() {
			sctx, scancel := context.WithTimeout(context.Background(), time.Minute)
			defer scancel()
			_ = x.Shutdown(sctx)
		})
		exp, ok := x.Sink().(*otel.Exporter)
		if !ok {
			t.Fatalf("%s sink = %T, want *otel.Exporter", id, x.Sink())
		}
		if exp.Config() != cc.FDBExporters[id].Exporter {
			t.Fatalf("%s opened with %+v", id, exp.Config())
		}
		exporters = append(exporters, x)
		stores = append(stores, exp)
	}
	primary, secondary := exporters[0], exporters[1]

	// The traces pipeline fans every request out to each exporter; the
	// exporters do not mutate data, so the request is shared.
	fanout := func(rs []*tracepb.ResourceSpans) error {
		var errs []error
		for _, x := range exporters {
			if x.Capabilities().MutatesData {
				return errors.New("fan-out requires non-mutating exporters")
			}
			errs = append(errs, x.ConsumeTraces(ctx, rs))
		}
		return errors.Join(errs...)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for agent := range collectorAgents {
		wg.Go(func() {
			<-start
			for req := range collectorRequests {
				if err := fanout(agentRequest(agent, req)); err != nil {
					t.Errorf("agent %d request %d: %v", agent, req, err)
					return
				}
				switch {
				case agent == 0 && req == 2:
					// A mid-stream Flush makes everything this agent sent durable
					// while the other agents keep sending.
					if err := primary.Flush(ctx); err != nil {
						t.Errorf("mid-stream flush: %v", err)
						return
					}
					for r := 0; r <= req; r++ {
						hits, err := stores[0].Trace(ctx, hex.EncodeToString(agentTraceID(0, r)))
						if err != nil || len(hits) != collectorSpans {
							t.Errorf("after flush, trace %d = %d spans, %v", r, len(hits), err)
						}
					}
				case agent == 3 && req == 1:
					bad := agentRequest(3, 99)
					bad[0].ScopeSpans[1].Spans[0].SpanId = make([]byte, 8)
					if err := fanout(bad); !IsPermanent(err) {
						t.Errorf("invalid upstream request = %v, want permanent rejection", err)
					}
				}
			}
			if agent == 0 {
				// An agent retry resends an already exported request.
				if err := fanout(agentRequest(0, 0)); err != nil {
					t.Errorf("resend: %v", err)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	for _, x := range exporters {
		if err := x.Flush(ctx); err != nil {
			t.Fatalf("Flush %s: %v", x.ID(), err)
		}
	}

	const total = collectorAgents*collectorRequests*collectorSpans + collectorSpans
	ps, ss := primary.Stats(), secondary.Stats()
	if ps.AcceptedRequests != collectorAgents*collectorRequests+1 || ps.AcceptedSpans != total || ps.RejectedSpans != collectorSpans ||
		ps.ExportedSpans != total || ps.FailedSpans != 0 || ps.QueuedSpans != 0 || ps.StoredSpans+ps.DuplicateSpans != total ||
		ps.Flushes < (total+36)/37 || ps.Exports < ps.Flushes {
		t.Fatalf("primary stats = %+v", ps)
	}
	if ss.AcceptedRequests != collectorAgents*collectorRequests+1 || ss.ExportedSpans != total || ss.StoredSpans != total || ss.DuplicateSpans != 0 ||
		ss.RejectedSpans != collectorSpans || ss.FailedSpans != 0 || ss.Flushes != 0 {
		t.Fatalf("secondary stats = %+v", ss)
	}

	model := collectorModel()
	if len(model) != collectorAgents*collectorRequests*collectorSpans {
		t.Fatalf("model has %d spans", len(model))
	}
	for i, exp := range stores {
		verifyCollectorStore(t, ctx, ids[i].String(), exp, model)
	}
	// The rejected request wrote nothing.
	for _, exp := range stores {
		if hits, err := exp.Trace(ctx, hex.EncodeToString(agentTraceID(3, 99))); err != nil || len(hits) != 0 {
			t.Fatalf("rejected trace stored: %v, %v", hits, err)
		}
	}

	for _, x := range exporters {
		sctx, scancel := context.WithTimeout(ctx, time.Minute)
		err := x.Shutdown(sctx)
		scancel()
		if err != nil {
			t.Fatalf("Shutdown %s: %v", x.ID(), err)
		}
		if err := x.ConsumeTraces(ctx, agentRequest(0, 0)); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("%s after Shutdown = %v", x.ID(), err)
		}
	}
	for _, exp := range stores {
		_, err := exp.Query(ctx, otel.Attribute{Scope: "resource", Key: "service.name", Type: "string", Value: "svc-0"})
		if err == nil || !IsPermanent(err) {
			t.Fatalf("query on a shut-down exporter = %v, want permanent operation error", err)
		}
	}
}
