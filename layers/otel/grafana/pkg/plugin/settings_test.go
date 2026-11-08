package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func TestParseSettingsDefaults(t *testing.T) {
	t.Parallel()
	s, err := ParseSettings(instanceSettings(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		ClusterString: "test:abc@127.0.0.1:4500", TLSMode: tlsModeOff, Database: "/OTEL/R_TEST", DefaultSchema: "T1", OTelMode: otelModeAuto,
		QueryTimeout: 30 * time.Second, TransactionTimeout: 5 * time.Second,
		SQLPageRows: 10_000, SQLPageBytes: 16 << 20, SQLMaxRows: 100_000, SQLStatementMemoryBytes: 256 << 20,
		TraceMaxSpans: 10_000, TraceMaxBytes: 64 << 20, SearchLimit: 20, SearchMaxCandidates: 20_000, SearchMaxBytes: 4 << 20, LogsLimit: 1_000,
	}
	if s.AllowedSchemas.restricted() {
		t.Fatalf("no allowedSchemas must admit every schema, got %+v", s.AllowedSchemas)
	}
	s.AllowedSchemas = schemaPolicy{}
	if !equalSettings(*s, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", *s, want)
	}
}

func equalSettings(a, b Settings) bool {
	return a.ClusterFilePath == b.ClusterFilePath && a.ClusterString == b.ClusterString && a.TLSMode == b.TLSMode &&
		a.Database == b.Database && a.DefaultSchema == b.DefaultSchema && a.OTelMode == b.OTelMode &&
		a.QueryTimeout == b.QueryTimeout && a.TransactionTimeout == b.TransactionTimeout &&
		a.SQLPageRows == b.SQLPageRows && a.SQLPageBytes == b.SQLPageBytes && a.SQLMaxRows == b.SQLMaxRows &&
		a.SQLStatementMemoryBytes == b.SQLStatementMemoryBytes && a.TraceMaxSpans == b.TraceMaxSpans &&
		a.TraceMaxBytes == b.TraceMaxBytes && a.SearchLimit == b.SearchLimit && a.SearchMaxCandidates == b.SearchMaxCandidates &&
		a.SearchMaxBytes == b.SearchMaxBytes && a.LogsLimit == b.LogsLimit && len(a.TransactionTags) == len(b.TransactionTags)
}

func TestParseSettingsValues(t *testing.T) {
	t.Parallel()
	in := instanceSettings(t, map[string]any{
		"clusterString": nil, "clusterFilePath": "/etc/foundationdb/fdb.cluster",
		"tlsMode": "cluster", "otelMode": "on", "allowedSchemas": []string{"T1", "T2", "X*"},
		"queryTimeoutSeconds": 12, "transactionTimeoutMs": "2500", "sqlPageRows": 5, "sqlPageBytes": 6,
		"sqlMaxRows": 7, "sqlStatementMemoryBytes": 8, "traceMaxSpans": 9, "traceMaxBytes": 10,
		"searchLimit": 1000, "searchMaxCandidates": 12, "searchMaxBytes": 13, "logsLimit": 14,
		"transactionTags": []string{" grafana ", "", "grafana", "dash"},
	})
	s, err := ParseSettings(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.ClusterFilePath != "/etc/foundationdb/fdb.cluster" || s.TLSMode != tlsModeCluster || s.OTelMode != otelModeOn {
		t.Fatalf("got %+v", s)
	}
	if s.QueryTimeout != 12*time.Second || s.TransactionTimeout != 2500*time.Millisecond || s.SQLPageRows != 5 ||
		s.SQLPageBytes != 6 || s.SQLMaxRows != 7 || s.SQLStatementMemoryBytes != 8 || s.TraceMaxSpans != 9 ||
		s.TraceMaxBytes != 10 || s.SearchLimit != 1000 || s.SearchMaxCandidates != 12 || s.SearchMaxBytes != 13 || s.LogsLimit != 14 {
		t.Fatalf("budgets: %+v", s)
	}
	if strings.Join(s.TransactionTags, ",") != "grafana,dash" {
		t.Fatalf("tags %q", s.TransactionTags)
	}
	for _, ok := range []string{"T1", "T2", "X", "X9"} {
		if !s.AllowedSchemas.allows(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	if s.AllowedSchemas.allows("T3") {
		t.Error("T3 admitted")
	}
}

func TestParseSettingsSecureJSON(t *testing.T) {
	t.Parallel()
	in := instanceSettings(t, map[string]any{"tlsMode": "config", "tlsCACert": testPKI(t).caPEM, "tlsVerifyPeers": "Check.Valid=1"})
	in.DecryptedSecureJSONData = map[string]string{"tlsClientCert": "C", "tlsClientKey": "K"}
	s, err := ParseSettings(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.TLSClientCert != "C" || s.TLSClientKey != "K" || s.TLSVerifyPeers != "Check.Valid=1" {
		t.Fatalf("got %+v", s)
	}
}

func TestParseSettingsRefusals(t *testing.T) {
	t.Parallel()
	ca := testPKI(t).caPEM
	cases := []struct {
		name   string
		json   map[string]any
		secure map[string]string
		field  string
	}{
		{"no cluster", map[string]any{"clusterString": nil}, nil, "clusterFilePath"},
		{"both clusters", map[string]any{"clusterFilePath": "/x"}, nil, "clusterString"},
		{"bad cluster string", map[string]any{"clusterString": "nonsense"}, nil, "clusterString"},
		{"bad tls mode", map[string]any{"tlsMode": "on"}, nil, "tlsMode"},
		{"config without CA", map[string]any{"tlsMode": "config"}, nil, "tlsCACert"},
		{"cert without key", map[string]any{"tlsMode": "config", "tlsCACert": ca}, map[string]string{"tlsClientCert": "C"}, "tlsClientCert"},
		{"key without cert", map[string]any{"tlsMode": "config", "tlsCACert": ca}, map[string]string{"tlsClientKey": "K"}, "tlsClientCert"},
		{"bad verify peers", map[string]any{"tlsMode": "config", "tlsCACert": ca, "tlsVerifyPeers": "Check.Bogus=1"}, nil, "tlsVerifyPeers"},
		{"no database", map[string]any{"database": nil}, nil, "database"},
		{"relative database", map[string]any{"database": "OTEL/R"}, nil, "database"},
		{"database with query", map[string]any{"database": "/OTEL?x=1"}, nil, "database"},
		{"database trailing slash", map[string]any{"database": "/OTEL/"}, nil, "database"},
		{"bad allowed schema", map[string]any{"allowedSchemas": []string{"T 1"}}, nil, "allowedSchemas"},
		{"two prefixes", map[string]any{"allowedSchemas": []string{"T*", "U*"}}, nil, "allowedSchemas"},
		{"inner star", map[string]any{"allowedSchemas": []string{"T*1"}}, nil, "allowedSchemas"},
		{"default outside allowed", map[string]any{"allowedSchemas": []string{"T2"}}, nil, "defaultSchema"},
		{"default malformed", map[string]any{"defaultSchema": "$tenant"}, nil, "defaultSchema"},
		{"bad otel mode", map[string]any{"otelMode": "yes"}, nil, "otelMode"},
		{"zero timeout", map[string]any{"queryTimeoutSeconds": 0}, nil, "queryTimeoutSeconds"},
		{"negative rows", map[string]any{"sqlMaxRows": -1}, nil, "sqlMaxRows"},
		{"search limit over max", map[string]any{"searchLimit": 1001}, nil, "searchLimit"},
		{"tx timeout over int32", map[string]any{"transactionTimeoutMs": 1 << 31}, nil, "transactionTimeoutMs"},
		{"non-numeric budget", map[string]any{"logsLimit": "lots"}, nil, "jsonData"},
		{"tag with comma", map[string]any{"transactionTags": []string{"a,b"}}, nil, "transactionTags"},
		{"tag too long", map[string]any{"transactionTags": []string{strings.Repeat("x", 17)}}, nil, "transactionTags"},
		{"too many tags", map[string]any{"transactionTags": []string{"a", "b", "c", "d", "e", "f"}}, nil, "transactionTags"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := instanceSettings(t, c.json)
			in.DecryptedSecureJSONData = c.secure
			_, err := ParseSettings(in)
			var se *SettingsError
			if !errors.As(err, &se) || se.Field != c.field {
				t.Fatalf("want a %s settings error, got %v", c.field, err)
			}
		})
	}
}

func TestParseSettingsEmptyJSON(t *testing.T) {
	t.Parallel()
	_, err := ParseSettings(backend.DataSourceInstanceSettings{})
	var se *SettingsError
	if !errors.As(err, &se) || se.Field != "clusterFilePath" {
		t.Fatalf("got %v", err)
	}
}

func TestDSN(t *testing.T) {
	t.Parallel()
	s, err := ParseSettings(instanceSettings(t, map[string]any{"transactionTags": []string{"graf", "dash"}, "transactionTimeoutMs": 4000}))
	if err != nil {
		t.Fatal(err)
	}
	want := "fdbsql:///OTEL/R_TEST?execution_scanned_bytes_limit=16777216&execution_scanned_rows_limit=10000" +
		"&max_rows=100000&max_statement_memory_bytes=268435456&read_only=true&schema=T1" +
		"&transaction_tags=graf%2Cdash&transaction_timeout=4000"
	if got := s.dsn("T1"); got != want {
		t.Fatalf("dsn\n got %s\nwant %s", got, want)
	}
	if got := s.dsn(""); strings.Contains(got, "schema=") || !strings.Contains(got, "read_only=true") {
		t.Fatalf("catalog dsn %s", got)
	}
}
