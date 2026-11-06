package otel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func testConfig() Config {
	c := DefaultConfig()
	c.Run = "unit_fixture_283"
	c.Count = 48
	c.Tenants = 2
	c.Attributes = 8
	c.MatchEvery = 3
	c.K = 100
	c.MaxCandidates = 1000
	return c
}

func TestConfigSafety(t *testing.T) {
	t.Parallel()
	c := testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"", "default", "../../prod", "good;DROP", "UPPER_case", "hello-world"} {
		bad := c
		bad.Run = run
		if bad.Validate() == nil {
			t.Fatalf("unsafe run accepted: %q", run)
		}
	}
	m := manifest(c, "ready")
	if err := checkManifest(c, m, true); err != nil {
		t.Fatal(err)
	}
	other := c
	other.Seed++
	if checkManifest(other, m, true) == nil {
		t.Fatal("different corpus authorized")
	}
	m.State = "loading"
	if checkManifest(c, m, true) == nil {
		t.Fatal("loading run query authorized")
	}
}

func TestCorpusAndLayoutOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	m := manifest(c, "ready")
	other := c
	other.Order = "time"
	other.HashBytes = 1
	other.Backend = "ch-attrs"
	if corpusIdentity(c) != corpusIdentity(other) {
		t.Fatal("physical layout changed source corpus fingerprint")
	}
	if checkManifest(other, m, true) == nil {
		t.Fatal("wrong layout/backend reopened owned run")
	}
	seen := newOrdinalSet(3)
	for _, i := range []int{2, 0, 1} {
		if err := seen.add(i, 3); err != nil {
			t.Fatal(err)
		}
	}
	if seen.count != 3 {
		t.Fatal("incomplete audit population")
	}
	if seen.add(1, 3) == nil || seen.add(3, 3) == nil {
		t.Fatal("duplicate/out-of-range ordinal accepted")
	}
}

func TestOTLPRoundTrip(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for i := 0; i < c.Count; i++ {
		s := Generate(c, i)
		m, err := toRecord(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeRecord(m)
		if err != nil {
			t.Fatal(err)
		}
		if !equalSpan(got, s) {
			t.Fatalf("OTLP scope/type round trip at %d", i)
		}
	}
}

func TestDuplicateAttributeRejected(t *testing.T) {
	t.Parallel()
	s := Generate(testConfig(), 0)
	s.Attributes = append(s.Attributes, s.Attributes[0])
	m, err := toRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeRecord(m); err == nil {
		t.Fatal("duplicate source key accepted")
	}
}

func TestUnknownOTLPBytesPreserved(t *testing.T) {
	t.Parallel()
	s := Generate(testConfig(), 0)
	m, err := toRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	r := m.ProtoReflect()
	f := r.Descriptor().Fields().ByName("otlp")
	wire := append([]byte(nil), r.Get(f).Bytes()...)
	wire = append(wire, 0xf8, 0x07, 0x01)
	r.Set(f, protoreflect.ValueOfBytes(wire))
	encoded, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	clone := m.ProtoReflect().Type().New().Interface()
	if err := proto.Unmarshal(encoded, clone); err != nil {
		t.Fatal(err)
	}
	got := clone.ProtoReflect().Get(f).Bytes()
	if !reflect.DeepEqual(got, wire) {
		t.Fatal("opaque OTLP bytes lost")
	}
	if _, err := decodeRecord(clone); err != nil {
		t.Fatal(err)
	}
}

func collision(target string) string {
	wanted := sha256.Sum256([]byte(target))
	for i := 0; ; i++ {
		s := fmt.Sprintf("collision_%d", i)
		h := sha256.Sum256([]byte(s))
		if s != target && h[0] == wanted[0] {
			return s
		}
	}
}

func TestPostingCollisionOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	s := Generate(c, 0)
	s.Attributes = append(s.Attributes, Attribute{"span", collision(QueryKey), "string", collision(QueryValue)})
	m, err := toRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	record := &recordlayer.FDBStoredRecord[proto.Message]{Record: m}
	for _, order := range []string{"trace", "time"} {
		entries, err := postings(subspace.Sub("test"), 1, order, record)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, p := range entries {
			if seen[string(p.Key)] {
				t.Fatal("same-span digest collision overwrote another attribute")
			}
			seen[string(p.Key)] = true
			var v coveringValue
			if err := json.Unmarshal(p.Value, &v); err != nil {
				t.Fatal(err)
			}
			if v.Start != s.Start {
				t.Fatal("covering timestamp wrong")
			}
		}
	}
}

func TestOracleIdentityOrderAndScope(t *testing.T) {
	t.Parallel()
	c := testConfig()
	got := Expected(c, "span", QueryKey, "string", QueryValue)
	if len(got) != 4 {
		t.Fatalf("got %d matching traces, want 4", len(got))
	}
	for i, h := range got {
		if len(ExpectedTrace(c, h.Trace)) != 2 {
			t.Fatal("trace pairing lost")
		}
		if i > 0 && (got[i-1].Latest < h.Latest || got[i-1].Latest == h.Latest && got[i-1].Trace < h.Trace) {
			t.Fatal("order wrong")
		}
	}
	if len(Expected(c, "resource", QueryKey, "string", QueryValue)) != 12 {
		t.Fatal("scope lookalike population wrong")
	}
	if len(Expected(c, "span", QueryKey, "string", "absent")) != 0 {
		t.Fatal("negative oracle wrong")
	}
	c.K = 2
	if len(Expected(c, "span", QueryKey, "string", QueryValue)) != 2 {
		t.Fatal("K not honored")
	}
}

func TestReverseTraceOrder(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"000000", "0123abcdef", "ffff00"} {
		if restoreTrace(reverseTrace(s)) != s {
			t.Fatal("reverse trace roundtrip")
		}
	}
	if strings.Compare(string(reverseTrace("0000")), string(reverseTrace("ffff"))) <= 0 {
		t.Fatal("trace tie inversion not descending")
	}
}

func TestFactoryLifecycleContract(t *testing.T) {
	t.Parallel()
	c := testConfig()
	_, idx, err := metadata(c)
	if err != nil {
		t.Fatal(err)
	}
	f := postingFactory{}
	if !f.IsIdempotent(idx) {
		t.Fatal("idempotence contract missing")
	}
	if err := f.ValidateIndexOptions(idx); err != nil {
		t.Fatal(err)
	}
	idx.Options["order"] = "unsafe"
	if f.ValidateIndexOptions(idx) == nil {
		t.Fatal("invalid layout accepted")
	}
	m := &postingMaintainer{}
	if m.CanDeleteWhere(nil) != nil {
		t.Fatal("whole-index delete refused")
	}
	if m.CanDeleteWhere(tuple.Tuple{int64(1)}) == nil {
		t.Fatal("unsafe prefix delete accepted")
	}
}

func TestCHHTTPParametersAndSummary(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("use_query_cache") != "0" {
			t.Error("query cache enabled")
		}
		if strings.Contains(r.URL.Query().Get("query"), "needle' OR") {
			t.Error("literal interpolated into SQL")
		}
		w.Header().Set("X-ClickHouse-Summary", `{"read_rows":"27","read_bytes":"4096","elapsed_ns":"1200"}`)
		fmt.Fprint(w, "")
	}))
	defer server.Close()
	b, err := OpenCH(testConfig(), server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, summary, err := b.requestWithSummary(t.Context(), "SELECT {value:String}", nil, map[string]string{"value": "needle' OR 1=1"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ReadRows != 27 || summary.ReadBytes != 4096 || summary.Elapsed != 1200 {
		t.Fatalf("missing server accounting: %+v", summary)
	}
}

func TestCHTimeSQLUsesOrderedDistinct(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Order = "time"
	b, err := OpenCH(c, "http://127.0.0.1:8123", true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sql, _ := b.querySQL(Attribute{"span", QueryKey, "string", QueryValue})
	if !strings.Contains(sql, "ORDER BY neg_time,rev_trace,span_id LIMIT 1 BY trace_id LIMIT 100") {
		t.Fatal(sql)
	}
	if strings.Contains(sql, "GROUP BY") {
		t.Fatal("time arm unnecessarily aggregates entire candidate range")
	}
	c.Order = "trace"
	other, err := OpenCH(c, "http://127.0.0.1:8123", true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	sql, _ = other.querySQL(Attribute{})
	if !strings.Contains(sql, "GROUP BY trace_id") {
		t.Fatal("trace-ordered arm must complete max/topK")
	}
}

func FuzzOTLPRoundTrip(f *testing.F) {
	f.Add(uint64(283), uint16(17))
	f.Add(uint64(0), uint16(0))
	f.Fuzz(func(t *testing.T, seed uint64, ordinal uint16) {
		c := testConfig()
		c.Seed = seed
		s := Generate(c, int(ordinal))
		m, err := toRecord(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeRecord(m)
		if err != nil || !equalSpan(got, s) {
			t.Fatalf("round trip: %v", err)
		}
	})
}
