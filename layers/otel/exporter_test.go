package otel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// OTLP value builders
// ---------------------------------------------------------------------------

func exStr(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func exInt(n int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
}

func exBool(b bool) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
}

func exDouble(f float64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
}

func exBytes(b []byte) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: b}}
}

func exArray(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
}

func exKvlist(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
}

func exKV(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

// exRandomID returns n random bytes with the top bit forced so it is nonzero.
func exRandomID(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[0] |= 0x80
	return b
}

func exFind(kvs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

// exSingle is the single-span ResourceSpans an exporter stores for sp.
func exSingle(rs *tracepb.ResourceSpans, ss *tracepb.ScopeSpans, sp *tracepb.Span) *tracepb.ResourceSpans {
	out := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{sp}}}}
	out.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
	out.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
	return out
}

func requireProtoEqual(t *testing.T, label string, got, want proto.Message) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Fatalf("%s mismatch\n got: %v\nwant: %v", label, got, want)
	}
}

// ---------------------------------------------------------------------------
// ExporterConfig
// ---------------------------------------------------------------------------

func validExporterConfig() ExporterConfig {
	c := DefaultExporterConfig()
	c.ClusterFile = "/etc/foundationdb/fdb.cluster"
	c.Run = "live_run_0001"
	return c
}

func TestExporterConfigDefaults(t *testing.T) {
	want := ExporterConfig{Order: "time", HashBytes: 8, PostingValueFormat: "tuple", RecordCompression: "brotli-4", BulkIndexConflicts: true, BatchSize: 64, Workers: 8, MaxCandidates: 20000, MaxBytes: 4 << 20, K: 100}
	if got := DefaultExporterConfig(); got != want {
		t.Fatalf("defaults: got %+v want %+v", got, want)
	}
	// Defaults deliberately omit the owned cluster file and run namespace.
	err := DefaultExporterConfig().Validate()
	requireOtelKind(t, err, "config")
	if err := validExporterConfig().Validate(); err != nil {
		t.Fatalf("defaults plus cluster and run must validate: %v", err)
	}
}

func TestExporterConfigYAML(t *testing.T) {
	full := `
cluster_file: /var/fdb/fdb.cluster
run: live_run_full
tenant: 42
order: trace
hash_bytes: 32
posting_value_format: json
record_compression: none
bulk_index_conflicts: false
batch_size: 16
workers: 4
max_candidates: 500
max_bytes: 1048576
k: 25
`
	cases := []struct {
		name string
		doc  string
		want func(*ExporterConfig)
	}{
		{"empty mapping keeps every default", "{}", func(*ExporterConfig) {}},
		{"partial keeps omitted defaults", "cluster_file: /tmp/fdb.cluster\nrun: live_run_0001\ntenant: 3\n", func(c *ExporterConfig) {
			c.ClusterFile, c.Run, c.Tenant = "/tmp/fdb.cluster", "live_run_0001", 3
		}},
		{"explicit false overrides true default", "bulk_index_conflicts: false\n", func(c *ExporterConfig) { c.BulkIndexConflicts = false }},
		{"explicit zero overrides nonzero default", "max_candidates: 0\nworkers: 0\n", func(c *ExporterConfig) { c.MaxCandidates, c.Workers = 0, 0 }},
		{"unknown keys are ignored", "run: live_run_0001\nnot_a_field: 7\n", func(c *ExporterConfig) { c.Run = "live_run_0001" }},
		{"every field", full, func(c *ExporterConfig) {
			*c = ExporterConfig{ClusterFile: "/var/fdb/fdb.cluster", Run: "live_run_full", Tenant: 42, Order: "trace", HashBytes: 32, PostingValueFormat: "json", RecordCompression: "none", BulkIndexConflicts: false, BatchSize: 16, Workers: 4, MaxCandidates: 500, MaxBytes: 1 << 20, K: 25}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ExporterConfig
			if err := yaml.Unmarshal([]byte(tc.doc), &got); err != nil {
				t.Fatal(err)
			}
			want := DefaultExporterConfig()
			tc.want(&want)
			if got != want {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
		})
	}

	t.Run("destination is replaced, not merged", func(t *testing.T) {
		got := ExporterConfig{ClusterFile: "stale", Run: "stale_run_000", BatchSize: 1, K: 1}
		if err := yaml.Unmarshal([]byte("tenant: 1\n"), &got); err != nil {
			t.Fatal(err)
		}
		want := DefaultExporterConfig()
		want.Tenant = 1
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})

	t.Run("nested values receive defaults", func(t *testing.T) {
		var doc struct {
			Exporters map[string]ExporterConfig `yaml:"exporters"`
			Pointer   *ExporterConfig           `yaml:"pointer"`
		}
		src := "exporters:\n  fdb/a:\n    run: live_run_aaaa\n  fdb/b:\n    batch_size: 8\npointer:\n  k: 5\n"
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatal(err)
		}
		a, b, p := DefaultExporterConfig(), DefaultExporterConfig(), DefaultExporterConfig()
		a.Run, b.BatchSize, p.K = "live_run_aaaa", 8, 5
		if doc.Exporters["fdb/a"] != a || doc.Exporters["fdb/b"] != b || doc.Pointer == nil || *doc.Pointer != p {
			t.Fatalf("nested decode: %+v %+v", doc.Exporters, doc.Pointer)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		want := validExporterConfig()
		want.Tenant, want.Order, want.BulkIndexConflicts, want.MaxBytes = 9, "trace", false, 12345
		wire, err := yaml.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got ExporterConfig
		if err := yaml.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip: got %+v want %+v\n%s", got, want, wire)
		}
	})

	for _, doc := range []string{"batch_size: not-a-number\n", "tenant: [1, 2]\n", "bulk_index_conflicts: maybe\n", "max_bytes: {a: 1}\n"} {
		t.Run("rejects "+strings.TrimSpace(doc), func(t *testing.T) {
			var got ExporterConfig
			err := yaml.Unmarshal([]byte(doc), &got)
			requireOtelKind(t, err, "config")
			if !strings.Contains(err.Error(), "exporter YAML") {
				t.Fatalf("error lacks YAML context: %v", err)
			}
		})
	}
}

func TestExporterConfigJSON(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want func(*ExporterConfig)
	}{
		{"empty object keeps every default", "{}", func(*ExporterConfig) {}},
		{"partial keeps omitted defaults", `{"cluster_file":"/tmp/fdb.cluster","run":"live_run_0001","tenant":3}`, func(c *ExporterConfig) {
			c.ClusterFile, c.Run, c.Tenant = "/tmp/fdb.cluster", "live_run_0001", 3
		}},
		{"explicit false overrides true default", `{"bulk_index_conflicts":false}`, func(c *ExporterConfig) { c.BulkIndexConflicts = false }},
		{"explicit zero overrides nonzero default", `{"k":0,"hash_bytes":0}`, func(c *ExporterConfig) { c.K, c.HashBytes = 0, 0 }},
		{"unknown keys are ignored", `{"run":"live_run_0001","extra":true}`, func(c *ExporterConfig) { c.Run = "live_run_0001" }},
		{"every field", `{"cluster_file":"/var/fdb/fdb.cluster","run":"live_run_full","tenant":42,"order":"trace","hash_bytes":32,"posting_value_format":"json","record_compression":"zlib-1","bulk_index_conflicts":false,"batch_size":16,"workers":4,"max_candidates":500,"max_bytes":1048576,"k":25}`, func(c *ExporterConfig) {
			*c = ExporterConfig{ClusterFile: "/var/fdb/fdb.cluster", Run: "live_run_full", Tenant: 42, Order: "trace", HashBytes: 32, PostingValueFormat: "json", RecordCompression: "zlib-1", BulkIndexConflicts: false, BatchSize: 16, Workers: 4, MaxCandidates: 500, MaxBytes: 1 << 20, K: 25}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ExporterConfig
			if err := json.Unmarshal([]byte(tc.doc), &got); err != nil {
				t.Fatal(err)
			}
			want := DefaultExporterConfig()
			tc.want(&want)
			if got != want {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
		})
	}

	t.Run("destination is replaced, not merged", func(t *testing.T) {
		got := ExporterConfig{ClusterFile: "stale", Run: "stale_run_000", Workers: 1}
		if err := json.Unmarshal([]byte(`{"tenant":1}`), &got); err != nil {
			t.Fatal(err)
		}
		want := DefaultExporterConfig()
		want.Tenant = 1
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})

	t.Run("nested values receive defaults", func(t *testing.T) {
		var doc struct {
			Exporters map[string]ExporterConfig `json:"exporters"`
			List      []ExporterConfig          `json:"list"`
			Pointer   *ExporterConfig           `json:"pointer"`
		}
		src := `{"exporters":{"a":{"run":"live_run_aaaa"}},"list":[{"workers":2},{}],"pointer":{"k":5}}`
		if err := json.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatal(err)
		}
		a, l0, p := DefaultExporterConfig(), DefaultExporterConfig(), DefaultExporterConfig()
		a.Run, l0.Workers, p.K = "live_run_aaaa", 2, 5
		if doc.Exporters["a"] != a || len(doc.List) != 2 || doc.List[0] != l0 || doc.List[1] != DefaultExporterConfig() || doc.Pointer == nil || *doc.Pointer != p {
			t.Fatalf("nested decode: %+v %+v %+v", doc.Exporters, doc.List, doc.Pointer)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		want := validExporterConfig()
		want.Tenant, want.PostingValueFormat, want.RecordCompression, want.Workers = 7, "json", "none", 3
		wire, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got ExporterConfig
		if err := json.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip: got %+v want %+v\n%s", got, want, wire)
		}
	})

	for _, doc := range []string{`{"batch_size":"64"}`, `{"max_bytes":1.5}`, `{"tenant":[1]}`, `{"bulk_index_conflicts":"yes"}`} {
		t.Run("rejects "+doc, func(t *testing.T) {
			var got ExporterConfig
			err := json.Unmarshal([]byte(doc), &got)
			requireOtelKind(t, err, "config")
			if !strings.Contains(err.Error(), "exporter JSON") {
				t.Fatalf("error lacks JSON context: %v", err)
			}
		})
	}
	t.Run("rejects malformed JSON", func(t *testing.T) {
		var got ExporterConfig
		if err := json.Unmarshal([]byte(`{"run":`), &got); err == nil {
			t.Fatal("malformed JSON accepted")
		}
	})
}

func TestExporterConfigValidate(t *testing.T) {
	valid := []struct {
		name   string
		mutate func(*ExporterConfig)
	}{
		{"defaults", func(*ExporterConfig) {}},
		{"batch_size 1", func(c *ExporterConfig) { c.BatchSize = 1 }},
		{"batch_size 64", func(c *ExporterConfig) { c.BatchSize = 64 }},
		{"workers 1", func(c *ExporterConfig) { c.Workers = 1 }},
		{"workers 16", func(c *ExporterConfig) { c.Workers = 16 }},
		{"tenant 0", func(c *ExporterConfig) { c.Tenant = 0 }},
		{"last tenant", func(c *ExporterConfig) { c.Tenant = exporterTenants - 1 }},
		{"run 8 chars", func(c *ExporterConfig) { c.Run = "abcd_123" }},
		{"run 48 chars", func(c *ExporterConfig) { c.Run = strings.Repeat("a", 48) }},
		{"trace order", func(c *ExporterConfig) { c.Order = "trace" }},
		{"hash_bytes 1", func(c *ExporterConfig) { c.HashBytes = 1 }},
		{"hash_bytes 32", func(c *ExporterConfig) { c.HashBytes = 32 }},
		{"json postings", func(c *ExporterConfig) { c.PostingValueFormat = "json" }},
		{"implicit json postings", func(c *ExporterConfig) { c.PostingValueFormat = "" }},
		{"no compression", func(c *ExporterConfig) { c.RecordCompression = "none" }},
		{"implicit no compression", func(c *ExporterConfig) { c.RecordCompression = "" }},
		{"zlib", func(c *ExporterConfig) { c.RecordCompression = "zlib-1" }},
		{"brotli-0", func(c *ExporterConfig) { c.RecordCompression = "brotli-0" }},
		{"bulk conflicts off", func(c *ExporterConfig) { c.BulkIndexConflicts = false }},
		{"k 1", func(c *ExporterConfig) { c.K = 1 }},
		{"k 10000", func(c *ExporterConfig) { c.K = 10000 }},
		{"max_candidates 1", func(c *ExporterConfig) { c.MaxCandidates = 1 }},
		{"max_bytes 1", func(c *ExporterConfig) { c.MaxBytes = 1 }},
	}
	for _, tc := range valid {
		t.Run("valid "+tc.name, func(t *testing.T) {
			c := validExporterConfig()
			tc.mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatalf("%+v: %v", c, err)
			}
		})
	}

	invalid := []struct {
		name    string
		mutate  func(*ExporterConfig)
		message string
	}{
		{"missing cluster file", func(c *ExporterConfig) { c.ClusterFile = "" }, "cluster file required"},
		{"batch_size 0", func(c *ExporterConfig) { c.BatchSize = 0 }, "batch_size must be 1..64"},
		{"batch_size 65", func(c *ExporterConfig) { c.BatchSize = 65 }, "batch_size must be 1..64"},
		{"workers 0", func(c *ExporterConfig) { c.Workers = 0 }, "workers must be 1..16"},
		{"workers 17", func(c *ExporterConfig) { c.Workers = 17 }, "workers must be 1..16"},
		{"negative tenant", func(c *ExporterConfig) { c.Tenant = -1 }, "tenant must be 0.."},
		{"tenant past range", func(c *ExporterConfig) { c.Tenant = exporterTenants }, "tenant must be 0.."},
		{"missing run", func(c *ExporterConfig) { c.Run = "" }, "run must be"},
		{"short run", func(c *ExporterConfig) { c.Run = "abc_123" }, "run must be"},
		{"long run", func(c *ExporterConfig) { c.Run = strings.Repeat("a", 49) }, "run must be"},
		{"uppercase run", func(c *ExporterConfig) { c.Run = "Live_Run_0001" }, "run must be"},
		{"punctuated run", func(c *ExporterConfig) { c.Run = "live-run-0001" }, "run must be"},
		{"unknown order", func(c *ExporterConfig) { c.Order = "span" }, "invalid corpus/query bounds"},
		{"empty order", func(c *ExporterConfig) { c.Order = "" }, "invalid corpus/query bounds"},
		{"hash_bytes 0", func(c *ExporterConfig) { c.HashBytes = 0 }, "invalid corpus/query bounds"},
		{"hash_bytes 33", func(c *ExporterConfig) { c.HashBytes = 33 }, "invalid corpus/query bounds"},
		{"k 0", func(c *ExporterConfig) { c.K = 0 }, "invalid corpus/query bounds"},
		{"k 10001", func(c *ExporterConfig) { c.K = 10001 }, "invalid corpus/query bounds"},
		{"max_candidates 0", func(c *ExporterConfig) { c.MaxCandidates = 0 }, "invalid corpus/query bounds"},
		{"max_bytes 0", func(c *ExporterConfig) { c.MaxBytes = 0 }, "invalid corpus/query bounds"},
		{"unknown compression", func(c *ExporterConfig) { c.RecordCompression = "gzip" }, "record compression must be"},
		{"unknown posting format", func(c *ExporterConfig) { c.PostingValueFormat = "proto" }, "posting value format must be"},
	}
	for _, tc := range invalid {
		t.Run("invalid "+tc.name, func(t *testing.T) {
			c := validExporterConfig()
			tc.mutate(&c)
			err := c.Validate()
			requireOtelKind(t, err, "config")
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("%+v: error %q lacks %q", c, err, tc.message)
			}
			// NewExporter validates before contacting any cluster.
			e, err := NewExporter(t.Context(), c)
			if e != nil {
				t.Fatal("NewExporter returned an exporter for an invalid config")
			}
			requireOtelKind(t, err, "config")
		})
	}
}

// ---------------------------------------------------------------------------
// Canonical attribute values
// ---------------------------------------------------------------------------

func TestCanonicalValue(t *testing.T) {
	cases := []struct {
		name      string
		in        *commonpb.AnyValue
		typ, want string
	}{
		{"string", exStr("chat"), "string", "chat"},
		{"empty string", exStr(""), "string", ""},
		{"unicode string", exStr("modèle-✓"), "string", "modèle-✓"},
		{"int", exInt(42), "int", "42"},
		{"negative int", exInt(-7), "int", "-7"},
		{"max int64", exInt(math.MaxInt64), "int", "9223372036854775807"},
		{"min int64", exInt(math.MinInt64), "int", "-9223372036854775808"},
		{"bool true", exBool(true), "string", "true"},
		{"bool false", exBool(false), "string", "false"},
		{"double", exDouble(0.7), "string", "0.7"},
		{"whole double", exDouble(3), "string", "3"},
		{"negative zero", exDouble(math.Copysign(0, -1)), "string", "-0"},
		{"large double", exDouble(1e21), "string", "1e+21"},
		{"small double", exDouble(1e-7), "string", "1e-07"},
		{"wide double", exDouble(123456789), "string", "1.23456789e+08"},
		{"NaN", exDouble(math.NaN()), "string", "NaN"},
		{"+Inf", exDouble(math.Inf(1)), "string", "+Inf"},
		{"-Inf", exDouble(math.Inf(-1)), "string", "-Inf"},
		{"bytes", exBytes([]byte{0, 1, 2, 0xff}), "string", "AAEC/w=="},
		{"empty bytes", exBytes(nil), "string", ""},
		{"nil value", nil, "string", ""},
		{"unset value", &commonpb.AnyValue{}, "string", ""},
		{"bool slice", exArray(exBool(true), exBool(false)), "string", `[true,false]`},
		{"int slice", exArray(exInt(1), exInt(-2), exInt(math.MaxInt64)), "string", `[1,-2,9223372036854775807]`},
		{"float slice", exArray(exDouble(1.5), exDouble(math.NaN()), exDouble(math.Inf(1)), exDouble(math.Inf(-1)), exDouble(1e21)), "string", `[1.5,"NaN","+Inf","-Inf",1e+21]`},
		{"string slice", exArray(exStr("a"), exStr(`b"c`), exStr("<&>"), exStr("line\nbreak")), "string", `["a","b\"c","<&>","line\nbreak"]`},
		{"invalid UTF-8 in slice", exArray(exStr("\xff")), "string", "[\"\ufffd\"]"},
		{"empty slice", exArray(), "string", `[]`},
		{"bytes in slice", exArray(exBytes([]byte{1, 2, 3})), "string", `["AQID"]`},
		{"unset in slice", exArray(&commonpb.AnyValue{}, nil), "string", `[null,null]`},
		{"nested slice and map", exArray(exStr("x"), exArray(exInt(1), exArray()), exKvlist(exKV("k", exBool(true)))), "string", `["x",[1,[]],{"k":true}]`},
		{"map", exKvlist(exKV("tier", exStr("gold")), exKV("rank", exInt(2))), "string", `{"tier":"gold","rank":2}`},
		{"map duplicate keys keep order", exKvlist(exKV("b", exInt(1)), exKV("a", exStr("x")), exKV("b", exInt(2))), "string", `{"b":1,"a":"x","b":2}`},
		{"empty map", exKvlist(), "string", `{}`},
		{"nested map", exKvlist(exKV("outer", exKvlist(exKV("inner", exArray(exDouble(0.25), exBool(false))))), exKV("empty", &commonpb.AnyValue{})), "string", `{"outer":{"inner":[0.25,false]},"empty":null}`},
		{"map key escaping", exKvlist(exKV(`k"<`, exStr("v"))), "string", `{"k\"<":"v"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, value := CanonicalValue(tc.in)
			if typ != tc.typ || value != tc.want {
				t.Fatalf("CanonicalValue = (%q, %q), want (%q, %q)", typ, value, tc.typ, tc.want)
			}
			a := CanonicalAttribute("span", "k", tc.in)
			if want := (Attribute{Scope: "span", Key: "k", Type: tc.typ, Value: tc.want}); a != want {
				t.Fatalf("CanonicalAttribute = %+v, want %+v", a, want)
			}
		})
	}
	// Equal OTLP values always canonicalize identically.
	a := exKvlist(exKV("x", exArray(exDouble(math.NaN()), exBytes([]byte("hi")))))
	b := proto.Clone(a).(*commonpb.AnyValue)
	if _, va := CanonicalValue(a); va != func() string { _, v := CanonicalValue(b); return v }() {
		t.Fatal("canonical value not deterministic")
	}
}

func TestCanonicalAttributeScopes(t *testing.T) {
	for _, scope := range []string{"resource", "scope", "span"} {
		got := CanonicalAttribute(scope, "gen_ai.request.temperature", exDouble(0.7))
		if want := (Attribute{Scope: scope, Key: "gen_ai.request.temperature", Type: "string", Value: "0.7"}); got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
		got = CanonicalAttribute(scope, "gen_ai.usage.input_tokens", exInt(12))
		if want := (Attribute{Scope: scope, Key: "gen_ai.usage.input_tokens", Type: "int", Value: "12"}); got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
}

func TestCanonicalAttributesIndexedList(t *testing.T) {
	longKey := strings.Repeat("k", maxIndexedKeyBytes+1)
	edgeKey := strings.Repeat("e", maxIndexedKeyBytes)
	huge := strings.Repeat("v", maxIndexedValueBytes+1)
	edge := strings.Repeat("w", maxIndexedValueBytes)

	t.Run("string and int lists pass through unchanged", func(t *testing.T) {
		in := []*commonpb.KeyValue{exKV("a", exStr("x")), exKV("b", exInt(1)), exKV(edgeKey, exStr(edge))}
		got := canonicalAttributes(in)
		if got.changed || got.unindexed != 0 || len(got.indexed) != len(in) {
			t.Fatalf("unexpected canonicalization: %+v", got)
		}
		for i := range in {
			if got.indexed[i] != in[i] {
				t.Fatalf("indexed[%d] was copied instead of reused", i)
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		for _, in := range [][]*commonpb.KeyValue{nil, {}} {
			if got := canonicalAttributes(in); got.changed || got.unindexed != 0 || len(got.indexed) != 0 {
				t.Fatalf("empty list: %+v", got)
			}
		}
	})

	t.Run("duplicates, conversions, nil and caps", func(t *testing.T) {
		in := []*commonpb.KeyValue{
			exKV("a", exInt(1)),
			nil,
			exKV("b", exStr("x")),
			exKV("a", exStr("2")),
			exKV("c", exBool(true)),
			exKV(longKey, exStr("v")),
			exKV("big", exStr(huge)),
			exKV("shrinks", exStr(huge)),
			exKV("grows", exStr("small")),
			exKV("d", exArray(exDouble(1.5))),
			exKV("a", exInt(3)),
			exKV("shrinks", exStr("ok")),
			exKV("grows", exStr(huge)),
		}
		got := canonicalAttributes(in)
		if !got.changed {
			t.Fatal("expected changed")
		}
		// First position kept, last value wins, oversized latest values unindexed.
		want := []Attribute{
			{"span", "a", "int", "3"},
			{"span", "b", "string", "x"},
			{"span", "c", "string", "true"},
			{"span", "shrinks", "string", "ok"},
			{"span", "d", "string", "[1.5]"},
		}
		var gotAttrs []Attribute
		for _, kv := range got.indexed {
			gotAttrs = append(gotAttrs, CanonicalAttribute("span", kv.Key, kv.Value))
			switch kv.Value.GetValue().(type) {
			case *commonpb.AnyValue_StringValue, *commonpb.AnyValue_IntValue:
			default:
				t.Fatalf("indexed %q kept non-canonical type %T", kv.Key, kv.Value.GetValue())
			}
		}
		if !reflect.DeepEqual(gotAttrs, want) {
			t.Fatalf("indexed\n got %+v\nwant %+v", gotAttrs, want)
		}
		if got.unindexed != 3 { // longKey, big, grows
			t.Fatalf("unindexed = %d, want 3", got.unindexed)
		}
		// The input list is never mutated.
		if in[4].Value.GetBoolValue() != true || in[0].Value.GetIntValue() != 1 {
			t.Fatal("input mutated")
		}
	})

	t.Run("duplicate alone marks changed", func(t *testing.T) {
		got := canonicalAttributes([]*commonpb.KeyValue{exKV("a", exStr("1")), exKV("a", exStr("2"))})
		if !got.changed || len(got.indexed) != 1 || got.indexed[0].Value.GetStringValue() != "2" {
			t.Fatalf("duplicate: %+v", got)
		}
	})
}

func TestPartitionExport(t *testing.T) {
	if got := partitionExport(nil, 4); len(got) != 0 {
		t.Fatalf("empty partition: %+v", got)
	}
	items := make([]exportSpan, 5)
	for i := range items {
		items[i] = exportSpan{logical: int64(10 * (i + 1)), cost: 100}
	}
	got := partitionExport(items, 2)
	type shape struct {
		first, next, n int
		logical        int64
		estimate       int
	}
	var shapes []shape
	for _, b := range got {
		shapes = append(shapes, shape{b.first, b.next, len(b.messages), b.logicalBytes, b.estimate})
	}
	want := []shape{{0, 2, 2, 30, 200}, {2, 4, 2, 70, 200}, {4, 5, 1, 50, 100}}
	if !reflect.DeepEqual(shapes, want) {
		t.Fatalf("size partition\n got %+v\nwant %+v", shapes, want)
	}

	// The conservative transaction estimate splits independently of size.
	items = []exportSpan{{cost: 200000}, {cost: 200000}, {cost: 50000}, {cost: 50000}, {cost: 1}}
	shapes = nil
	for _, b := range partitionExport(items, 64) {
		shapes = append(shapes, shape{b.first, b.next, len(b.messages), b.logicalBytes, b.estimate})
	}
	want = []shape{{0, 1, 1, 0, 200000}, {1, 4, 3, 0, exportSpanBudget}, {4, 5, 1, 0, 1}}
	if !reflect.DeepEqual(shapes, want) {
		t.Fatalf("budget partition\n got %+v\nwant %+v", shapes, want)
	}
}

// ---------------------------------------------------------------------------
// ResourceSpansFromReadOnly
// ---------------------------------------------------------------------------

func exSpanContext(t *testing.T, sampled, remote bool, state string) trace.SpanContext {
	t.Helper()
	var tid trace.TraceID
	var sid trace.SpanID
	copy(tid[:], exRandomID(t, 16))
	copy(sid[:], exRandomID(t, 8))
	cfg := trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: remote}
	if sampled {
		cfg.TraceFlags = trace.FlagsSampled
	}
	if state != "" {
		ts, err := trace.ParseTraceState(state)
		if err != nil {
			t.Fatal(err)
		}
		cfg.TraceState = ts
	}
	return trace.NewSpanContext(cfg)
}

func TestResourceSpansFromReadOnlySpanFields(t *testing.T) {
	sc := exSpanContext(t, true, false, "vendor=opaque")
	parentSC := trace.NewSpanContext(trace.SpanContextConfig{TraceID: sc.TraceID(), SpanID: exSpanContext(t, true, false, "").SpanID(), TraceFlags: trace.FlagsSampled, Remote: true})
	linkSC := exSpanContext(t, false, true, "other=link")
	start := time.Unix(1_700_000_000, 123)
	res := resource.NewWithAttributes(
		"https://opentelemetry.io/schemas/1.26.0",
		attribute.String("service.name", "genai-gateway"),
		attribute.Bool("service.canary", true),
		attribute.Int64("service.shard", 3),
	)
	scope := instrumentation.Scope{Name: "fdb.dev/otel/test", Version: "1.2.3", SchemaURL: "https://example.test/scope", Attributes: attribute.NewSet(attribute.String("scope.team", "ml"), attribute.Float64("scope.ratio", 0.25))}
	stub := tracetest.SpanStub{
		Name:        "chat gpt-4o",
		SpanContext: sc,
		Parent:      parentSC,
		SpanKind:    trace.SpanKindClient,
		StartTime:   start,
		EndTime:     start.Add(time.Second),
		Attributes: []attribute.KeyValue{
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.Int64("gen_ai.usage.input_tokens", 42),
			attribute.Bool("gen_ai.request.stream", true),
			attribute.Float64("gen_ai.request.temperature", 0.5),
			attribute.Float64("nan", math.NaN()),
			attribute.Float64("pinf", math.Inf(1)),
			attribute.Float64("ninf", math.Inf(-1)),
			attribute.ByteSlice("blob", []byte{0, 1, 2, 0xff}),
			attribute.BoolSlice("bools", []bool{true, false}),
			attribute.Int64Slice("ints", []int64{1, -2}),
			attribute.IntSlice("plain_ints", []int{7}),
			attribute.Float64Slice("floats", []float64{1.5, math.NaN(), math.Inf(-1)}),
			attribute.StringSlice("strings", []string{"stop", "length"}),
			attribute.Slice("nested", attribute.StringValue("x"), attribute.Int64SliceValue([]int64{1}), attribute.MapValue(attribute.Bool("k", true))),
			attribute.Map("labels", attribute.String("z", "1"), attribute.Int("a", 2)),
			{Key: "empty"},
			attribute.String("dup", "first"),
			attribute.Int64("dup", 2),
		},
		Events: []sdktrace.Event{
			{Name: "gen_ai.content.prompt", Time: start.Add(time.Millisecond), Attributes: []attribute.KeyValue{attribute.String("gen_ai.prompt", "hi")}, DroppedAttributeCount: 2},
			{Name: "bare", Time: start.Add(2 * time.Millisecond)},
		},
		Links:                []sdktrace.Link{{SpanContext: linkSC, Attributes: []attribute.KeyValue{attribute.Bool("link.retry", true)}, DroppedAttributeCount: 1}},
		Status:               sdktrace.Status{Code: codes.Error, Description: "rate limited"},
		DroppedAttributes:    3,
		DroppedEvents:        4,
		DroppedLinks:         -1,
		Resource:             res,
		InstrumentationScope: scope,
	}
	tid, sid, psid := sc.TraceID(), sc.SpanID(), parentSC.SpanID()
	ltid, lsid := linkSC.TraceID(), linkSC.SpanID()
	wantSpan := &tracepb.Span{
		TraceId:           tid[:],
		SpanId:            sid[:],
		TraceState:        "vendor=opaque",
		ParentSpanId:      psid[:],
		Flags:             0x301, // sampled | has_is_remote | is_remote (remote parent)
		Name:              "chat gpt-4o",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: uint64(start.UnixNano()),
		EndTimeUnixNano:   uint64(start.Add(time.Second).UnixNano()),
		Attributes: []*commonpb.KeyValue{
			exKV("gen_ai.operation.name", exStr("chat")),
			exKV("gen_ai.usage.input_tokens", exInt(42)),
			exKV("gen_ai.request.stream", exBool(true)),
			exKV("gen_ai.request.temperature", exDouble(0.5)),
			exKV("nan", exDouble(math.NaN())),
			exKV("pinf", exDouble(math.Inf(1))),
			exKV("ninf", exDouble(math.Inf(-1))),
			exKV("blob", exBytes([]byte{0, 1, 2, 0xff})),
			exKV("bools", exArray(exBool(true), exBool(false))),
			exKV("ints", exArray(exInt(1), exInt(-2))),
			exKV("plain_ints", exArray(exInt(7))),
			exKV("floats", exArray(exDouble(1.5), exDouble(math.NaN()), exDouble(math.Inf(-1)))),
			exKV("strings", exArray(exStr("stop"), exStr("length"))),
			exKV("nested", exArray(exStr("x"), exArray(exInt(1)), exKvlist(exKV("k", exBool(true))))),
			exKV("labels", exKvlist(exKV("a", exInt(2)), exKV("z", exStr("1")))), // attribute.Map sorts keys
			exKV("empty", &commonpb.AnyValue{}),
			exKV("dup", exStr("first")),
			exKV("dup", exInt(2)),
		},
		DroppedAttributesCount: 3,
		Events: []*tracepb.Span_Event{
			{TimeUnixNano: uint64(start.Add(time.Millisecond).UnixNano()), Name: "gen_ai.content.prompt", Attributes: []*commonpb.KeyValue{exKV("gen_ai.prompt", exStr("hi"))}, DroppedAttributesCount: 2},
			{TimeUnixNano: uint64(start.Add(2 * time.Millisecond).UnixNano()), Name: "bare"},
		},
		DroppedEventsCount: 4,
		Links: []*tracepb.Span_Link{{
			TraceId: ltid[:], SpanId: lsid[:], TraceState: "other=link",
			Attributes:             []*commonpb.KeyValue{exKV("link.retry", exBool(true))},
			DroppedAttributesCount: 1,
			Flags:                  0x300, // unsampled | has_is_remote | is_remote
		}},
		DroppedLinksCount: 0, // negative counts clamp to zero
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "rate limited"},
	}
	want := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{ // resource attributes iterate in key order
			exKV("service.canary", exBool(true)),
			exKV("service.name", exStr("genai-gateway")),
			exKV("service.shard", exInt(3)),
		}},
		SchemaUrl: "https://opentelemetry.io/schemas/1.26.0",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "fdb.dev/otel/test", Version: "1.2.3", Attributes: []*commonpb.KeyValue{
				exKV("scope.ratio", exDouble(0.25)),
				exKV("scope.team", exStr("ml")),
			}},
			SchemaUrl: "https://example.test/scope",
			Spans:     []*tracepb.Span{wantSpan},
		}},
	}}
	got := ResourceSpansFromReadOnly([]sdktrace.ReadOnlySpan{stub.Snapshot()})
	if len(got) != 1 {
		t.Fatalf("got %d resource spans", len(got))
	}
	requireProtoEqual(t, "converted span", got[0], want[0])

	// Every converted attribute canonicalizes to the documented query value.
	canonical := map[string][2]string{
		"gen_ai.operation.name":      {"string", "chat"},
		"gen_ai.usage.input_tokens":  {"int", "42"},
		"gen_ai.request.stream":      {"string", "true"},
		"gen_ai.request.temperature": {"string", "0.5"},
		"nan":                        {"string", "NaN"},
		"pinf":                       {"string", "+Inf"},
		"ninf":                       {"string", "-Inf"},
		"blob":                       {"string", "AAEC/w=="},
		"bools":                      {"string", `[true,false]`},
		"ints":                       {"string", `[1,-2]`},
		"plain_ints":                 {"string", `[7]`},
		"floats":                     {"string", `[1.5,"NaN","-Inf"]`},
		"strings":                    {"string", `["stop","length"]`},
		"nested":                     {"string", `["x",[1],{"k":true}]`},
		"labels":                     {"string", `{"a":2,"z":"1"}`},
		"empty":                      {"string", ""},
	}
	for _, kv := range got[0].ScopeSpans[0].Spans[0].Attributes {
		if kv.Key == "dup" {
			continue
		}
		typ, value := CanonicalValue(kv.Value)
		if w, ok := canonical[kv.Key]; !ok || w != [2]string{typ, value} {
			t.Errorf("%s canonicalizes to (%q, %q), want %q", kv.Key, typ, value, w)
		}
	}
	// Duplicate keys survive conversion; the indexed copy keeps the last value.
	set := canonicalAttributes(got[0].ScopeSpans[0].Spans[0].Attributes)
	if v := exFind(set.indexed, "dup"); v.GetIntValue() != 2 {
		t.Fatalf("duplicate canonical value = %v, want int 2", v)
	}
}

func TestResourceSpansFromReadOnlyGrouping(t *testing.T) {
	resA := resource.NewWithAttributes("schema/a", attribute.String("service.name", "a"))
	resA2 := resource.NewWithAttributes("schema/a", attribute.String("service.name", "a")) // equivalent to resA
	resB := resource.NewWithAttributes("schema/b", attribute.String("service.name", "a"))  // schema differs
	s1 := instrumentation.Scope{Name: "s1", Version: "1"}
	s2 := instrumentation.Scope{Name: "s2"}
	s1Attrs := instrumentation.Scope{Name: "s1", Version: "1", Attributes: attribute.NewSet(attribute.String("x", "y"))}
	start := time.Unix(1_700_000_000, 0)
	mk := func(name string, res *resource.Resource, scope instrumentation.Scope) sdktrace.ReadOnlySpan {
		return tracetest.SpanStub{Name: name, SpanContext: exSpanContext(t, true, false, ""), StartTime: start, EndTime: start, Resource: res, InstrumentationScope: scope}.Snapshot()
	}
	unclamped := tracetest.SpanStub{Name: "pre-epoch", SpanContext: exSpanContext(t, false, false, ""), StartTime: time.Unix(-1, 0), EndTime: time.Unix(-1, 0), Status: sdktrace.Status{Code: codes.Ok}, SpanKind: trace.SpanKindConsumer}.Snapshot()
	spans := []sdktrace.ReadOnlySpan{
		mk("1", resA, s1),
		nil,
		mk("2", resA2, s2),
		mk("3", resB, s1),
		mk("4", resA, s1),
		mk("5", resA2, s1Attrs),
		unclamped,
	}
	got := ResourceSpansFromReadOnly(spans)
	names := func(ss *tracepb.ScopeSpans) []string {
		var out []string
		for _, sp := range ss.Spans {
			out = append(out, sp.Name)
		}
		return out
	}
	if len(got) != 3 {
		t.Fatalf("got %d resource groups, want 3", len(got))
	}
	if got[0].SchemaUrl != "schema/a" || got[1].SchemaUrl != "schema/b" || got[2].SchemaUrl != "" {
		t.Fatalf("resource order: %q %q %q", got[0].SchemaUrl, got[1].SchemaUrl, got[2].SchemaUrl)
	}
	a := got[0].ScopeSpans
	if len(a) != 3 || a[0].Scope.Name != "s1" || len(a[0].Scope.Attributes) != 0 || a[1].Scope.Name != "s2" || a[2].Scope.Name != "s1" || len(a[2].Scope.Attributes) != 1 {
		t.Fatalf("resource A scopes: %v", a)
	}
	if !reflect.DeepEqual(names(a[0]), []string{"1", "4"}) || !reflect.DeepEqual(names(a[1]), []string{"2"}) || !reflect.DeepEqual(names(a[2]), []string{"5"}) {
		t.Fatalf("resource A spans: %v %v %v", names(a[0]), names(a[1]), names(a[2]))
	}
	if b := got[1].ScopeSpans; len(b) != 1 || !reflect.DeepEqual(names(b[0]), []string{"3"}) {
		t.Fatalf("resource B: %v", b)
	}
	// A span without resource or scope keeps both absent.
	c := got[2]
	if c.Resource != nil || len(c.ScopeSpans) != 1 || c.ScopeSpans[0].Scope != nil {
		t.Fatalf("empty resource/scope group: %v", c)
	}
	sp := c.ScopeSpans[0].Spans[0]
	if sp.StartTimeUnixNano != 0 || sp.EndTimeUnixNano != 0 {
		t.Fatalf("pre-epoch timestamps not clamped: %d %d", sp.StartTimeUnixNano, sp.EndTimeUnixNano)
	}
	if sp.Kind != tracepb.Span_SPAN_KIND_CONSUMER || sp.Status.GetCode() != tracepb.Status_STATUS_CODE_OK || sp.Flags != 0x100 || len(sp.ParentSpanId) != 0 {
		t.Fatalf("root span fields: %v", sp)
	}
	if len(ResourceSpansFromReadOnly(nil)) != 0 || len(ResourceSpansFromReadOnly([]sdktrace.ReadOnlySpan{nil})) != 0 {
		t.Fatal("empty input produced resource spans")
	}
}

func TestOTLPEnumMappings(t *testing.T) {
	kinds := map[trace.SpanKind]tracepb.Span_SpanKind{
		trace.SpanKindUnspecified: tracepb.Span_SPAN_KIND_UNSPECIFIED,
		trace.SpanKindInternal:    tracepb.Span_SPAN_KIND_INTERNAL,
		trace.SpanKindServer:      tracepb.Span_SPAN_KIND_SERVER,
		trace.SpanKindClient:      tracepb.Span_SPAN_KIND_CLIENT,
		trace.SpanKindProducer:    tracepb.Span_SPAN_KIND_PRODUCER,
		trace.SpanKindConsumer:    tracepb.Span_SPAN_KIND_CONSUMER,
		trace.SpanKind(99):        tracepb.Span_SPAN_KIND_UNSPECIFIED,
	}
	for in, want := range kinds {
		if got := otlpSpanKind(in); got != want {
			t.Errorf("kind %v -> %v, want %v", in, got, want)
		}
	}
	statuses := map[codes.Code]tracepb.Status_StatusCode{
		codes.Unset: tracepb.Status_STATUS_CODE_UNSET,
		codes.Ok:    tracepb.Status_STATUS_CODE_OK,
		codes.Error: tracepb.Status_STATUS_CODE_ERROR,
	}
	for in, want := range statuses {
		if got := otlpStatusCode(in); got != want {
			t.Errorf("status %v -> %v, want %v", in, got, want)
		}
	}
	for in, want := range map[int]uint32{-5: 0, 0: 0, 7: 7, math.MaxInt32: math.MaxInt32} {
		if got := clampCount(in); got != want {
			t.Errorf("clampCount(%d) = %d, want %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Export preparation (no FDB)
// ---------------------------------------------------------------------------

func TestExporterPrepareExport(t *testing.T) {
	cfg := validExporterConfig()
	cfg.Tenant = 5
	e := &Exporter{cfg: cfg, done: make(chan struct{})}
	traceID, spanA, spanB := exRandomID(t, 16), exRandomID(t, 8), exRandomID(t, 8)
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 4242, protowire.VarintType), 7)
	rs := &tracepb.ResourceSpans{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc")), exKV("res.flag", exBool(true))}},
		SchemaUrl: "schema/r",
		ScopeSpans: []*tracepb.ScopeSpans{nil, {
			Scope:     &commonpb.InstrumentationScope{Name: "scope", Attributes: []*commonpb.KeyValue{exKV("scope.team", exStr("ml"))}},
			SchemaUrl: "schema/s",
			Spans: []*tracepb.Span{
				{TraceId: traceID, SpanId: spanA, Name: "first", StartTimeUnixNano: 100, Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("chat")), exKV("t", exDouble(0.5))}},
				nil,
				{TraceId: traceID, SpanId: spanB, Name: "plain", StartTimeUnixNano: 200, Attributes: []*commonpb.KeyValue{exKV("n", exInt(1))}},
				{TraceId: traceID, SpanId: spanA, Name: "replacement", StartTimeUnixNano: 150, Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("embeddings")), exKV("dup", exInt(1)), exKV("dup", exInt(2))}},
			},
		}},
	}
	rs.ProtoReflect().SetUnknown(unknown)
	plain := &tracepb.ResourceSpans{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("plain"))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: traceID, SpanId: exRandomID(t, 8), StartTimeUnixNano: 300}}}},
	}
	items, stats, err := e.prepareExport(t.Context(), []*tracepb.ResourceSpans{nil, rs, plain})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ExportStats{ReceivedSpans: 4, DuplicateSpans: 1, NormalizedSpans: 2}); stats != want {
		t.Fatalf("stats %+v want %+v", stats, want)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items", len(items))
	}
	ss := rs.ScopeSpans[1]
	// The replacement keeps the first occurrence's position.
	wants := []struct {
		single     *tracepb.ResourceSpans
		start      int64
		indexed    []Attribute
		normalized bool
	}{
		{exSingle(rs, ss, ss.Spans[3]), 150, []Attribute{{"resource", "service.name", "string", "svc"}, {"resource", "res.flag", "string", "true"}, {"scope", "scope.team", "string", "ml"}, {"span", "gen_ai.operation.name", "string", "embeddings"}, {"span", "dup", "int", "2"}}, true},
		{exSingle(rs, ss, ss.Spans[2]), 200, []Attribute{{"resource", "service.name", "string", "svc"}, {"resource", "res.flag", "string", "true"}, {"scope", "scope.team", "string", "ml"}, {"span", "n", "int", "1"}}, true},
		{exSingle(plain, plain.ScopeSpans[0], plain.ScopeSpans[0].Spans[0]), 300, []Attribute{{"resource", "service.name", "string", "plain"}}, false},
	}
	ordinals := map[int]bool{}
	for i, w := range wants {
		it := items[i]
		s, err := decodeRecord(it.message)
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
		sp := w.single.ScopeSpans[0].Spans[0]
		if s.Tenant != 5 || s.Trace != hex.EncodeToString(sp.TraceId) || s.ID != hex.EncodeToString(sp.SpanId) || s.Start != w.start {
			t.Fatalf("item %d envelope %+v", i, s)
		}
		if it.key != s.Trace+s.ID || it.normalized != w.normalized || it.logical <= 0 || it.cost <= 2048 {
			t.Fatalf("item %d metadata %+v", i, it)
		}
		if !reflect.DeepEqual(s.Attributes, w.indexed) {
			t.Fatalf("item %d indexed\n got %+v\nwant %+v", i, s.Attributes, w.indexed)
		}
		r := it.message.ProtoReflect()
		restored, err := restoreOTLP(r.Get(r.Descriptor().Fields().ByName("otlp")).Bytes())
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("item %d restored", i), restored, w.single)
		if s.Ordinal < 1 || ordinals[s.Ordinal] {
			t.Fatalf("item %d ordinal %d not positive and unique", i, s.Ordinal)
		}
		ordinals[s.Ordinal] = true
	}
	// The caller's input is never rewritten by canonicalization.
	if rs.Resource.Attributes[1].Value.GetBoolValue() != true || ss.Spans[0].Attributes[1].Value.GetDoubleValue() != 0.5 {
		t.Fatal("input mutated")
	}

	t.Run("admission", func(t *testing.T) {
		good := func() *tracepb.Span {
			return &tracepb.Span{TraceId: exRandomID(t, 16), SpanId: exRandomID(t, 8), StartTimeUnixNano: 1}
		}
		cases := map[string]func(*tracepb.Span){
			"zero trace":       func(s *tracepb.Span) { s.TraceId = make([]byte, 16) },
			"short trace":      func(s *tracepb.Span) { s.TraceId = s.TraceId[:15] },
			"missing trace":    func(s *tracepb.Span) { s.TraceId = nil },
			"zero span":        func(s *tracepb.Span) { s.SpanId = make([]byte, 8) },
			"long span":        func(s *tracepb.Span) { s.SpanId = append(s.SpanId, 1) },
			"start past int64": func(s *tracepb.Span) { s.StartTimeUnixNano = math.MaxInt64 + 1 },
			"oversized span": func(s *tracepb.Span) {
				s.Attributes = []*commonpb.KeyValue{exKV("a", exStr(strings.Repeat("x", 60000))), exKV("b", exStr(strings.Repeat("y", 60000))), exKV("c", exStr(strings.Repeat("z", 60000)))}
			},
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				bad := good()
				mutate(bad)
				in := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{good(), bad}}}}}
				items, _, err := e.prepareExport(t.Context(), in)
				requireOtelKind(t, err, "admission")
				if items != nil {
					t.Fatal("inadmissible export returned prepared items")
				}
			})
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := e.prepareExport(ctx, []*tracepb.ResourceSpans{plain})
		requireOtelKind(t, err, "canceled")
	})
}

func TestRestoreOTLPRejectsCorruption(t *testing.T) {
	one := &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "a"}}}}}
	two := &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "a"}, {Name: "b"}}}}}
	withFidelity := func(payload []byte) []byte {
		rs := proto.Clone(one).(*tracepb.ResourceSpans)
		u := protowire.AppendTag(nil, fidelityField, protowire.BytesType)
		rs.ProtoReflect().SetUnknown(protowire.AppendBytes(u, payload))
		wire, err := proto.Marshal(rs)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	mustMarshal := func(m proto.Message) []byte {
		wire, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	short, _ := fidelityPayload([3]bool{true}, []*commonpb.KeyValue{exKV("a", exBool(true))})
	for name, wire := range map[string][]byte{
		"not protobuf":         {0xff, 0xff, 0xff},
		"two spans":            mustMarshal(two),
		"no spans":             mustMarshal(&tracepb.ResourceSpans{}),
		"fidelity not array":   withFidelity([]byte{0xff}),
		"fidelity wrong arity": withFidelity(short),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := restoreOTLP(wire)
			requireOtelKind(t, err, "corruption")
		})
	}
	// A record without fidelity restores verbatim.
	got, err := restoreOTLP(mustMarshal(one))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "plain restore", got, one)
}

// ---------------------------------------------------------------------------
// Real FDB integration
// ---------------------------------------------------------------------------

func startExporterCluster(t *testing.T) string {
	t.Helper()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
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
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
}

func openTestExporter(t *testing.T, ctx context.Context, cluster string, mutate func(*ExporterConfig)) *Exporter {
	t.Helper()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile = cluster
	cfg.Run = freshRun(t)
	cfg.Tenant = 11
	if mutate != nil {
		mutate(&cfg)
	}
	e, err := NewExporter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e.Config() != cfg {
		t.Fatalf("Config() = %+v, want %+v", e.Config(), cfg)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return e
}

func requireHits(t *testing.T, ctx context.Context, e *Exporter, a Attribute, want ...Hit) QueryResult {
	t.Helper()
	got, err := e.Query(ctx, a)
	if err != nil {
		t.Fatalf("query %+v: %v", a, err)
	}
	if got.Hits == nil {
		t.Fatalf("query %+v returned nil hits", a)
	}
	if len(want) == 0 {
		want = []Hit{}
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("query %+v\n got %+v\nwant %+v", a, got.Hits, want)
	}
	return got
}

func requireTrace(t *testing.T, ctx context.Context, e *Exporter, traceID string, want ...SpanHit) {
	t.Helper()
	got, err := e.Trace(ctx, traceID)
	if err != nil {
		t.Fatalf("trace %s: %v", traceID, err)
	}
	if len(want) == 0 {
		want = []SpanHit{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %s\n got %+v\nwant %+v", traceID, got, want)
	}
}

func requireSpan(t *testing.T, ctx context.Context, e *Exporter, spanID string, want ...SpanMatch) {
	t.Helper()
	got, err := e.Span(ctx, spanID)
	if err != nil {
		t.Fatalf("span %s: %v", spanID, err)
	}
	if len(want) == 0 {
		want = []SpanMatch{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("span %s\n got %+v\nwant %+v", spanID, got, want)
	}
}

func requireTime(t *testing.T, ctx context.Context, e *Exporter, r TimeRange, want ...Hit) {
	t.Helper()
	got, err := e.Time(ctx, r)
	if err != nil {
		t.Fatalf("time %+v: %v", r, err)
	}
	if len(want) == 0 {
		want = []Hit{}
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("time %+v\n got %+v\nwant %+v", r, got.Hits, want)
	}
}

// requireRecorded checks LoadSpanOTLP against the SDK's own view of every span.
func requireRecorded(t *testing.T, ctx context.Context, e *Exporter, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, ro := range spans {
		want := ResourceSpansFromReadOnly([]sdktrace.ReadOnlySpan{ro})
		sc := ro.SpanContext()
		got, err := e.LoadSpanOTLP(ctx, sc.TraceID().String(), sc.SpanID().String())
		if err != nil {
			t.Fatalf("load %s/%s (%s): %v", sc.TraceID(), sc.SpanID(), ro.Name(), err)
		}
		requireProtoEqual(t, "LoadSpanOTLP "+ro.Name(), got, want[0])
	}
}

func sortedSpanHits(hits ...SpanHit) []SpanHit {
	sortSpanHits(hits)
	return hits
}

var exporterLayouts = []struct {
	name   string
	mutate func(*ExporterConfig)
}{
	{"default", nil},
	{"trace_order_json_uncompressed", func(c *ExporterConfig) {
		c.Order, c.PostingValueFormat, c.RecordCompression, c.BulkIndexConflicts, c.HashBytes = "trace", "json", "none", false, 32
		c.BatchSize, c.Workers = 1, 1
	}},
}

// One FDB testcontainer serves every subtest; each opens a fresh run namespace.
func TestExporterRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, layout := range exporterLayouts {
		t.Run("sdk_syncer_genai/"+layout.name, func(t *testing.T) { testExporterSDKSyncer(t, cluster, layout.mutate) })
	}
	t.Run("sdk_batch_span_processor", func(t *testing.T) { testExporterBatchSpanProcessor(t, cluster) })
	t.Run("otlp_fidelity", func(t *testing.T) { testExporterOTLPFidelity(t, cluster) })
	t.Run("batching_dedup_idempotent", func(t *testing.T) { testExporterBatching(t, cluster) })
	t.Run("shutdown_and_reopen", func(t *testing.T) { testExporterShutdown(t, cluster) })
	t.Run("concurrent_exports", func(t *testing.T) { testExporterConcurrent(t, cluster) })
}

func testExporterSDKSyncer(t *testing.T, cluster string, mutate func(*ExporterConfig)) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, mutate)
	rec := tracetest.NewSpanRecorder()
	res := resource.NewWithAttributes(
		"https://opentelemetry.io/schemas/1.26.0",
		attribute.String("service.name", "genai-gateway"),
		attribute.String("deployment.environment.name", "integration"),
		attribute.Int64("service.shard", 3),
		attribute.Bool("service.canary", true),
		attribute.StringSlice("service.regions", []string{"us-east-1", "eu-west-1"}),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(e), sdktrace.WithSpanProcessor(rec), sdktrace.WithResource(res))
	tpOpen := true
	t.Cleanup(func() {
		if tpOpen {
			_ = tp.Shutdown(context.Background())
		}
	})
	tracer := tp.Tracer(
		"fdb.dev/layers/otel/exporter_test",
		trace.WithInstrumentationVersion("1.4.0"),
		trace.WithSchemaURL("https://opentelemetry.io/schemas/1.26.0"),
		trace.WithInstrumentationAttributes(attribute.String("scope.team", "ml-platform"), attribute.Bool("scope.experimental", true), attribute.Float64("scope.sample_ratio", 0.25)),
	)

	base := time.Unix(1_750_000_000, 0)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	ns := func(d time.Duration) int64 { return at(d).UnixNano() }
	ms := time.Millisecond
	responseID := "chatcmpl-" + freshRun(t)

	rootCtx, agent := tracer.Start(context.Background(), "invoke_agent planner", trace.WithTimestamp(at(0)), trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", "planner"),
		attribute.String("gen_ai.request.model", "gpt-4o"),
		attribute.String("custom.session", "session-1"),
	))
	_, chat := tracer.Start(rootCtx, "chat gpt-4o", trace.WithTimestamp(at(10*ms)), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.system", "openai"),
		attribute.String("gen_ai.request.model", "gpt-4o"),
		attribute.String("gen_ai.response.model", "gpt-4o-2024-08-06"),
		attribute.String("gen_ai.response.id", responseID),
		attribute.Int64("gen_ai.usage.input_tokens", 1234),
		attribute.Int64("gen_ai.usage.output_tokens", 56),
		attribute.Float64("gen_ai.request.temperature", 0.7),
		attribute.Bool("gen_ai.request.stream", true),
		attribute.StringSlice("gen_ai.response.finish_reasons", []string{"stop", "length"}),
		attribute.Int64Slice("custom.shard_ids", []int64{3, 1, 4}),
		attribute.Float64("custom.score", math.NaN()),
		attribute.Float64("custom.ceiling", math.Inf(1)),
		attribute.Float64("custom.floor", math.Inf(-1)),
		attribute.Map("custom.labels", attribute.String("tier", "gold"), attribute.Int("rank", 2)),
	))
	chat.AddEvent("gen_ai.content.prompt", trace.WithTimestamp(at(11*ms)), trace.WithAttributes(attribute.String("gen_ai.prompt", "What is FoundationDB?")))
	chat.AddEvent("gen_ai.content.completion", trace.WithTimestamp(at(39*ms)), trace.WithAttributes(attribute.String("gen_ai.completion", "A distributed ordered KV store."), attribute.Int64("gen_ai.choice.index", 0)))
	chat.SetStatus(codes.Error, "rate limited")
	chat.End(trace.WithTimestamp(at(40 * ms)))

	// Same start as chat: Trace orders the tie by span ID.
	_, tool := tracer.Start(rootCtx, "execute_tool search", trace.WithTimestamp(at(10*ms)), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", "search"),
		attribute.ByteSlice("custom.digest", []byte{0xde, 0xad, 0xbe, 0xef}),
	))
	tool.SetStatus(codes.Ok, "")
	tool.End(trace.WithTimestamp(at(20 * ms)))
	agent.AddEvent("agent.plan", trace.WithTimestamp(at(1*ms)))
	agent.SetStatus(codes.Ok, "")
	agent.End(trace.WithTimestamp(at(50 * ms)))

	// A later, separate trace links back to the agent span.
	_, embed := tracer.Start(context.Background(), "embeddings text-embedding-3-small", trace.WithTimestamp(at(time.Second)), trace.WithSpanKind(trace.SpanKindClient),
		trace.WithLinks(trace.Link{SpanContext: agent.SpanContext(), Attributes: []attribute.KeyValue{attribute.String("link.reason", "follow-up")}}),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "embeddings"),
			attribute.String("gen_ai.request.model", "text-embedding-3-small"),
			attribute.String("gen_ai.response.id", responseID+"-embed"),
			attribute.Bool("gen_ai.request.stream", false),
			attribute.Float64("gen_ai.request.temperature", 0.7),
		))
	embed.End(trace.WithTimestamp(at(time.Second + 5*ms)))

	t1, t2 := agent.SpanContext().TraceID().String(), embed.SpanContext().TraceID().String()
	agentID, chatID, toolID, embedID := agent.SpanContext().SpanID().String(), chat.SpanContext().SpanID().String(), tool.SpanContext().SpanID().String(), embed.SpanContext().SpanID().String()
	if t1 == t2 || chat.SpanContext().TraceID().String() != t1 || tool.SpanContext().TraceID().String() != t1 {
		t.Fatal("unexpected SDK trace topology")
	}
	hit := func(tr string, d time.Duration) Hit { return Hit{Trace: tr, Latest: ns(d)} }
	span := func(k, v string) Attribute { return Attribute{Scope: "span", Key: k, Type: "string", Value: v} }
	both := []Hit{hit(t2, time.Second), hit(t1, 10*ms)}

	queries := []struct {
		name string
		a    Attribute
		want []Hit
	}{
		{"operation chat", span("gen_ai.operation.name", "chat"), []Hit{hit(t1, 10*ms)}},
		{"operation invoke_agent", span("gen_ai.operation.name", "invoke_agent"), []Hit{hit(t1, 0)}},
		{"operation execute_tool", span("gen_ai.operation.name", "execute_tool"), []Hit{hit(t1, 10*ms)}},
		{"operation embeddings", span("gen_ai.operation.name", "embeddings"), []Hit{hit(t2, time.Second)}},
		{"operation absent", span("gen_ai.operation.name", "text_completion"), nil},
		{"request model on parent and child", span("gen_ai.request.model", "gpt-4o"), []Hit{hit(t1, 10*ms)}},
		{"request model embeddings", span("gen_ai.request.model", "text-embedding-3-small"), []Hit{hit(t2, time.Second)}},
		{"response id", span("gen_ai.response.id", responseID), []Hit{hit(t1, 10*ms)}},
		{"response id embeddings", span("gen_ai.response.id", responseID+"-embed"), []Hit{hit(t2, time.Second)}},
		{"response id absent", span("gen_ai.response.id", responseID+"-missing"), nil},
		{"int attribute", Attribute{"span", "gen_ai.usage.input_tokens", "int", "1234"}, []Hit{hit(t1, 10*ms)}},
		{"int attribute is not a string", span("gen_ai.usage.input_tokens", "1234"), nil},
		{"bool true", CanonicalAttribute("span", "gen_ai.request.stream", exBool(true)), []Hit{hit(t1, 10*ms)}},
		{"bool false", CanonicalAttribute("span", "gen_ai.request.stream", exBool(false)), []Hit{hit(t2, time.Second)}},
		{"float shared by both traces", CanonicalAttribute("span", "gen_ai.request.temperature", exDouble(0.7)), both},
		{"float NaN", CanonicalAttribute("span", "custom.score", exDouble(math.NaN())), []Hit{hit(t1, 10*ms)}},
		{"float +Inf", CanonicalAttribute("span", "custom.ceiling", exDouble(math.Inf(1))), []Hit{hit(t1, 10*ms)}},
		{"float -Inf", CanonicalAttribute("span", "custom.floor", exDouble(math.Inf(-1))), []Hit{hit(t1, 10*ms)}},
		{"string array", CanonicalAttribute("span", "gen_ai.response.finish_reasons", exArray(exStr("stop"), exStr("length"))), []Hit{hit(t1, 10*ms)}},
		{"string array is ordered", CanonicalAttribute("span", "gen_ai.response.finish_reasons", exArray(exStr("length"), exStr("stop"))), nil},
		{"int array", CanonicalAttribute("span", "custom.shard_ids", exArray(exInt(3), exInt(1), exInt(4))), []Hit{hit(t1, 10*ms)}},
		{"map", CanonicalAttribute("span", "custom.labels", exKvlist(exKV("rank", exInt(2)), exKV("tier", exStr("gold")))), []Hit{hit(t1, 10*ms)}},
		{"bytes", CanonicalAttribute("span", "custom.digest", exBytes([]byte{0xde, 0xad, 0xbe, 0xef})), []Hit{hit(t1, 10*ms)}},
		{"span attribute is not a resource attribute", Attribute{"resource", "gen_ai.operation.name", "string", "chat"}, nil},
		{"resource string", Attribute{"resource", "service.name", "string", "genai-gateway"}, both},
		{"resource int", Attribute{"resource", "service.shard", "int", "3"}, both},
		{"resource bool", CanonicalAttribute("resource", "service.canary", exBool(true)), both},
		{"resource array", CanonicalAttribute("resource", "service.regions", exArray(exStr("us-east-1"), exStr("eu-west-1"))), both},
		{"resource value absent", Attribute{"resource", "service.name", "string", "other"}, nil},
		{"scope string", Attribute{"scope", "scope.team", "string", "ml-platform"}, both},
		{"scope bool", CanonicalAttribute("scope", "scope.experimental", exBool(true)), both},
		{"scope float", CanonicalAttribute("scope", "scope.sample_ratio", exDouble(0.25)), both},
		{"event attributes are not indexed", span("gen_ai.prompt", "What is FoundationDB?"), nil},
		{"link attributes are not indexed", span("link.reason", "follow-up"), nil},
	}
	for _, q := range queries {
		t.Run("query "+q.name, func(t *testing.T) { requireHits(t, ctx, e, q.a, q.want...) })
	}

	requireTrace(t, ctx, e, t1, append([]SpanHit{{agentID, ns(0)}}, sortedSpanHits(SpanHit{chatID, ns(10 * ms)}, SpanHit{toolID, ns(10 * ms)})...)...)
	requireTrace(t, ctx, e, t2, SpanHit{embedID, ns(time.Second)})
	requireTrace(t, ctx, e, hex.EncodeToString(exRandomID(t, 16)))

	requireSpan(t, ctx, e, agentID, SpanMatch{t1, agentID, ns(0)})
	requireSpan(t, ctx, e, chatID, SpanMatch{t1, chatID, ns(10 * ms)})
	requireSpan(t, ctx, e, toolID, SpanMatch{t1, toolID, ns(10 * ms)})
	requireSpan(t, ctx, e, embedID, SpanMatch{t2, embedID, ns(time.Second)})
	requireSpan(t, ctx, e, hex.EncodeToString(exRandomID(t, 8)))

	window := func(from, to int64) TimeRange { return TimeRange{Start: from, End: to} }
	requireTime(t, ctx, e, window(ns(0), ns(10*ms)), hit(t1, 0))             // end is exclusive
	requireTime(t, ctx, e, window(ns(0), ns(10*ms)+1), hit(t1, 10*ms))       // latest start in window
	requireTime(t, ctx, e, window(ns(0), ns(time.Second)), hit(t1, 10*ms))   // excludes t2's start
	requireTime(t, ctx, e, window(ns(0), ns(time.Second)+1), both...)        // includes t2's start
	requireTime(t, ctx, e, window(ns(10*ms), ns(time.Second)+1), both...)    // start is inclusive
	requireTime(t, ctx, e, window(ns(0)+1, ns(10*ms)), nil...)               // gap between spans
	requireTime(t, ctx, e, window(ns(10*ms)+1, ns(time.Second)), nil...)     // gap between traces
	requireTime(t, ctx, e, window(ns(time.Second), ns(time.Second)), nil...) // empty window
	requireTime(t, ctx, e, window(ns(-time.Hour), ns(0)), nil...)            // before everything
	requireTime(t, ctx, e, window(math.MinInt64, math.MaxInt64), both...)    // unbounded
	if _, err := e.Time(ctx, window(ns(time.Second), ns(0))); err == nil {
		t.Fatal("inverted time window accepted")
	} else {
		requireOtelKind(t, err, "config")
	}

	ended := rec.Ended()
	if len(ended) != 4 {
		t.Fatalf("recorder saw %d spans", len(ended))
	}
	requireRecorded(t, ctx, e, ended)

	// Spot-check the restored chat span directly against the SDK inputs.
	got, err := e.LoadSpanOTLP(ctx, t1, chatID)
	if err != nil {
		t.Fatal(err)
	}
	sp := got.ScopeSpans[0].Spans[0]
	agentSID := agent.SpanContext().SpanID()
	if sp.Name != "chat gpt-4o" || sp.Kind != tracepb.Span_SPAN_KIND_CLIENT || !reflect.DeepEqual(sp.ParentSpanId, agentSID[:]) ||
		sp.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || sp.Status.GetMessage() != "rate limited" ||
		sp.StartTimeUnixNano != uint64(ns(10*ms)) || sp.EndTimeUnixNano != uint64(ns(40*ms)) {
		t.Fatalf("restored chat span: %v", sp)
	}
	if len(sp.Events) != 2 || sp.Events[0].Name != "gen_ai.content.prompt" || sp.Events[1].Name != "gen_ai.content.completion" || exFind(sp.Events[1].Attributes, "gen_ai.choice.index").GetIntValue() != 0 {
		t.Fatalf("restored chat events: %v", sp.Events)
	}
	if v := exFind(sp.Attributes, "custom.score"); v == nil || !math.IsNaN(v.GetDoubleValue()) {
		t.Fatalf("NaN attribute not restored as double: %v", v)
	}
	if v := exFind(sp.Attributes, "gen_ai.request.temperature"); v.GetDoubleValue() != 0.7 {
		t.Fatalf("temperature not restored as double: %v", v)
	}
	if v := exFind(sp.Attributes, "gen_ai.request.stream"); v == nil || v.GetValue() == nil || !v.GetBoolValue() {
		t.Fatalf("stream not restored as bool: %v", v)
	}
	if v := exFind(sp.Attributes, "gen_ai.response.finish_reasons"); len(v.GetArrayValue().GetValues()) != 2 {
		t.Fatalf("finish reasons not restored as array: %v", v)
	}
	if v := exFind(sp.Attributes, "custom.labels"); len(v.GetKvlistValue().GetValues()) != 2 {
		t.Fatalf("labels not restored as map: %v", v)
	}
	if v := exFind(got.Resource.Attributes, "service.canary"); !v.GetBoolValue() {
		t.Fatalf("resource bool not restored: %v", got.Resource)
	}
	if sc := got.ScopeSpans[0].Scope; sc.Name != "fdb.dev/layers/otel/exporter_test" || sc.Version != "1.4.0" || !exFind(sc.Attributes, "scope.experimental").GetBoolValue() {
		t.Fatalf("restored scope: %v", sc)
	}
	embedded, err := e.LoadSpanOTLP(ctx, t2, embedID)
	if err != nil {
		t.Fatal(err)
	}
	links := embedded.ScopeSpans[0].Spans[0].Links
	agentTID := agent.SpanContext().TraceID()
	if len(links) != 1 || !reflect.DeepEqual(links[0].TraceId, agentTID[:]) || !reflect.DeepEqual(links[0].SpanId, agentSID[:]) || exFind(links[0].Attributes, "link.reason").GetStringValue() != "follow-up" {
		t.Fatalf("restored links: %v", links)
	}

	// TracerProvider shutdown shuts the exporter down too.
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	tpOpen = false
	_, err = e.Query(ctx, span("gen_ai.operation.name", "chat"))
	requireOtelKind(t, err, "operation")
}

func testExporterBatchSpanProcessor(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 2, 3 })
	rec := tracetest.NewSpanRecorder()
	bsp := sdktrace.NewBatchSpanProcessor(e, sdktrace.WithMaxExportBatchSize(5), sdktrace.WithBatchTimeout(time.Hour))
	res := resource.NewSchemaless(attribute.String("service.name", "batch-agent"), attribute.Float64("service.version_ratio", 1.5))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(bsp), sdktrace.WithSpanProcessor(rec), sdktrace.WithResource(res))
	tpOpen := true
	t.Cleanup(func() {
		if tpOpen {
			_ = tp.Shutdown(context.Background())
		}
	})
	tracer := tp.Tracer("batch", trace.WithInstrumentationAttributes(attribute.String("scope.kind", "batch")))
	base := time.Unix(1_760_000_000, 0)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }

	const tools = 11
	rootCtx, root := tracer.Start(context.Background(), "invoke_agent batcher", trace.WithTimestamp(at(0)), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.request.model", "claude-sonnet"),
	))
	var want []SpanHit
	var calls []trace.Span
	for i := 1; i <= tools; i++ {
		_, s := tracer.Start(rootCtx, fmt.Sprintf("execute_tool step-%d", i), trace.WithTimestamp(at(i)), trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("gen_ai.tool.call.id", fmt.Sprintf("call-%02d", i)),
			attribute.Int("gen_ai.tool.step", i),
			attribute.Bool("gen_ai.tool.even", i%2 == 0),
		))
		s.AddEvent("tool.result", trace.WithTimestamp(at(i)), trace.WithAttributes(attribute.Int("bytes", i*10)))
		if i == tools {
			s.SetStatus(codes.Error, "tool failed")
		}
		s.End(trace.WithTimestamp(at(i + 1)))
		calls = append(calls, s)
		want = append(want, SpanHit{s.SpanContext().SpanID().String(), at(i).UnixNano()})
	}
	root.End(trace.WithTimestamp(at(tools + 2)))
	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	tr := root.SpanContext().TraceID().String()
	want = append([]SpanHit{{root.SpanContext().SpanID().String(), at(0).UnixNano()}}, want...)
	requireTrace(t, ctx, e, tr, want...)
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "execute_tool"}, Hit{tr, at(tools).UnixNano()})
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.request.model", "string", "claude-sonnet"}, Hit{tr, at(0).UnixNano()})
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.tool.call.id", "string", "call-07"}, Hit{tr, at(7).UnixNano()})
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.tool.step", "int", "3"}, Hit{tr, at(3).UnixNano()})
	requireHits(t, ctx, e, CanonicalAttribute("span", "gen_ai.tool.even", exBool(true)), Hit{tr, at(10).UnixNano()})
	requireHits(t, ctx, e, CanonicalAttribute("resource", "service.version_ratio", exDouble(1.5)), Hit{tr, at(tools).UnixNano()})
	requireHits(t, ctx, e, Attribute{"scope", "scope.kind", "string", "batch"}, Hit{tr, at(tools).UnixNano()})
	for i, s := range calls {
		requireSpan(t, ctx, e, s.SpanContext().SpanID().String(), SpanMatch{tr, s.SpanContext().SpanID().String(), at(i + 1).UnixNano()})
	}
	requireTime(t, ctx, e, TimeRange{at(3).UnixNano(), at(6).UnixNano()}, Hit{tr, at(5).UnixNano()})
	if len(rec.Ended()) != tools+1 {
		t.Fatalf("recorder saw %d spans", len(rec.Ended()))
	}
	requireRecorded(t, ctx, e, rec.Ended())

	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	tpOpen = false
	_, err := e.Trace(ctx, tr)
	requireOtelKind(t, err, "operation")
}

func testExporterOTLPFidelity(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	traceID, spanA, spanB := exRandomID(t, 16), exRandomID(t, 8), exRandomID(t, 8)
	tr, idA, idB := hex.EncodeToString(traceID), hex.EncodeToString(spanA), hex.EncodeToString(spanB)
	const start = int64(1_770_000_000_000_000_000)
	longKey := strings.Repeat("k", maxIndexedKeyBytes+1)
	hugeValue := strings.Repeat("v", maxIndexedValueBytes+1)
	nested := exKvlist(exKV("list", exArray(exInt(1), exDouble(2.5), exBool(false))), exKV("who", exStr("me")), exKV("list", exStr("again")))

	rs := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			exKV("service.name", exStr("fidelity-svc")),
			exKV("res.dup", exInt(1)),
			exKV("res.ratio", exDouble(0.25)),
			exKV("res.dup", exStr("two")),
		}, DroppedAttributesCount: 1},
		SchemaUrl: "https://example.test/resource",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope:     &commonpb.InstrumentationScope{Name: "fidelity", Version: "9", Attributes: []*commonpb.KeyValue{exKV("scope.flag", exBool(false)), exKV("scope.blob", exBytes([]byte{1, 2, 3}))}},
			SchemaUrl: "https://example.test/scope",
			Spans: []*tracepb.Span{{
				TraceId: traceID, SpanId: spanA, TraceState: "vendor=x", Name: "chat fidelity", Kind: tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: uint64(start), EndTimeUnixNano: uint64(start + 1000), Flags: 0x101,
				Attributes: []*commonpb.KeyValue{
					exKV("gen_ai.operation.name", exStr("chat")),
					exKV("gen_ai.response.id", exStr("resp-fidelity")),
					exKV("dup", exInt(1)),
					exKV("flag", exBool(true)),
					exKV("nested", nested),
					exKV("nanv", exDouble(math.NaN())),
					exKV("dup", exInt(2)),
					exKV(longKey, exStr("v")),
					exKV("huge", exStr(hugeValue)),
					exKV("min", exInt(math.MinInt64)),
					exKV("empty", &commonpb.AnyValue{}),
				},
				DroppedAttributesCount: 2,
				Events:                 []*tracepb.Span_Event{{TimeUnixNano: uint64(start + 10), Name: "evt", Attributes: []*commonpb.KeyValue{exKV("ratio", exDouble(1.5))}}},
				Links:                  []*tracepb.Span_Link{{TraceId: exRandomID(t, 16), SpanId: exRandomID(t, 8), Attributes: []*commonpb.KeyValue{exKV("kind", exBool(true))}}},
				Status:                 &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
			}},
		}},
	}
	// Unknown wrapper fields are preserved alongside the fidelity payload.
	rs.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 4242, protowire.VarintType), 7))
	rs.ScopeSpans[0].ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 4343, protowire.BytesType), "scope-extra"))
	plain := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("plain-svc")), exKV("service.shard", exInt(4))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "plain"}, Spans: []*tracepb.Span{{
			TraceId: traceID, SpanId: spanB, ParentSpanId: spanA, Name: "plain child", StartTimeUnixNano: uint64(start + 100), EndTimeUnixNano: uint64(start + 200),
			Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("embeddings")), exKV("gen_ai.usage.input_tokens", exInt(9))},
		}}}},
	}
	original := proto.Clone(rs).(*tracepb.ResourceSpans)

	stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs, plain})
	if err != nil {
		t.Fatal(err)
	}
	if stats.LogicalBytes <= int64(len(hugeValue)) {
		t.Fatalf("logical bytes %d omit the stored payload", stats.LogicalBytes)
	}
	stats.LogicalBytes = 0
	if want := (ExportStats{ReceivedSpans: 2, Spans: 2, Batches: 1, NormalizedSpans: 1, UnindexedAttributes: 2}); stats != want {
		t.Fatalf("stats %+v want %+v", stats, want)
	}
	if !proto.Equal(rs, original) {
		t.Fatal("export mutated its input")
	}

	hitA, hitB := Hit{tr, start}, Hit{tr, start + 100}
	span := func(k string, v *commonpb.AnyValue) Attribute { return CanonicalAttribute("span", k, v) }
	for _, q := range []struct {
		a    Attribute
		want []Hit
	}{
		{span("gen_ai.operation.name", exStr("chat")), []Hit{hitA}},
		{span("gen_ai.operation.name", exStr("embeddings")), []Hit{hitB}},
		{span("gen_ai.response.id", exStr("resp-fidelity")), []Hit{hitA}},
		{span("dup", exInt(2)), []Hit{hitA}},
		{span("dup", exInt(1)), nil},
		{span("flag", exBool(true)), []Hit{hitA}},
		{span("nanv", exDouble(math.NaN())), []Hit{hitA}},
		{span("min", exInt(math.MinInt64)), []Hit{hitA}},
		{span("nested", nested), []Hit{hitA}},
		{Attribute{"span", "nested", "string", `{"list":[1,2.5,false],"who":"me","list":"again"}`}, []Hit{hitA}},
		{Attribute{"span", "empty", "string", ""}, []Hit{hitA}},
		{span(longKey, exStr("v")), nil},
		{span("huge", exStr(hugeValue)), nil},
		{span("ratio", exDouble(1.5)), nil},
		{span("gen_ai.usage.input_tokens", exInt(9)), []Hit{hitB}},
		{CanonicalAttribute("resource", "service.name", exStr("fidelity-svc")), []Hit{hitA}},
		{CanonicalAttribute("resource", "service.name", exStr("plain-svc")), []Hit{hitB}},
		{CanonicalAttribute("resource", "service.shard", exInt(4)), []Hit{hitB}},
		{CanonicalAttribute("resource", "res.dup", exStr("two")), []Hit{hitA}},
		{CanonicalAttribute("resource", "res.dup", exInt(1)), nil},
		{CanonicalAttribute("resource", "res.ratio", exDouble(0.25)), []Hit{hitA}},
		{CanonicalAttribute("scope", "scope.flag", exBool(false)), []Hit{hitA}},
		{Attribute{"scope", "scope.blob", "string", "AQID"}, []Hit{hitA}},
	} {
		requireHits(t, ctx, e, q.a, q.want...)
	}
	requireTrace(t, ctx, e, tr, SpanHit{idA, start}, SpanHit{idB, start + 100})

	got, err := e.LoadSpanOTLP(ctx, tr, idA)
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "normalized span", got, exSingle(original, original.ScopeSpans[0], original.ScopeSpans[0].Spans[0]))
	attrs := got.ScopeSpans[0].Spans[0].Attributes
	if len(attrs) != 11 || attrs[2].Value.GetIntValue() != 1 || attrs[6].Value.GetIntValue() != 2 || attrs[7].Key != longKey || attrs[8].Value.GetStringValue() != hugeValue {
		t.Fatalf("restored span attributes lost duplicates or unindexed values")
	}
	if ra := got.Resource.Attributes; len(ra) != 4 || ra[1].Value.GetIntValue() != 1 || ra[3].Value.GetStringValue() != "two" {
		t.Fatalf("restored resource attributes: %v", ra)
	}
	if _, ok := got.ScopeSpans[0].Scope.Attributes[1].Value.GetValue().(*commonpb.AnyValue_BytesValue); !ok {
		t.Fatalf("restored scope attribute type: %v", got.ScopeSpans[0].Scope.Attributes[1])
	}
	got, err = e.LoadSpanOTLP(ctx, tr, idB)
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "plain span", got, exSingle(plain, plain.ScopeSpans[0], plain.ScopeSpans[0].Spans[0]))

	// SDK ReadOnlySpans may carry duplicate keys; ExportSpans keeps them too.
	stubSC := exSpanContext(t, true, false, "")
	stub := tracetest.SpanStub{
		Name: "stub chat", SpanContext: stubSC, StartTime: time.Unix(0, start+200), EndTime: time.Unix(0, start+300),
		Attributes:           []attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat"), attribute.Int64("dup", 1), attribute.String("dup", "last"), attribute.Float64Slice("scores", []float64{math.Inf(1), 0.5})},
		Resource:             resource.NewSchemaless(attribute.String("service.name", "stub-svc")),
		InstrumentationScope: instrumentation.Scope{Name: "stub"},
	}.Snapshot()
	if err := e.ExportSpans(ctx, []sdktrace.ReadOnlySpan{stub}); err != nil {
		t.Fatal(err)
	}
	stubTrace, stubID := stubSC.TraceID().String(), stubSC.SpanID().String()
	stubHit := Hit{stubTrace, start + 200}
	requireHits(t, ctx, e, Attribute{"span", "dup", "string", "last"}, stubHit)
	requireHits(t, ctx, e, Attribute{"span", "dup", "int", "1"}) // superseded in both spans
	requireHits(t, ctx, e, Attribute{"span", "scores", "string", `["+Inf",0.5]`}, stubHit)
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, stubHit, hitA)
	requireRecorded(t, ctx, e, []sdktrace.ReadOnlySpan{stub})
	got, err = e.LoadSpanOTLP(ctx, stubTrace, stubID)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.ScopeSpans[0].Spans[0].Attributes; len(a) != 4 || a[1].Value.GetIntValue() != 1 || a[2].Value.GetStringValue() != "last" {
		t.Fatalf("stub duplicates not restored: %v", a)
	}

	// Lookups of absent or malformed identities.
	missing := hex.EncodeToString(exRandomID(t, 8))
	_, err = e.LoadSpanOTLP(ctx, tr, missing)
	var notFound *SpanNotFoundError
	if !errors.As(err, &notFound) || notFound.Trace != tr || notFound.Span != missing {
		t.Fatalf("want SpanNotFoundError for %s/%s, got %T: %v", tr, missing, err, err)
	}
	requireOtelKind(t, err, "operation")
	for _, ids := range [][2]string{
		{strings.ToUpper(tr), idA},
		{tr, strings.ToUpper(idA)},
		{tr[:30], idA},
		{tr, idA + "00"},
		{strings.Repeat("0", 32), idA},
		{tr, strings.Repeat("0", 16)},
		{"zz" + tr[2:], idA},
		{"", ""},
	} {
		rs, err := e.LoadSpanOTLP(ctx, ids[0], ids[1])
		if rs != nil {
			t.Fatalf("load %q/%q returned a span", ids[0], ids[1])
		}
		requireOtelKind(t, err, "config")
	}

	// An inadmissible span fails the whole call without partial writes.
	goodID := exRandomID(t, 8)
	good := &tracepb.Span{TraceId: traceID, SpanId: goodID, StartTimeUnixNano: uint64(start + 500), Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("partial"))}}
	for name, bad := range map[string]*tracepb.Span{
		"zero span id":       {TraceId: traceID, SpanId: make([]byte, 8), StartTimeUnixNano: 1},
		"short trace id":     {TraceId: traceID[:15], SpanId: exRandomID(t, 8), StartTimeUnixNano: 1},
		"zero trace id":      {TraceId: make([]byte, 16), SpanId: exRandomID(t, 8), StartTimeUnixNano: 1},
		"start beyond int64": {TraceId: traceID, SpanId: exRandomID(t, 8), StartTimeUnixNano: math.MaxUint64},
	} {
		_, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{
			{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{good}}}},
			{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{bad}}}},
		})
		if err == nil {
			t.Fatalf("%s: inadmissible span accepted", name)
		}
		requireOtelKind(t, err, "admission")
		requireSpan(t, ctx, e, hex.EncodeToString(goodID))
		requireHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "partial"})
	}
	requireTrace(t, ctx, e, tr, SpanHit{idA, start}, SpanHit{idB, start + 100})
}

func testExporterBatching(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 2, 3 })
	const start = int64(1_780_000_000_000_000_000)
	t1, t2 := exRandomID(t, 16), exRandomID(t, 16)
	ids := make([][]byte, 5)
	for i := range ids {
		ids[i] = exRandomID(t, 8)
	}
	mk := func(trace, span []byte, at int64, name, resp string) *tracepb.Span {
		return &tracepb.Span{TraceId: trace, SpanId: span, Name: name, StartTimeUnixNano: uint64(at), EndTimeUnixNano: uint64(at + 5), Attributes: []*commonpb.KeyValue{
			exKV("gen_ai.operation.name", exStr("chat")),
			exKV("gen_ai.response.id", exStr(resp)),
			exKV("gen_ai.usage.output_tokens", exInt(at-start)),
		}}
	}
	rsA := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("batch-a"))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "batch"}, Spans: []*tracepb.Span{
			mk(t1, ids[0], start, "s0", "resp-0"),
			mk(t1, ids[1], start+10, "s1 original", "resp-old"),
			mk(t1, ids[2], start+20, "s2", "resp-2"),
		}}},
	}
	rsB := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("batch-b"))}},
		ScopeSpans: []*tracepb.ScopeSpans{nil, {Scope: &commonpb.InstrumentationScope{Name: "batch"}, Spans: []*tracepb.Span{
			mk(t2, ids[3], start+30, "s3", "resp-3"),
			nil,
			mk(t1, ids[1], start+15, "s1 replacement", "resp-new"), // same identity as s1: later wins
			mk(t2, ids[4], start+40, "s4", "resp-4"),
		}}},
	}
	input := []*tracepb.ResourceSpans{rsA, nil, rsB}
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LogicalBytes <= 0 {
		t.Fatalf("logical bytes %d", stats.LogicalBytes)
	}
	logical := stats.LogicalBytes
	stats.LogicalBytes = 0
	if want := (ExportStats{ReceivedSpans: 6, DuplicateSpans: 1, Spans: 5, Batches: 3}); stats != want {
		t.Fatalf("stats %+v want %+v", stats, want)
	}

	tr1, tr2 := hex.EncodeToString(t1), hex.EncodeToString(t2)
	id := func(i int) string { return hex.EncodeToString(ids[i]) }
	resp := func(v string) Attribute { return Attribute{"span", "gen_ai.response.id", "string", v} }
	type snapshot struct {
		traces  [2][]SpanHit
		queries []QueryResult
		time    QueryResult
	}
	queries := []Attribute{resp("resp-old"), resp("resp-new"), {"span", "gen_ai.operation.name", "string", "chat"}, {"resource", "service.name", "string", "batch-a"}, {"resource", "service.name", "string", "batch-b"}, {"span", "gen_ai.usage.output_tokens", "int", "15"}}
	take := func() snapshot {
		t.Helper()
		var s snapshot
		var err error
		if s.traces[0], err = e.Trace(ctx, tr1); err != nil {
			t.Fatal(err)
		}
		if s.traces[1], err = e.Trace(ctx, tr2); err != nil {
			t.Fatal(err)
		}
		for _, q := range queries {
			r, err := e.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			s.queries = append(s.queries, QueryResult{Hits: r.Hits, Candidates: r.Candidates})
		}
		r, err := e.Time(ctx, TimeRange{start, start + 41})
		if err != nil {
			t.Fatal(err)
		}
		s.time = QueryResult{Hits: r.Hits, Candidates: r.Candidates}
		return s
	}

	requireTrace(t, ctx, e, tr1, SpanHit{id(0), start}, SpanHit{id(1), start + 15}, SpanHit{id(2), start + 20})
	requireTrace(t, ctx, e, tr2, SpanHit{id(3), start + 30}, SpanHit{id(4), start + 40})
	requireHits(t, ctx, e, resp("resp-old"))
	requireHits(t, ctx, e, resp("resp-new"), Hit{tr1, start + 15})
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.usage.output_tokens", "int", "10"})
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.usage.output_tokens", "int", "15"}, Hit{tr1, start + 15})
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, Hit{tr2, start + 40}, Hit{tr1, start + 20})
	requireHits(t, ctx, e, Attribute{"resource", "service.name", "string", "batch-a"}, Hit{tr1, start + 20})
	requireHits(t, ctx, e, Attribute{"resource", "service.name", "string", "batch-b"}, Hit{tr2, start + 40}, Hit{tr1, start + 15})
	requireSpan(t, ctx, e, id(1), SpanMatch{tr1, id(1), start + 15})
	requireTime(t, ctx, e, TimeRange{start + 10, start + 15}) // the replaced start was never written
	requireTime(t, ctx, e, TimeRange{start + 10, start + 16}, Hit{tr1, start + 15})
	got, err := e.LoadSpanOTLP(ctx, tr1, id(1))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "deduplicated span", got, exSingle(rsB, rsB.ScopeSpans[1], rsB.ScopeSpans[1].Spans[2]))
	before := take()
	if q := before.queries[2]; q.Candidates != 5 {
		t.Fatalf("operation postings = %d, want one per stored span", q.Candidates)
	}

	// Re-exporting the same input is idempotent: same stats, records and postings.
	again, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if again.LogicalBytes != logical {
		t.Fatalf("re-export logical bytes %d, want %d", again.LogicalBytes, logical)
	}
	again.LogicalBytes = 0
	if again != stats {
		t.Fatalf("re-export stats %+v want %+v", again, stats)
	}
	if err := e.ConsumeTraces(ctx, input); err != nil {
		t.Fatal(err)
	}
	if after := take(); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-export changed state\nbefore %+v\n after %+v", before, after)
	}

	// Re-exporting one identity with new content replaces it in place.
	update := &tracepb.ResourceSpans{Resource: rsA.Resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: rsA.ScopeSpans[0].Scope, Spans: []*tracepb.Span{mk(t1, ids[0], start+1, "s0 v2", "resp-0-v2")}}}}
	if err := e.ConsumeTraces(ctx, []*tracepb.ResourceSpans{update}); err != nil {
		t.Fatal(err)
	}
	requireHits(t, ctx, e, resp("resp-0"))
	requireHits(t, ctx, e, resp("resp-0-v2"), Hit{tr1, start + 1})
	requireTrace(t, ctx, e, tr1, SpanHit{id(0), start + 1}, SpanHit{id(1), start + 15}, SpanHit{id(2), start + 20})
	requireTime(t, ctx, e, TimeRange{start, start + 1})
	if r, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"}); err != nil || r.Candidates != 5 {
		t.Fatalf("operation postings after update: %+v %v", r, err)
	}

	// Empty and all-nil inputs commit nothing.
	for name, in := range map[string][]*tracepb.ResourceSpans{
		"nil":       nil,
		"empty":     {},
		"nil items": {nil, {}, {ScopeSpans: []*tracepb.ScopeSpans{nil, {}, {Spans: []*tracepb.Span{nil}}}}},
	} {
		s, err := e.ExportResourceSpans(ctx, in)
		if err != nil || s != (ExportStats{}) {
			t.Fatalf("%s: stats %+v err %v", name, s, err)
		}
	}
	if err := e.ExportSpans(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// A canceled export writes nothing.
	canceled, cancelExport := context.WithCancel(ctx)
	cancelExport()
	fresh := &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{mk(t1, exRandomID(t, 8), start+99, "late", "resp-late")}}}}
	_, err = e.ExportResourceSpans(canceled, []*tracepb.ResourceSpans{fresh})
	requireOtelKind(t, err, "canceled")
	requireHits(t, ctx, e, resp("resp-late"))
}

func testExporterShutdown(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	traceID, spanID := exRandomID(t, 16), exRandomID(t, 8)
	tr, id := hex.EncodeToString(traceID), hex.EncodeToString(spanID)
	const start = int64(1_790_000_000_000_000_000)
	in := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: traceID, SpanId: spanID, StartTimeUnixNano: uint64(start), Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("chat"))}}}}}}}
	if _, err := e.ExportResourceSpans(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	requireShutDown := func(name string, err error) {
		t.Helper()
		requireOtelKind(t, err, "operation")
		if !strings.Contains(err.Error(), "shut down") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	stats, err := e.ExportResourceSpans(ctx, in)
	requireShutDown("ExportResourceSpans", err)
	if stats != (ExportStats{}) {
		t.Fatalf("rejected export reported stats %+v", stats)
	}
	requireShutDown("ExportResourceSpans empty", func() error { _, err := e.ExportResourceSpans(ctx, nil); return err }())
	requireShutDown("ConsumeTraces", e.ConsumeTraces(ctx, in))
	stub := tracetest.SpanStub{SpanContext: exSpanContext(t, true, false, ""), StartTime: time.Unix(0, start)}.Snapshot()
	requireShutDown("ExportSpans", e.ExportSpans(ctx, []sdktrace.ReadOnlySpan{stub}))
	q, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"})
	requireShutDown("Query", err)
	if q.Hits == nil || len(q.Hits) != 0 {
		t.Fatalf("rejected query hits %+v", q.Hits)
	}
	hits, err := e.Trace(ctx, tr)
	requireShutDown("Trace", err)
	if hits == nil || len(hits) != 0 {
		t.Fatalf("rejected trace %+v", hits)
	}
	matches, err := e.Span(ctx, id)
	requireShutDown("Span", err)
	if matches == nil || len(matches) != 0 {
		t.Fatalf("rejected span %+v", matches)
	}
	tq, err := e.Time(ctx, TimeRange{start, start + 1})
	requireShutDown("Time", err)
	if tq.Hits == nil || len(tq.Hits) != 0 {
		t.Fatalf("rejected time %+v", tq.Hits)
	}
	loaded, err := e.LoadSpanOTLP(ctx, tr, id)
	requireShutDown("LoadSpanOTLP", err)
	if loaded != nil {
		t.Fatal("rejected load returned a span")
	}

	// Committed data outlives the handle; reopening verifies the run manifest.
	reopened, err := NewExporter(ctx, e.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Shutdown(context.Background()) })
	requireTrace(t, ctx, reopened, tr, SpanHit{id, start})
	requireHits(t, ctx, reopened, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, Hit{tr, start})

	// Another tenant of the same run sees none of it.
	other := e.Config()
	other.Tenant++
	neighbor, err := NewExporter(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = neighbor.Shutdown(context.Background()) })
	requireTrace(t, ctx, neighbor, tr)
	requireSpan(t, ctx, neighbor, id)
	requireHits(t, ctx, neighbor, Attribute{"span", "gen_ai.operation.name", "string", "chat"})
	requireTime(t, ctx, neighbor, TimeRange{start, start + 1})
	if _, err := neighbor.LoadSpanOTLP(ctx, tr, id); err == nil {
		t.Fatal("neighbor tenant loaded another tenant's span")
	}

	// A layout change on an existing run is refused.
	mismatch := e.Config()
	mismatch.HashBytes = 16
	if x, err := NewExporter(ctx, mismatch); err == nil {
		_ = x.Shutdown(context.Background())
		t.Fatal("layout mismatch accepted")
	} else {
		requireOtelKind(t, err, "ownership")
	}
}

func testExporterConcurrent(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 3, 4 })
	const writers, perTrace = 4, 7
	const start = int64(1_800_000_000_000_000_000)
	type traceWant struct {
		id   string
		hits []SpanHit
	}
	wants := make([]traceWant, writers)
	inputs := make([][]*tracepb.ResourceSpans, writers)
	for w := range writers {
		traceID := exRandomID(t, 16)
		wants[w].id = hex.EncodeToString(traceID)
		ss := &tracepb.ScopeSpans{}
		for i := range perTrace {
			spanID := exRandomID(t, 8)
			at := start + int64(w*100+i)
			ss.Spans = append(ss.Spans, &tracepb.Span{TraceId: traceID, SpanId: spanID, StartTimeUnixNano: uint64(at), Attributes: []*commonpb.KeyValue{
				exKV("gen_ai.operation.name", exStr("chat")),
				exKV("writer", exInt(int64(w))),
				exKV("ratio", exDouble(float64(i)/2)),
			}})
			wants[w].hits = append(wants[w].hits, SpanHit{hex.EncodeToString(spanID), at})
		}
		sortSpanHits(wants[w].hits)
		inputs[w] = []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{ss}}}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*writers)
	for w := range writers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			stats, err := e.ExportResourceSpans(ctx, inputs[w])
			if err == nil && (stats.Spans != perTrace || stats.NormalizedSpans != perTrace) {
				err = fmt.Errorf("writer %d stats %+v", w, stats)
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var all, third []Hit
	for w, want := range wants {
		requireTrace(t, ctx, e, want.id, want.hits...)
		last := want.hits[len(want.hits)-1].Start
		requireHits(t, ctx, e, Attribute{"span", "writer", "int", fmt.Sprint(w)}, Hit{want.id, last})
		all = append(all, Hit{want.id, last})
		third = append(third, Hit{want.id, start + int64(w*100+3)}) // ratio 1.5 is span i=3
	}
	byLatest := func(h []Hit) { sort.Slice(h, func(i, j int) bool { return h[i].Latest > h[j].Latest }) }
	byLatest(all)
	byLatest(third)
	requireHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, all...)
	requireHits(t, ctx, e, CanonicalAttribute("span", "ratio", exDouble(1.5)), third...)
}

func TestExporterMessageDeltaIntegration(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) {
		c.EnableMessageDeltas = true
	})

	input := deltaConversation(10, "array")
	before := cloneDeltaSpans(input)
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Spans != 10 || stats.MessageDeltas.TotalMessages != 10*11+3*10 || stats.MessageDeltas.UniqueMessages != 2*10+3 {
		t.Fatalf("unexpected export/delta stats: %+v", stats)
	}
	if stats.MessageDeltas.SavedBytes <= 0 {
		t.Fatalf("expected positive saved bytes: %+v", stats.MessageDeltas)
	}
	for i, want := range before {
		sp := want.ScopeSpans[0].Spans[0]
		tr := hex.EncodeToString(sp.TraceId)
		id := hex.EncodeToString(sp.SpanId)
		got, err := e.LoadSpanOTLP(ctx, tr, id)
		if err != nil {
			t.Fatalf("turn %d LoadSpanOTLP: %v", i+1, err)
		}
		requireProtoEqual(t, fmt.Sprintf("turn %d reconstructed", i+1), got, want)
	}
}
