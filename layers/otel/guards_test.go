package otel

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestEveryConfigBound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		set  func(*Config)
	}{
		{"count low", func(c *Config) { c.Count = 0 }},
		{"count high", func(c *Config) { c.Count = 100000001 }},
		{"tenants low", func(c *Config) { c.Tenants = 0 }},
		{"tenants high", func(c *Config) { c.Tenants = (c.Count+1)/2 + 1 }},
		{"attributes low", func(c *Config) { c.Attributes = 3 }},
		{"attributes high", func(c *Config) { c.Attributes = 129 }},
		{"selectivity", func(c *Config) { c.MatchEvery = 0 }},
		{"batch low", func(c *Config) { c.Batch = 0 }},
		{"batch high", func(c *Config) { c.Batch = 65 }},
		{"save mode", func(c *Config) { c.SaveMode = "unknown" }},
		{"empty save mode", func(c *Config) { c.SaveMode = "" }},
		{"workers low", func(c *Config) { c.Workers = 0 }},
		{"workers high", func(c *Config) { c.Workers = 9 }},
		{"CH batch low", func(c *Config) { c.CHBatch = 0 }},
		{"CH batch high", func(c *Config) { c.CHBatch = 65537 }},
		{"CH bytes low", func(c *Config) { c.CHBatchBytes = 0 }},
		{"CH bytes high", func(c *Config) { c.CHBatchBytes = 64<<20 + 1 }},
		{"tenant low", func(c *Config) { c.Tenant = -1 }},
		{"tenant high", func(c *Config) { c.Tenant = c.Tenants }},
		{"K low", func(c *Config) { c.K = 0 }},
		{"K high", func(c *Config) { c.K = 10001 }},
		{"iterations low", func(c *Config) { c.Iterations = 0 }},
		{"iterations high", func(c *Config) { c.Iterations = 100001 }},
		{"candidate low", func(c *Config) { c.MaxCandidates = 0 }},
		{"candidate high", func(c *Config) { c.MaxCandidates = 100000001 }},
		{"bytes low", func(c *Config) { c.MaxBytes = 0 }},
		{"hash low", func(c *Config) { c.HashBytes = 0 }},
		{"hash high", func(c *Config) { c.HashBytes = 33 }},
		{"order", func(c *Config) { c.Order = "unordered" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.set(&c)
			var got *ConfigError
			if err := c.Validate(); !errors.As(err, &got) {
				t.Fatalf("expected ConfigError, got %v", err)
			}
		})
	}
	for _, run := range []string{strings.Repeat("a", 8), strings.Repeat("b", 48)} {
		c := testConfig()
		c.Run = run
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{strings.Repeat("a", 7), strings.Repeat("a", 49)} {
		c := testConfig()
		c.Run = run
		if c.Validate() == nil {
			t.Fatal("namespace boundary accepted")
		}
	}
	low := testConfig()
	low.Count = 1
	low.Tenants = 1
	low.Attributes = 4
	low.MatchEvery = 1
	low.Batch = 1
	low.Workers = 1
	low.CHBatch = 1
	low.CHBatchBytes = 1
	low.Tenant = 0
	low.K = 1
	low.Iterations = 1
	low.MaxCandidates = 1
	low.MaxBytes = 1
	low.HashBytes = 1
	if err := low.Validate(); err != nil {
		t.Fatal(err)
	}
	high := testConfig()
	high.Count = 100000000
	high.Tenants = 50000000
	high.Attributes = 128
	high.Batch = 64
	high.Workers = 8
	high.CHBatch = 65536
	high.CHBatchBytes = 64 << 20
	high.K = 10000
	high.Iterations = 100000
	high.HashBytes = 32
	high.Order = "time"
	if err := high.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveModeIsExecutionOnly(t *testing.T) {
	t.Parallel()
	c := testConfig()
	baseline := manifest(c, "ready")
	for _, mode := range []string{"serial", "pipelined", "batch"} {
		for _, workers := range []int{1, 8} {
			for _, diagnostics := range []bool{false, true} {
				changed := c
				changed.SaveMode = mode
				changed.Workers = workers
				changed.Diagnostics = diagnostics
				if err := changed.Validate(); err != nil {
					t.Fatalf("mode=%s workers=%d diagnostics=%t: %v", mode, workers, diagnostics, err)
				}
				if corpusIdentity(changed) != corpusIdentity(c) {
					t.Fatalf("execution settings changed source corpus: mode=%s workers=%d diagnostics=%t", mode, workers, diagnostics)
				}
				if err := checkManifest(changed, baseline, true); err != nil {
					t.Fatalf("execution settings changed stored ownership: mode=%s workers=%d diagnostics=%t: %v", mode, workers, diagnostics, err)
				}
			}
		}
	}
	defaults := DefaultConfig()
	if defaults.SaveMode != "pipelined" || defaults.Workers != 1 || defaults.Diagnostics {
		t.Fatalf("unexpected ingestion defaults: mode=%s workers=%d diagnostics=%t", defaults.SaveMode, defaults.Workers, defaults.Diagnostics)
	}
}

func TestManifestEveryAxis(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for _, tc := range []struct {
		name string
		set  func(*Manifest)
	}{{"tool", func(m *Manifest) { m.Tool += "x" }}, {"run", func(m *Manifest) { m.Run += "x" }}, {"corpus", func(m *Manifest) { m.Corpus += "x" }}, {"order", func(m *Manifest) { m.Order = "time" }}, {"hash", func(m *Manifest) { m.HashBytes = 1 }}, {"backend", func(m *Manifest) { m.Backend = "ch-attrs" }}, {"state", func(m *Manifest) { m.State = "loading" }}} {
		t.Run(tc.name, func(t *testing.T) {
			m := manifest(c, "ready")
			tc.set(&m)
			var got *OwnershipError
			if err := checkManifest(c, m, true); !errors.As(err, &got) {
				t.Fatalf("missing manifest axis: %v", err)
			}
		})
	}
	m := manifest(c, "loading")
	if err := checkManifest(c, m, false); err != nil {
		t.Fatal("owned partial run cannot be dropped", err)
	}
}

func TestGenerationKnobsAndLiteralIdentity(t *testing.T) {
	t.Parallel()
	c := testConfig()
	s := Generate(c, 0)
	if ToolVersion != "otelbench-v2" {
		t.Fatalf("unexpected fixture version: %s", ToolVersion)
	}
	wantTrace := fmt.Sprintf("%016x%016x", referenceFixtureWord(283, "trace-a-v2", 0), referenceFixtureWord(283, "trace-b-v2", 0))
	wantID := fmt.Sprintf("%016x", referenceFixtureWord(283, "span-v2", 0))
	if s.Trace != wantTrace || s.ID != wantID {
		t.Fatalf("seeded V2 fixture identity: got %s %s want %s %s", s.Trace, s.ID, wantTrace, wantID)
	}
	if s.Ordinal != 0 || s.Tenant != 0 || len(s.Attributes) != 8 {
		t.Fatal("fixture knobs ignored")
	}
	c.Seed++
	if Generate(c, 0).Trace == s.Trace || Generate(c, 0).ID == s.ID {
		t.Fatal("seed ignored")
	}
	for _, n := range []int{4, 5, 128} {
		c.Attributes = n
		got := Generate(c, 17)
		if len(got.Attributes) != n {
			t.Fatal("attribute count ignored")
		}
	}
	if Generate(testConfig(), 0).Attributes[0].Value != QueryValue || Generate(testConfig(), 4).Attributes[0].Value == QueryValue {
		t.Fatal("selectivity schedule changed")
	}
}

// The identity specification uses a SHA-256 keyed permutation, not truncated
// randomWord hashes. Keep this calculation independent of both ID helpers.
func referenceFixtureWord(seed uint64, domain string, ordinal int) uint64 {
	material := binary.BigEndian.AppendUint64([]byte(domain), seed)
	material = binary.BigEndian.AppendUint64(material, 0)
	key := sha256.Sum256(material)
	permutation := func(value uint64) uint64 {
		value ^= binary.BigEndian.Uint64(key[:8])
		value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
		value = (value ^ (value >> 27)) * 0x94d049bb133111eb
		return value ^ (value >> 31)
	}
	value := permutation(uint64(ordinal) + 1)
	if value == 0 {
		value = permutation(0)
	}
	return value
}

func TestV2IdentityAndAdjacentTraceContract(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{0, 283, math.MaxUint64} {
		c := testConfig()
		c.Seed = seed
		ids := map[string]bool{}
		traces := map[string]int{}
		for _, ordinal := range []int{0, 1, 2, 3, 96, 97, 98, 99, 99999998, 99999999} {
			s := Generate(c, ordinal)
			wantTrace := fmt.Sprintf("%016x%016x", referenceFixtureWord(seed, "trace-a-v2", ordinal/2), referenceFixtureWord(seed, "trace-b-v2", ordinal/2))
			wantID := fmt.Sprintf("%016x", referenceFixtureWord(seed, "span-v2", ordinal))
			if s.Trace != wantTrace || s.ID != wantID || s.ID == strings.Repeat("0", 16) || s.Trace == strings.Repeat("0", 32) || ids[s.ID] {
				t.Fatalf("seed=%d ordinal=%d: invalid or repeated identity: %+v", seed, ordinal, s)
			}
			if prev, ok := traces[s.Trace]; ok && prev/2 != ordinal/2 {
				t.Fatalf("seed=%d: distinct trace ordinals %d and %d collided", seed, prev/2, ordinal/2)
			}
			if paired := Generate(c, ordinal^1); paired.Trace != s.Trace || paired.ID == s.ID || paired.Tenant != s.Tenant || s.Tenant != (ordinal/2)%c.Tenants {
				t.Fatalf("seed=%d ordinal=%d: adjacent trace contract failed", seed, ordinal)
			}
			ids[s.ID], traces[s.Trace] = true, ordinal
		}
	}
}

func TestV2GenAIAttributeSlots(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for _, n := range []int{4, 5, 7, 8, 9, 128} {
		c.Attributes = n
		s := Generate(c, 17)
		if len(s.Attributes) != n {
			t.Fatalf("attributes=%d got %d", n, len(s.Attributes))
		}
		if n >= 8 {
			want := []Attribute{
				{"span", "gen_ai.operation.name", "string", "chat"},
				{"span", "gen_ai.provider.name", "string", "openai"},
				{"span", "gen_ai.request.model", "string", "gpt-4"},
				{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + s.ID},
			}
			if !sameJSON(s.Attributes[4:8], want) {
				t.Fatalf("attributes=%d: GenAI slots=%+v want %+v", n, s.Attributes[4:8], want)
			}
		} else {
			for j := 4; j < n; j++ {
				want := Attribute{"span", fmt.Sprintf("custom.literal.%d", j), "string", fmt.Sprintf("v_%d_17", j)}
				if s.Attributes[j] != want {
					t.Fatalf("attributes=%d slot=%d: partial GenAI replacement: %+v", n, j, s.Attributes[j])
				}
			}
		}
		if n > 8 && s.Attributes[8] != (Attribute{"span", "custom.literal.8", "string", "v_8_17"}) {
			t.Fatal("GenAI replacement changed later filler slots")
		}
	}
}

func TestFactoryOptionGuards(t *testing.T) {
	t.Parallel()
	c := testConfig()
	_, index, err := metadata(c)
	if err != nil {
		t.Fatal(err)
	}
	factory := postingFactory{}
	types := factory.IndexTypes()
	if len(types) != 1 || types[0] != indexType {
		t.Fatal("registration types wrong")
	}
	for _, width := range []string{"", "no", "-1", "0", "33"} {
		copy := *index
		copy.Options = map[string]string{"order": "trace", "hash_bytes": width}
		var got *ConfigError
		if err := factory.ValidateIndexOptions(&copy); !errors.As(err, &got) {
			t.Fatalf("hash width %q accepted: %v", width, err)
		}
	}
	for _, width := range []string{"1", "32"} {
		copy := *index
		copy.Options = map[string]string{"order": "time", "hash_bytes": width}
		if err := factory.ValidateIndexOptions(&copy); err != nil {
			t.Fatal(err)
		}
	}
	if factory.ValidateChangedOptions(index, index, nil) != nil || factory.ValidateChangedOptions(index, index, map[string]bool{}) != nil {
		t.Fatal("empty option changes rejected")
	}
	var got *ConfigError
	if err := factory.ValidateChangedOptions(index, index, map[string]bool{"order": true}); !errors.As(err, &got) {
		t.Fatalf("immutable option change accepted: %v", err)
	}
}

func TestOpenGuardsAndCancellation(t *testing.T) {
	t.Parallel()
	c := testConfig()
	var config *ConfigError
	if _, err := OpenFDBContext(t.Context(), c, ""); !errors.As(err, &config) {
		t.Fatal("empty cluster file accepted", err)
	}
	for _, endpoint := range []string{"", "ftp://example.test", "http://", "http://127.0.0.1:8123?query=DROP", "http://127.0.0.1:8123#fragment", "http://%zz"} {
		if _, err := OpenCHContext(t.Context(), c, endpoint, false); !errors.As(err, &config) {
			t.Fatalf("invalid endpoint %q: %v", endpoint, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := OpenFDBContext(ctx, c, "not-a-cluster-file"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FDB bootstrap ignored context: %v", err)
	}
	if _, err := OpenCHContext(ctx, c, "http://127.0.0.1:8123", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("CH setup ignored context: %v", err)
	}
	path := filepath.Join(t.TempDir(), "empty.cluster")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFDBContext(t.Context(), c, path); !errors.As(err, &config) {
		t.Fatalf("empty cluster contents accepted: %v", err)
	}
}

func TestTypedErrorClassification(t *testing.T) {
	t.Parallel()
	cause := errors.New("transport cause")
	for _, kind := range []string{"config", "ownership", "admission", "budget", "corruption", "http", "audit", "oracle", "canceled"} {
		err := wrap(kind, "operation", cause)
		var generic *OtelError
		if !errors.As(err, &generic) || generic.Kind != kind || generic.Op != "operation" || !errors.Is(err, cause) {
			t.Fatalf("typed cause/context lost: %v", err)
		}
		var matched bool
		switch kind {
		case "config":
			var v *ConfigError
			matched = errors.As(err, &v)
		case "ownership":
			var v *OwnershipError
			matched = errors.As(err, &v)
		case "admission":
			var v *AdmissionError
			matched = errors.As(err, &v)
		case "budget":
			var v *QueryBudgetError
			matched = errors.As(err, &v)
		case "corruption":
			var v *CorruptionError
			matched = errors.As(err, &v)
		case "http":
			var v *HTTPError
			matched = errors.As(err, &v)
		case "audit":
			var v *AuditError
			matched = errors.As(err, &v)
		case "oracle":
			var v *OracleError
			matched = errors.As(err, &v)
		default:
			var v *OperationError
			matched = errors.As(err, &v)
		}
		if !matched || err.Error() == "" {
			t.Fatalf("specific classification failed: %v", err)
		}
	}
	if wrap("http", "nil", nil) != nil {
		t.Fatal("nil cause became failure")
	}
	if fail("config", "x", "literal").Error() == "" || failf("config", "x", "n=%d", 3).Error() == "" {
		t.Fatal("error formatting empty")
	}
}

func TestOracleLiteralTraceOrder(t *testing.T) {
	t.Parallel()
	input := []SpanHit{{ID: "c", Start: 8}, {ID: "z", Start: 4}, {ID: "a", Start: 8}, {ID: "b", Start: 8}}
	oracleTraceSort(input)
	want := []SpanHit{{ID: "z", Start: 4}, {ID: "a", Start: 8}, {ID: "b", Start: 8}, {ID: "c", Start: 8}}
	if !sameJSON(input, want) {
		t.Fatal("independent trace order wrong", input)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ExpectedContext(ctx, testConfig(), "span", QueryKey, "string", QueryValue); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := ExpectedTraceContext(ctx, testConfig(), "trace"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := oracleTopKContext(ctx, map[string]int64{"a": 1}, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := oracleTopKContext(t.Context(), nil, 0); err == nil {
		t.Fatal("invalid oracle K accepted")
	}
	m := map[string]int64{}
	for _, id := range []string{"a", "b", "c", "d"} {
		m[id] = math.MaxInt64
	}
	hits, err := oracleTopKContext(t.Context(), m, 2)
	if err != nil || !sameJSON(hits, []Hit{{"d", math.MaxInt64}, {"c", math.MaxInt64}}) || cap(hits) != 2 {
		t.Fatalf("bounded oracle tie order/storage: %v %+v", err, hits)
	}
}

func TestProjectionCoverageBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		key, value int
		reject     bool
	}{{"key boundary", 1024, 1, false}, {"key over", 1025, 1, true}, {"value boundary", 1, 64000, false}, {"value over", 1, 64001, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := Generate(testConfig(), 0)
			s.Attributes = []Attribute{{"span", strings.Repeat("k", tc.key), "string", strings.Repeat("v", tc.value)}}
			m, err := toRecord(s)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := postings(subspace.Sub("cap"), 32, "trace", &recordlayer.FDBStoredRecord[proto.Message]{Record: m})
			var admission *AdmissionError
			if tc.reject {
				if !errors.As(err, &admission) || len(entries) != 0 {
					t.Fatalf("over-cap projection admitted: %v", err)
				}
			} else if err != nil || len(entries) != 1 {
				t.Fatalf("at-cap projection rejected: %v", err)
			}
		})
	}
	if rows, err := postings(subspace.Sub("nil"), 32, "trace", nil); err != nil || rows != nil {
		t.Fatal("nil source should have no entries")
	}
}

func TestSourceAdmissionFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*Span)
	}{{"trace hex", func(s *Span) { s.Trace = "zz" }}, {"span hex", func(s *Span) { s.ID = "zz" }}, {"short trace", func(s *Span) { s.Trace = "ab" }}, {"short span", func(s *Span) { s.ID = "ab" }}, {"integer", func(s *Span) { s.Attributes[0].Type = "int"; s.Attributes[0].Value = "not-an-int" }}, {"type", func(s *Span) { s.Attributes[0].Type = "bool" }}, {"scope", func(s *Span) { s.Attributes[0].Scope = "unknown" }}} {
		t.Run(tc.name, func(t *testing.T) {
			s := Generate(testConfig(), 0)
			tc.change(&s)
			var admission *AdmissionError
			if _, err := toRecord(s); !errors.As(err, &admission) {
				t.Fatalf("invalid source accepted/untyped: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(proto.Message)
	}{
		{"wire", func(m proto.Message) {
			r := m.ProtoReflect()
			r.Set(r.Descriptor().Fields().ByName("otlp"), protoreflect.ValueOfBytes([]byte{0xff}))
		}},
		{"envelope identity", func(m proto.Message) {
			r := m.ProtoReflect()
			r.Set(r.Descriptor().Fields().ByName("trace_id"), protoreflect.ValueOfString(strings.Repeat("a", 32)))
		}},
		{"envelope span identity", func(m proto.Message) {
			r := m.ProtoReflect()
			r.Set(r.Descriptor().Fields().ByName("span_id"), protoreflect.ValueOfString(strings.Repeat("a", 16)))
		}},
		{"envelope start time", func(m proto.Message) {
			r := m.ProtoReflect()
			f := r.Descriptor().Fields().ByName("start_ns")
			r.Set(f, protoreflect.ValueOfInt64(r.Get(f).Int()+1))
		}},
		{"multiple sources", func(m proto.Message) {
			mutateOTLP(t, m, func(r *tracepb.ResourceSpans) {
				r.ScopeSpans[0].Spans = append(r.ScopeSpans[0].Spans, proto.Clone(r.ScopeSpans[0].Spans[0]).(*tracepb.Span))
			})
		}},
		{"no scope", func(m proto.Message) { mutateOTLP(t, m, func(r *tracepb.ResourceSpans) { r.ScopeSpans = nil }) }},
		{"unsupported source type", func(m proto.Message) {
			mutateOTLP(t, m, func(r *tracepb.ResourceSpans) {
				r.ScopeSpans[0].Spans[0].Attributes[0].Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := toRecord(Generate(testConfig(), 0))
			if err != nil {
				t.Fatal(err)
			}
			tc.change(m)
			var admission *AdmissionError
			if _, err := decodeRecord(m); !errors.As(err, &admission) {
				t.Fatalf("invalid decoded source accepted/untyped: %v", err)
			}
		})
	}
}

func mutateOTLP(t *testing.T, m proto.Message, change func(*tracepb.ResourceSpans)) {
	t.Helper()
	r := m.ProtoReflect()
	field := r.Descriptor().Fields().ByName("otlp")
	var source tracepb.ResourceSpans
	if err := proto.Unmarshal(r.Get(field).Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	change(&source)
	wire, err := proto.Marshal(&source)
	if err != nil {
		t.Fatal(err)
	}
	r.Set(field, protoreflect.ValueOfBytes(wire))
}
