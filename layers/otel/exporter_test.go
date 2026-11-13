package otel

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

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
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

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
	// D2: no order, hash_bytes or posting_value_format; zlib-1 records.
	want := ExporterConfig{RecordCompression: "zlib-1", BulkIndexConflicts: true, BatchSize: 64, Workers: 8, MaxCandidates: 20000, MaxBytes: 4 << 20, K: 100, MaxInFlightTransactions: 32, TransactionTimeout: 10 * time.Second}
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
record_compression: none
bulk_index_conflicts: false
batch_size: 16
workers: 4
max_candidates: 500
max_bytes: 1048576
k: 25
max_inflight_transactions: 7
transaction_timeout: 45s
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
		{"max_inflight_transactions only", "max_inflight_transactions: 1024\n", func(c *ExporterConfig) { c.MaxInFlightTransactions = 1024 }},
		{"transaction_timeout only", "transaction_timeout: 2m30s\n", func(c *ExporterConfig) { c.TransactionTimeout = 150 * time.Second }},
		{"transaction_timeout zero", "transaction_timeout: 0s\n", func(c *ExporterConfig) { c.TransactionTimeout = 0 }},
		{"every field", full, func(c *ExporterConfig) {
			*c = ExporterConfig{ClusterFile: "/var/fdb/fdb.cluster", Run: "live_run_full", Tenant: 42, RecordCompression: "none", BulkIndexConflicts: false, BatchSize: 16, Workers: 4, MaxCandidates: 500, MaxBytes: 1 << 20, K: 25, MaxInFlightTransactions: 7, TransactionTimeout: 45 * time.Second}
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
		want.Tenant, want.RecordCompression, want.BulkIndexConflicts, want.MaxBytes, want.MaxInFlightTransactions = 9, "zlib-default", false, 12345, 5
		want.TransactionTimeout = 1500 * time.Millisecond
		wire, err := yaml.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wire), "transaction_timeout: 1.5s\n") {
			t.Fatalf("transaction_timeout is not a Go duration:\n%s", wire)
		}
		var got ExporterConfig
		if err := yaml.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip: got %+v want %+v\n%s", got, want, wire)
		}
	})

	// D2: the custom store's keys are refused, naming the removal, as are
	// unknown keys (the collector's config refuses them too).
	for doc, message := range map[string]string{"order: time\n": "order was removed", "hash_bytes: 8\n": "hash_bytes was removed", "posting_value_format: tuple\n": "posting_value_format was removed", "run: live_run_0001\nnot_a_field: 7\n": `unknown key "not_a_field"`} {
		t.Run("refuses "+strings.TrimSpace(doc), func(t *testing.T) {
			var got ExporterConfig
			err := yaml.Unmarshal([]byte(doc), &got)
			requireOtelKind(t, err, "config")
			if !strings.Contains(err.Error(), message) || !strings.Contains(err.Error(), "exporter YAML") {
				t.Fatalf("error %v lacks %q", err, message)
			}
			requireRemovedKey(t, err, doc)
		})
	}
	// YAML merge keys (<<) bring in their mappings' keys, through an alias or
	// inline: accepted and refused as the config's own keys are. An alias of
	// the whole config is checked too; a quoted "<<" is a plain key.
	t.Run("merge keys and aliases", func(t *testing.T) {
		var doc struct {
			Defaults yaml.Node      `yaml:"defaults"`
			Exporter ExporterConfig `yaml:"exporter"`
		}
		src := "defaults: &d {batch_size: 8, workers: 2}\nexporter:\n  <<: [*d, {tenant: 3}]\n  run: live_run_0001\n"
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatal(err)
		}
		want := DefaultExporterConfig()
		want.Run, want.BatchSize, want.Workers, want.Tenant = "live_run_0001", 8, 2, 3
		if doc.Exporter != want {
			t.Fatalf("merged config %+v, want %+v", doc.Exporter, want)
		}
		for src, message := range map[string]string{
			"defaults: &d {not_a_field: 1}\nexporter:\n  <<: *d\n":             `unknown key "not_a_field"`,
			"defaults: &d {order: time}\nexporter:\n  <<: [{tenant: 3}, *d]\n": "order was removed",
			"defaults: &d {run: live_run_0001, extra: 1}\nexporter: *d\n":      `unknown key "extra"`,
			"exporter:\n  \"<<\": {batch_size: 8}\n":                           `unknown key "<<"`,
		} {
			var config *ConfigError
			if err := yaml.Unmarshal([]byte(src), &doc); !errors.As(err, &config) || !strings.Contains(err.Error(), message) {
				t.Fatalf("%q: %v, want a config error: %s", src, err, message)
			}
		}
	})
	for _, doc := range []string{"batch_size: not-a-number\n", "tenant: [1, 2]\n", "bulk_index_conflicts: maybe\n", "max_bytes: {a: 1}\n", "max_inflight_transactions: many\n", "transaction_timeout: 10\n", "transaction_timeout: soon\n"} {
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
		{"explicit zero overrides nonzero default", `{"k":0,"max_inflight_transactions":0,"transaction_timeout":"0s"}`, func(c *ExporterConfig) {
			c.K, c.MaxInFlightTransactions, c.TransactionTimeout = 0, 0, 0
		}},
		{"transaction_timeout only", `{"transaction_timeout":"2m30s"}`, func(c *ExporterConfig) { c.TransactionTimeout = 150 * time.Second }},
		{"every field", `{"cluster_file":"/var/fdb/fdb.cluster","run":"live_run_full","tenant":42,"record_compression":"zlib-1","bulk_index_conflicts":false,"batch_size":16,"workers":4,"max_candidates":500,"max_bytes":1048576,"k":25,"max_inflight_transactions":64,"transaction_timeout":"45s"}`, func(c *ExporterConfig) {
			*c = ExporterConfig{ClusterFile: "/var/fdb/fdb.cluster", Run: "live_run_full", Tenant: 42, RecordCompression: "zlib-1", BulkIndexConflicts: false, BatchSize: 16, Workers: 4, MaxCandidates: 500, MaxBytes: 1 << 20, K: 25, MaxInFlightTransactions: 64, TransactionTimeout: 45 * time.Second}
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
		want.Tenant, want.RecordCompression, want.Workers, want.MaxInFlightTransactions = 7, "none", 3, 1
		want.TransactionTimeout = 1500 * time.Millisecond
		wire, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wire), `"transaction_timeout":"1.5s"`) || strings.Count(string(wire), "transaction_timeout") != 1 {
			t.Fatalf("transaction_timeout is not one Go duration string: %s", wire)
		}
		var got ExporterConfig
		if err := json.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip: got %+v want %+v\n%s", got, want, wire)
		}
	})

	for doc, message := range map[string]string{`{"order":"time"}`: "order was removed", `{"hash_bytes":8}`: "hash_bytes was removed", `{"posting_value_format":"tuple"}`: "posting_value_format was removed", `{"run":"live_run_0001","extra":true}`: `unknown key "extra"`} {
		t.Run("refuses "+doc, func(t *testing.T) {
			var got ExporterConfig
			err := json.Unmarshal([]byte(doc), &got)
			requireOtelKind(t, err, "config")
			if !strings.Contains(err.Error(), message) || !strings.Contains(err.Error(), "exporter JSON") {
				t.Fatalf("error %v lacks %q", err, message)
			}
			requireRemovedKey(t, err, doc)
		})
	}
	for _, doc := range []string{`{"batch_size":"64"}`, `{"max_bytes":1.5}`, `{"tenant":[1]}`, `{"bulk_index_conflicts":"yes"}`, `{"max_inflight_transactions":"32"}`, `{"transaction_timeout":10000000000}`, `{"transaction_timeout":"soon"}`} {
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
		{"max_inflight_transactions 1", func(c *ExporterConfig) { c.MaxInFlightTransactions = 1 }},
		{"max_inflight_transactions 1024", func(c *ExporterConfig) { c.MaxInFlightTransactions = 1024 }},
		{"transaction_timeout 1s", func(c *ExporterConfig) { c.TransactionTimeout = time.Second }},
		{"transaction_timeout 5m", func(c *ExporterConfig) { c.TransactionTimeout = 5 * time.Minute }},
		{"tenant 0", func(c *ExporterConfig) { c.Tenant = 0 }},
		{"last tenant", func(c *ExporterConfig) { c.Tenant = exporterTenants - 1 }},
		{"run 8 chars", func(c *ExporterConfig) { c.Run = "abcd_123" }},
		{"run 48 chars", func(c *ExporterConfig) { c.Run = strings.Repeat("a", 48) }},
		{"no compression", func(c *ExporterConfig) { c.RecordCompression = "none" }},
		{"zlib", func(c *ExporterConfig) { c.RecordCompression = "zlib-1" }},
		{"zlib default", func(c *ExporterConfig) { c.RecordCompression = "zlib-default" }},
		{"bulk conflicts off", func(c *ExporterConfig) { c.BulkIndexConflicts = false }},
		{"bulk conflicts without messages", func(c *ExporterConfig) { c.BulkIndexConflicts = true }},
		{"full text without bulk conflicts", func(c *ExporterConfig) { c.EnableMessageFullText = true }},
		{"bulk conflicts with full text", func(c *ExporterConfig) { c.BulkIndexConflicts, c.EnableMessageFullText = true, true }},
		{"bulk conflicts with deltas", func(c *ExporterConfig) { c.BulkIndexConflicts, c.EnableMessageDeltas = true, true }},
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
		{"max_inflight_transactions 0", func(c *ExporterConfig) { c.MaxInFlightTransactions = 0 }, "max_inflight_transactions must be 1..1024"},
		{"max_inflight_transactions negative", func(c *ExporterConfig) { c.MaxInFlightTransactions = -1 }, "max_inflight_transactions must be 1..1024"},
		{"max_inflight_transactions 1025", func(c *ExporterConfig) { c.MaxInFlightTransactions = 1025 }, "max_inflight_transactions must be 1..1024"},
		{"transaction_timeout 0", func(c *ExporterConfig) { c.TransactionTimeout = 0 }, "transaction_timeout must be 1s..5m0s"},
		{"transaction_timeout negative", func(c *ExporterConfig) { c.TransactionTimeout = -time.Second }, "transaction_timeout must be 1s..5m0s"},
		{"transaction_timeout 999ms", func(c *ExporterConfig) { c.TransactionTimeout = 999 * time.Millisecond }, "transaction_timeout must be 1s..5m0s"},
		{"transaction_timeout over 5m", func(c *ExporterConfig) { c.TransactionTimeout = 5*time.Minute + time.Millisecond }, "transaction_timeout must be 1s..5m0s"},
		{"negative tenant", func(c *ExporterConfig) { c.Tenant = -1 }, "tenant must be 0.."},
		{"tenant past range", func(c *ExporterConfig) { c.Tenant = exporterTenants }, "tenant must be 0.."},
		{"missing run", func(c *ExporterConfig) { c.Run = "" }, "run must be"},
		{"short run", func(c *ExporterConfig) { c.Run = "abc_123" }, "run must be"},
		{"long run", func(c *ExporterConfig) { c.Run = strings.Repeat("a", 49) }, "run must be"},
		{"uppercase run", func(c *ExporterConfig) { c.Run = "Live_Run_0001" }, "run must be"},
		{"punctuated run", func(c *ExporterConfig) { c.Run = "live-run-0001" }, "run must be"},
		{"k 0", func(c *ExporterConfig) { c.K = 0 }, "invalid corpus/query bounds"},
		{"k 10001", func(c *ExporterConfig) { c.K = 10001 }, "invalid corpus/query bounds"},
		{"max_candidates 0", func(c *ExporterConfig) { c.MaxCandidates = 0 }, "invalid corpus/query bounds"},
		{"max_bytes 0", func(c *ExporterConfig) { c.MaxBytes = 0 }, "invalid corpus/query bounds"},
		{"unknown compression", func(c *ExporterConfig) { c.RecordCompression = "gzip" }, "record_compression must be none, zlib-1 or zlib-default"},
		// D2: SQL and Java read the records; neither reads Brotli. Implicit none is gone too.
		{"brotli-0", func(c *ExporterConfig) { c.RecordCompression = "brotli-0" }, "record_compression must be none, zlib-1 or zlib-default"},
		{"brotli-4, the custom store's default", func(c *ExporterConfig) { c.RecordCompression = "brotli-4" }, "record_compression must be none, zlib-1 or zlib-default"},
		{"implicit no compression", func(c *ExporterConfig) { c.RecordCompression = "" }, "record_compression must be none, zlib-1 or zlib-default"},
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

// D1: typed values; bytes and JSON are reported, and no query accepts them.
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
		{"bool true", exBool(true), "bool", "true"},
		{"bool false", exBool(false), "bool", "false"},
		{"double", exDouble(0.7), "double", "0.7"},
		{"whole double", exDouble(3), "double", "3"},
		{"negative zero", exDouble(math.Copysign(0, -1)), "double", "-0"},
		{"large double", exDouble(1e21), "double", "1e+21"},
		{"small double", exDouble(1e-7), "double", "1e-07"},
		{"wide double", exDouble(123456789), "double", "1.23456789e+08"},
		{"NaN", exDouble(math.NaN()), "double", "NaN"},
		{"+Inf", exDouble(math.Inf(1)), "double", "+Inf"},
		{"-Inf", exDouble(math.Inf(-1)), "double", "-Inf"},
		{"bytes", exBytes([]byte{0, 1, 2, 0xff}), "bytes", "AAEC/w=="},
		{"empty bytes", exBytes(nil), "bytes", ""},
		{"nil value", nil, "json", "null"},
		{"unset value", &commonpb.AnyValue{}, "json", "null"},
		{"bool slice", exArray(exBool(true), exBool(false)), "json", `[true,false]`},
		{"int slice", exArray(exInt(1), exInt(-2), exInt(math.MaxInt64)), "json", `[1,-2,9223372036854775807]`},
		{"float slice", exArray(exDouble(1.5), exDouble(math.NaN()), exDouble(math.Inf(1)), exDouble(math.Inf(-1)), exDouble(1e21)), "json", `[1.5,"NaN","+Inf","-Inf",1e+21]`},
		{"string slice", exArray(exStr("a"), exStr(`b"c`), exStr("<&>"), exStr("line\nbreak")), "json", `["a","b\"c","<&>","line\nbreak"]`},
		{"invalid UTF-8 in slice", exArray(exStr("\xff")), "json", "[\"\ufffd\"]"},
		{"empty slice", exArray(), "json", `[]`},
		{"bytes in slice", exArray(exBytes([]byte{1, 2, 3})), "json", `["AQID"]`},
		{"unset in slice", exArray(&commonpb.AnyValue{}, nil), "json", `[null,null]`},
		{"nested slice and map", exArray(exStr("x"), exArray(exInt(1), exArray()), exKvlist(exKV("k", exBool(true)))), "json", `["x",[1,[]],{"k":true}]`},
		{"map", exKvlist(exKV("tier", exStr("gold")), exKV("rank", exInt(2))), "json", `{"tier":"gold","rank":2}`},
		{"map duplicate keys keep order", exKvlist(exKV("b", exInt(1)), exKV("a", exStr("x")), exKV("b", exInt(2))), "json", `{"b":1,"a":"x","b":2}`},
		{"empty map", exKvlist(), "json", `{}`},
		{"nested map", exKvlist(exKV("outer", exKvlist(exKV("inner", exArray(exDouble(0.25), exBool(false))))), exKV("empty", &commonpb.AnyValue{})), "json", `{"outer":{"inner":[0.25,false]},"empty":null}`},
		{"map key escaping", exKvlist(exKV(`k"<`, exStr("v"))), "json", `{"k\"<":"v"}`},
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
		if want := (Attribute{Scope: scope, Key: "gen_ai.request.temperature", Type: "double", Value: "0.7"}); got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
		got = CanonicalAttribute(scope, "gen_ai.usage.input_tokens", exInt(12))
		if want := (Attribute{Scope: scope, Key: "gen_ai.usage.input_tokens", Type: "int", Value: "12"}); got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// ResourceSpansFromReadOnly
// ---------------------------------------------------------------------------

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

	// Every converted attribute canonicalizes to the documented typed value (D1).
	canonical := map[string][2]string{
		"gen_ai.operation.name":      {"string", "chat"},
		"gen_ai.usage.input_tokens":  {"int", "42"},
		"gen_ai.request.stream":      {"bool", "true"},
		"gen_ai.request.temperature": {"double", "0.5"},
		"nan":                        {"double", "NaN"},
		"pinf":                       {"double", "+Inf"},
		"ninf":                       {"double", "-Inf"},
		"blob":                       {"bytes", "AAEC/w=="},
		"bools":                      {"json", `[true,false]`},
		"ints":                       {"json", `[1,-2]`},
		"plain_ints":                 {"json", `[7]`},
		"floats":                     {"json", `[1.5,"NaN","-Inf"]`},
		"strings":                    {"json", `["stop","length"]`},
		"nested":                     {"json", `["x",[1],{"k":true}]`},
		"labels":                     {"json", `{"a":2,"z":"1"}`},
		"empty":                      {"json", "null"},
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

// requireRemovedKey checks that err carries a RemovedConfigKeyError exactly
// when doc names one of the removed keys, and for that key.
func requireRemovedKey(t *testing.T, err error, doc string) {
	t.Helper()
	var removed *RemovedConfigKeyError
	key := ""
	for k := range removedExporterKeys {
		if strings.Contains(doc, k) {
			key = k
		}
	}
	if errors.As(err, &removed) != (key != "") || key != "" && removed.Key != key {
		t.Fatalf("%v: removed key %+v, want %q", err, removed, key)
	}
}
