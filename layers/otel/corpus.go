// Package otel implements an immutable-span storage experiment, not an OTLP service.
package otel

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	ToolVersion = "otelbench-v2"
	QueryKey    = "customer.incident_key_283"
	QueryValue  = "283"
)

type Config struct {
	Run                string `json:"run"`
	Backend            string `json:"backend"`
	Seed               uint64 `json:"seed"`
	Count              int    `json:"count"`
	Tenants            int    `json:"tenants"`
	Attributes         int    `json:"attributes"`
	MatchEvery         int    `json:"match_every"`
	Batch              int    `json:"batch"`
	SaveMode           string `json:"save_mode"`
	RecordCompression  string `json:"record_compression,omitempty"`
	BulkIndexConflicts bool   `json:"bulk_index_conflicts,omitempty"`
	Workers            int    `json:"workers"`
	CHBatch            int    `json:"ch_batch"`
	CHBatchBytes       int    `json:"ch_batch_bytes"`
	CHIndexGranularity int    `json:"ch_index_granularity"`
	CHQueryTransport   string `json:"ch_query_transport"`
	CHNativeAddr       string `json:"ch_native_addr,omitempty"`
	Tenant             int    `json:"tenant"`
	K                  int    `json:"k"`
	Iterations         int    `json:"iterations"`
	MaxCandidates      int    `json:"max_candidates"`
	MaxBytes           int64  `json:"max_bytes"`
	// Order is the ClickHouse backends' query layout: trace or time.
	Order string `json:"order"`
	// Messages is the tenant store's GenAI message layout the Exporter's
	// readers query: "", "deltas" or "fulltext_v2". No otelbench backend
	// stores messages.
	Messages string `json:"messages,omitempty"`
}

func DefaultConfig() Config {
	return Config{Seed: 283, Count: 10000, Tenants: 10, Attributes: 8, MatchEvery: 100, Batch: 32, SaveMode: "pipelined", RecordCompression: "none", Workers: 1, CHBatch: 8192, CHBatchBytes: 8 << 20, CHIndexGranularity: 8192, CHQueryTransport: "http", K: 100, Iterations: 20, MaxCandidates: 20000, MaxBytes: 4 << 20, Order: "trace"}
}

func (c Config) Validate() error {
	if !regexp.MustCompile(`^[a-z0-9_]{8,48}$`).MatchString(c.Run) {
		return fail("config", "namespace", "run must be an explicit fresh 8..48 character lowercase alphanumeric/underscore namespace")
	}
	if c.Count < 1 || c.Count > 100000000 || c.Tenants < 1 || c.Tenants > (c.Count+1)/2 || c.Attributes < 4 || c.Attributes > 128 || c.MatchEvery < 1 || c.Batch < 1 || c.Batch > 64 || (c.SaveMode != "serial" && c.SaveMode != "pipelined" && c.SaveMode != "batch") || c.Workers < 1 || c.Workers > 8 || c.CHBatch < 1 || c.CHBatch > 65536 || c.CHBatchBytes < 1 || c.CHBatchBytes > 64<<20 || c.Tenant < 0 || c.Tenant >= c.Tenants || c.K < 1 || c.K > 10000 || c.Iterations < 1 || c.Iterations > 100000 || c.MaxCandidates < 1 || c.MaxCandidates > 100000000 || c.MaxBytes < 1 || (c.Order != "trace" && c.Order != "time") {
		return fail("config", "bounds", "invalid corpus/query bounds")
	}
	switch c.RecordCompression {
	case "", "none":
	case "zlib-1", "zlib-default":
		// Java-readable deflate, which the OTEL_V1 stores accept.
		if c.Backend != "sql" {
			return fail("config", "record compression", "Record Layer compression requires the SQL backend")
		}
	default:
		return fail("config", "record compression", "record compression must be none, zlib-1 or zlib-default")
	}
	switch c.Messages {
	case messagesNone, messagesDeltas, messagesFullText:
	default:
		return fail("config", "message layout", "message layout must be empty, deltas or fulltext_v2")
	}
	// Coalescing index write conflicts is the Exporter's SpanWriter's option,
	// which the sql backend's compiled direct batch saves run; sqlbackend.Open
	// refuses it on its other write paths.
	if c.BulkIndexConflicts && (c.Backend != "sql" || c.SaveMode != "batch") {
		return fail("config", "bulk index conflicts", "bulk index conflicts require the batch save mode of the sql backend")
	}
	if c.CHIndexGranularity < 1 || c.CHIndexGranularity > 8192 {
		return fail("config", "CH index granularity", "index granularity must be 1..8192 for fresh tables")
	}
	if c.CHQueryTransport != "http" && c.CHQueryTransport != "native" {
		return fail("config", "CH query transport", "query transport must be http or native")
	}
	if c.CHNativeAddr != "" || c.CHQueryTransport == "native" {
		if _, err := nativeAddress(c.CHNativeAddr); err != nil {
			return err
		}
	}
	return nil
}

func nativeAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fail("config", "CH native address", "explicit loopback host:port required; no DSN or credentials")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fail("config", "CH native address", "port must be 1..65535")
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.Unmap().IsLoopback() {
		return "", fail("config", "CH native address", "only literal loopback addresses or localhost are supported")
	}
	return net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(n)), nil
}

type Attribute struct {
	Scope string `json:"scope"`
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value string `json:"value"`
}
type Span struct {
	Ordinal    int         `json:"ordinal"`
	Tenant     int         `json:"tenant"`
	Trace      string      `json:"trace_id"`
	ID         string      `json:"span_id"`
	Start      int64       `json:"start_ns"`
	Attributes []Attribute `json:"attributes"`
}
type Hit struct {
	Trace  string `json:"trace_id"`
	Latest int64  `json:"latest"`
}
type SpanHit struct {
	ID    string `json:"span_id"`
	Start int64  `json:"start_ns"`
}
type QueryResult struct {
	Hits                []Hit  `json:"hits"`
	Candidates          int    `json:"candidates"`
	Bytes               int64  `json:"logical_read_bytes"`
	ServerReadRows      int64  `json:"server_read_rows,omitempty"`
	ServerReadBytes     int64  `json:"server_read_bytes,omitempty"`
	ServerElapsed       int64  `json:"server_elapsed_ns,omitempty"`
	ResponseBytes       int64  `json:"http_response_bytes,omitempty"`
	ServerSummary       bool   `json:"server_summary_available"`
	QueryID             string `json:"query_id,omitempty"`
	QueryTransport      string `json:"query_transport,omitempty"`
	ServerMetricsSource string `json:"server_metrics_source,omitempty"`
}

func randomWord(seed uint64, domain string, i int) [32]byte {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], seed)
	binary.BigEndian.PutUint64(b[8:], uint64(i))
	return sha256.Sum256(append([]byte(domain), b[:]...))
}

// fixtureWord cycle-walks a keyed bijection to guarantee nonzero unique IDs.
// These randomized-looking fixture identities are not cryptographic IDs.
func fixtureWord(seed uint64, domain string, ordinal int) uint64 {
	key := randomWord(seed, domain, 0)
	mix := func(x uint64) uint64 {
		x ^= binary.BigEndian.Uint64(key[:8])
		x ^= x >> 30
		x *= 0xbf58476d1ce4e5b9
		x ^= x >> 27
		x *= 0x94d049bb133111eb
		return x ^ (x >> 31)
	}
	v := mix(uint64(ordinal) + 1)
	if v == 0 {
		v = mix(0)
	}
	return v
}

// Generate makes exactly two adjacent spans per trace (except an odd final
// singleton), with injective trace/span identities and scope/type controls.
func Generate(c Config, i int) Span {
	traceOrdinal := i / 2
	var tr [16]byte
	var id [8]byte
	binary.BigEndian.PutUint64(tr[:8], fixtureWord(c.Seed, "trace-a-v2", traceOrdinal))
	binary.BigEndian.PutUint64(tr[8:], fixtureWord(c.Seed, "trace-b-v2", traceOrdinal))
	binary.BigEndian.PutUint64(id[:], fixtureWord(c.Seed, "span-v2", i))
	ts := randomWord(c.Seed, "time", i)
	s := Span{Ordinal: i, Tenant: traceOrdinal % c.Tenants, Trace: hex.EncodeToString(tr[:]), ID: hex.EncodeToString(id[:]), Start: 1700000000000000000 + int64(binary.BigEndian.Uint64(ts[:8])%86400)*1000000000}
	value := "other_" + fmt.Sprint(traceOrdinal)
	if (traceOrdinal/c.Tenants)%c.MatchEvery == 0 {
		value = QueryValue
	}
	s.Attributes = []Attribute{{"span", QueryKey, "string", value}, {"resource", QueryKey, "string", QueryValue}, {"scope", QueryKey, "string", QueryValue}, {"span", QueryKey + ".int", "int", QueryValue}}
	// Some spans carry the identical literal key with an integer instead: they
	// must never satisfy a string query, even if a digest is deliberately short.
	if (traceOrdinal/c.Tenants)%c.MatchEvery != 0 && i%3 == 0 {
		s.Attributes[0] = Attribute{"span", QueryKey, "int", QueryValue}
	}
	for j := 4; j < c.Attributes; j++ {
		s.Attributes = append(s.Attributes, Attribute{"span", fmt.Sprintf("custom.literal.%d", j), "string", fmt.Sprintf("v_%d_%d", j, i%97)})
	}
	if c.Attributes >= 8 {
		s.Attributes[4] = Attribute{"span", "gen_ai.operation.name", "string", "chat"}
		s.Attributes[5] = Attribute{"span", "gen_ai.provider.name", "string", "openai"}
		s.Attributes[6] = Attribute{"span", "gen_ai.request.model", "string", "gpt-4"}
		// Uniqueness is a synthetic fixture contract, not an OTel/provider promise.
		s.Attributes[7] = Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + s.ID}
	}
	return s
}

// Expected deliberately inspects source fixture fields directly: no OTLP
// extractor, digest, FDB key, or ClickHouse encoded-map key is shared with it.
func Expected(c Config, scope, key, typ, value string) []Hit {
	out, _ := ExpectedContext(context.Background(), c, scope, key, typ, value)
	return out
}

func ExpectedContext(ctx context.Context, c Config, scope, key, typ, value string) ([]Hit, error) {
	a := Attribute{Scope: scope, Key: key, Type: typ, Value: value}
	results, _, _, err := scanOraclePredicates(ctx, c, []Attribute{a}, nil)
	return results[a], err
}

// oracleHeap independently implements result ordering so backend sort bugs cannot define expected answers.
type oracleHeap []Hit

func (h oracleHeap) Len() int { return len(h) }
func (h oracleHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	return a.Latest < b.Latest || (a.Latest == b.Latest && a.Trace < b.Trace)
}
func (h oracleHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *oracleHeap) Push(v any)   { *h = append(*h, v.(Hit)) }
func (h *oracleHeap) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

func oracleTraceSort(out []SpanHit) {
	for i := 1; i < len(out); i++ {
		v := out[i]
		j := i
		for j > 0 {
			p := out[j-1]
			if p.Start < v.Start || (p.Start == v.Start && p.ID <= v.ID) {
				break
			}
			out[j] = p
			j--
		}
		out[j] = v
	}
}

func ExpectedTrace(c Config, trace string) []SpanHit {
	out, _ := ExpectedTraceContext(context.Background(), c, trace)
	return out
}

func ExpectedTraceContext(ctx context.Context, c Config, trace string) ([]SpanHit, error) {
	out := []SpanHit{}
	for i := 0; i < c.Count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "oracle scan", err)
		}
		s := Generate(c, i)
		if s.Tenant == c.Tenant && s.Trace == trace {
			out = append(out, SpanHit{s.ID, s.Start})
		}
	}
	oracleTraceSort(out)
	return out, ctx.Err()
}

func sortSpanHits(s []SpanHit) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Start != s[j].Start {
			return s[i].Start < s[j].Start
		}
		return s[i].ID < s[j].ID
	})
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func mapKey(a Attribute) string {
	b, _ := json.Marshal([]string{a.Scope, a.Key, a.Type})
	return string(b)
}

func equalSpan(a, b Span) bool {
	sortAttrs := func(s *Span) {
		sort.Slice(s.Attributes, func(i, j int) bool {
			x, y := s.Attributes[i], s.Attributes[j]
			if x.Scope != y.Scope {
				return x.Scope < y.Scope
			}
			if x.Key != y.Key {
				return x.Key < y.Key
			}
			return x.Type < y.Type
		})
	}
	sortAttrs(&a)
	sortAttrs(&b)
	return sameJSON(a, b)
}

type ordinalSet struct {
	bits  []uint64
	count int
}

func newOrdinalSet(n int) *ordinalSet { return &ordinalSet{bits: make([]uint64, (n+63)/64)} }
func (s *ordinalSet) add(i, n int) error {
	if i < 0 || i >= n {
		return fail("audit", "ordinal", "source ordinal out of range")
	}
	mask := uint64(1) << uint(i%64)
	if s.bits[i/64]&mask != 0 {
		return failf("audit", "ordinal", "duplicate source ordinal %d", i)
	}
	s.bits[i/64] |= mask
	s.count++
	return nil
}

func corpusIdentity(c Config) string {
	b, _ := json.Marshal(struct {
		Version                                string
		Seed                                   uint64
		Count, Tenants, Attributes, MatchEvery int
	}{ToolVersion, c.Seed, c.Count, c.Tenants, c.Attributes, c.MatchEvery})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Manifest struct {
	Tool               string `json:"tool"`
	Run                string `json:"run"`
	Corpus             string `json:"corpus"`
	State              string `json:"state"`
	Order              string `json:"order"`
	Backend            string `json:"backend"`
	CHIndexGranularity int    `json:"ch_index_granularity,omitempty"`
}

func manifest(c Config, state string) Manifest {
	m := Manifest{Tool: ToolVersion, Run: c.Run, Corpus: corpusIdentity(c), State: state, Order: c.Order, Backend: c.Backend}
	// CH table settings are layout identity, not source identity.
	if c.Backend == "ch-map" || c.Backend == "ch-attrs" {
		m.CHIndexGranularity = c.CHIndexGranularity
	}
	return m
}

func checkManifest(c Config, m Manifest, ready bool) error {
	if m.Tool != ToolVersion || m.Run != c.Run || m.Corpus != corpusIdentity(c) || m.Order != c.Order || m.Backend != c.Backend {
		return fail("ownership", "manifest", "ownership/corpus manifest mismatch; refusing access")
	}
	if (c.Backend == "ch-map" || c.Backend == "ch-attrs") && (m.CHIndexGranularity < 1 || m.CHIndexGranularity != c.CHIndexGranularity) {
		return fail("ownership", "manifest", "CH index granularity missing or mismatched; refusing access")
	}
	if ready && m.State != "ready" {
		return failf("ownership", "manifest", "run is not ready (state %q); explicit owned drop only", m.State)
	}
	return nil
}

// FixtureOTLP is a fixture span's single-span ResourceSpans and its
// deterministic wire bytes, the otlp the sql backend's OTEL_V1 rows store.
func FixtureOTLP(s Span) (*tracepb.ResourceSpans, []byte, error) {
	tr, err := hex.DecodeString(s.Trace)
	if err != nil {
		return nil, nil, wrap("admission", "trace identity", err)
	}
	id, err := hex.DecodeString(s.ID)
	if err != nil {
		return nil, nil, wrap("admission", "span identity", err)
	}
	if len(tr) != 16 || len(id) != 8 {
		return nil, nil, fail("admission", "identity", "expected 16-byte trace and 8-byte span identifiers")
	}
	otlp := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, SchemaUrl: "otelbench-resource-v2", ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "otelbench", Version: "2"}, SchemaUrl: "otelbench-scope-v2", Spans: []*tracepb.Span{{TraceId: tr, SpanId: id, Name: "immutable-fixture", StartTimeUnixNano: uint64(s.Start), EndTimeUnixNano: uint64(s.Start + 1000)}}}}}
	for _, a := range s.Attributes {
		v := &commonpb.AnyValue{}
		switch a.Type {
		case "string":
			v.Value = &commonpb.AnyValue_StringValue{StringValue: a.Value}
		case "int":
			n, e := strconv.ParseInt(a.Value, 10, 64)
			if e != nil {
				return nil, nil, wrap("admission", "integer attribute", e)
			}
			v.Value = &commonpb.AnyValue_IntValue{IntValue: n}
		default:
			return nil, nil, failf("admission", "OTLP encode", "unsupported fixture type %q", a.Type)
		}
		kv := &commonpb.KeyValue{Key: a.Key, Value: v}
		switch a.Scope {
		case "span":
			otlp.ScopeSpans[0].Spans[0].Attributes = append(otlp.ScopeSpans[0].Spans[0].Attributes, kv)
		case "resource":
			otlp.Resource.Attributes = append(otlp.Resource.Attributes, kv)
		case "scope":
			otlp.ScopeSpans[0].Scope.Attributes = append(otlp.ScopeSpans[0].Scope.Attributes, kv)
		default:
			return nil, nil, failf("admission", "OTLP encode", "unsupported fixture scope %q", a.Scope)
		}
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(otlp)
	if err != nil {
		return nil, nil, err
	}
	return otlp, wire, nil
}
