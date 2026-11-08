package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/recordlayer"
)

// TLS modes (jsonData.tlsMode).
const (
	tlsModeOff     = "off"     // plaintext; a ":tls" cluster is refused
	tlsModeCluster = "cluster" // the client's own resolution: ":tls" suffix + FDB_TLS_* env
	tlsModeConfig  = "config"  // tls.Config built from the datasource settings
)

// OTel modes (jsonData.otelMode).
const (
	otelModeAuto = "auto"
	otelModeOn   = "on"
	otelModeOff  = "off"
)

// Budget defaults and ceilings (RFC §9).
const (
	defaultQueryTimeout       = 30 * time.Second
	defaultTransactionTimeout = 5 * time.Second
	defaultSQLPageRows        = 10_000
	defaultSQLPageBytes       = 16 << 20
	defaultSQLMaxRows         = 100_000
	defaultSQLStatementMemory = 256 << 20
	defaultTraceMaxSpans      = 10_000
	defaultTraceMaxBytes      = 64 << 20
	defaultSearchLimit        = 20
	maxSearchLimit            = 1_000
	defaultSearchCandidates   = 20_000
	maxSearchCandidates       = 100_000_000 // otel.ReaderConfig's ceiling
	defaultSearchMaxBytes     = 4 << 20
	defaultLogsLimit          = 1_000

	// otel.ReaderConfig accepts 1s..5min per transaction.
	minTransactionTimeoutMs = 1_000
	maxTransactionTimeoutMs = 300_000
	// FDB's MAX_TRANSACTION_TAG_LENGTH, in bytes.
	maxTagBytes = 16
)

// Settings is the parsed and validated datasource configuration.
type Settings struct {
	ClusterFilePath string
	ClusterString   string

	TLSMode        string
	TLSCACert      string
	TLSVerifyPeers string
	TLSClientCert  string
	TLSClientKey   string

	SecureSocksProxy bool

	Database       string
	DefaultSchema  string
	AllowedSchemas schemaPolicy
	OTelMode       string

	QueryTimeout            time.Duration
	TransactionTimeout      time.Duration
	SQLPageRows             int64
	SQLPageBytes            int64
	SQLMaxRows              int64
	SQLStatementMemoryBytes int64
	TraceMaxSpans           int
	TraceMaxBytes           int64
	SearchLimit             int
	SearchMaxCandidates     int
	SearchMaxBytes          int64
	LogsLimit               int
	TransactionTags         []string
}

// SettingsError names the setting that is invalid.
type SettingsError struct {
	Field  string
	Reason string
}

func (e *SettingsError) Error() string {
	return fmt.Sprintf("invalid datasource setting %s: %s", e.Field, e.Reason)
}

func settingsErrorf(field, format string, args ...any) error {
	return &SettingsError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// jsonSettings is jsonData as the config editor or provisioning writes it.
// Numbers are accepted as JSON numbers or numeric strings (YAML provisioning).
type jsonSettings struct {
	ClusterFilePath         string   `json:"clusterFilePath"`
	ClusterString           string   `json:"clusterString"`
	TLSMode                 string   `json:"tlsMode"`
	TLSCACert               string   `json:"tlsCACert"`
	TLSVerifyPeers          string   `json:"tlsVerifyPeers"`
	EnableSecureSocksProxy  bool     `json:"enableSecureSocksProxy"`
	Database                string   `json:"database"`
	DefaultSchema           string   `json:"defaultSchema"`
	AllowedSchemas          []string `json:"allowedSchemas"`
	OTelMode                string   `json:"otelMode"`
	QueryTimeoutSeconds     *flexInt `json:"queryTimeoutSeconds"`
	TransactionTimeoutMs    *flexInt `json:"transactionTimeoutMs"`
	SQLPageRows             *flexInt `json:"sqlPageRows"`
	SQLPageBytes            *flexInt `json:"sqlPageBytes"`
	SQLMaxRows              *flexInt `json:"sqlMaxRows"`
	SQLStatementMemoryBytes *flexInt `json:"sqlStatementMemoryBytes"`
	TraceMaxSpans           *flexInt `json:"traceMaxSpans"`
	TraceMaxBytes           *flexInt `json:"traceMaxBytes"`
	SearchLimit             *flexInt `json:"searchLimit"`
	SearchMaxCandidates     *flexInt `json:"searchMaxCandidates"`
	SearchMaxBytes          *flexInt `json:"searchMaxBytes"`
	LogsLimit               *flexInt `json:"logsLimit"`
	TransactionTags         []string `json:"transactionTags"`
}

// flexInt is an integer written as a JSON number or a numeric string.
type flexInt struct {
	v int64
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(strings.TrimSpace(s))
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("%s is not an integer", b)
	}
	f.v = n
	return nil
}

// databasePath is a relational database path such as /OTEL/R_RUN.
var databasePath = regexp.MustCompile(`^(/[A-Za-z0-9_]+)+$`)

// ParseSettings reads and validates the instance settings. It does no I/O.
func ParseSettings(s backend.DataSourceInstanceSettings) (*Settings, error) {
	var j jsonSettings
	if len(bytes.TrimSpace(s.JSONData)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(s.JSONData))
		if err := dec.Decode(&j); err != nil {
			return nil, settingsErrorf("jsonData", "%v", err)
		}
	}
	out := &Settings{
		ClusterFilePath:  strings.TrimSpace(j.ClusterFilePath),
		ClusterString:    strings.TrimSpace(j.ClusterString),
		TLSMode:          j.TLSMode,
		TLSCACert:        j.TLSCACert,
		TLSVerifyPeers:   strings.TrimSpace(j.TLSVerifyPeers),
		TLSClientCert:    s.DecryptedSecureJSONData["tlsClientCert"],
		TLSClientKey:     s.DecryptedSecureJSONData["tlsClientKey"],
		SecureSocksProxy: j.EnableSecureSocksProxy,
		Database:         strings.TrimSpace(j.Database),
		DefaultSchema:    strings.TrimSpace(j.DefaultSchema),
		OTelMode:         j.OTelMode,
	}

	switch {
	case out.ClusterFilePath == "" && out.ClusterString == "":
		return nil, settingsErrorf("clusterFilePath", "set a cluster file path or a cluster string")
	case out.ClusterFilePath != "" && out.ClusterString != "":
		return nil, settingsErrorf("clusterString", "set a cluster file path or a cluster string, not both")
	case out.ClusterString != "":
		if _, err := client.ParseClusterString(out.ClusterString); err != nil {
			return nil, settingsErrorf("clusterString", "%v", err)
		}
	}

	if out.TLSMode == "" {
		out.TLSMode = tlsModeOff
	}
	switch out.TLSMode {
	case tlsModeOff, tlsModeCluster:
	case tlsModeConfig:
		if strings.TrimSpace(out.TLSCACert) == "" {
			return nil, settingsErrorf("tlsCACert", "tlsMode config needs a CA certificate; peers are verified against it alone")
		}
		if (out.TLSClientCert == "") != (out.TLSClientKey == "") {
			return nil, settingsErrorf("tlsClientCert", "client certificate and key must be set together")
		}
		if out.TLSVerifyPeers != "" {
			if _, err := transport.ParseVerifyPeers(out.TLSVerifyPeers); err != nil {
				return nil, settingsErrorf("tlsVerifyPeers", "%v", err)
			}
		}
	default:
		return nil, settingsErrorf("tlsMode", "%q is not off, cluster or config", out.TLSMode)
	}

	if out.Database == "" {
		return nil, settingsErrorf("database", "a relational database path is required, for example /OTEL/R_RUN")
	}
	if !databasePath.MatchString(out.Database) {
		return nil, settingsErrorf("database", "%q is not a database path (/SEGMENT[/SEGMENT...], letters, digits and _)", out.Database)
	}

	policy, err := parseSchemaPolicy(j.AllowedSchemas)
	if err != nil {
		return nil, err
	}
	out.AllowedSchemas = policy
	if out.DefaultSchema != "" {
		if err := policy.check(out.DefaultSchema); err != nil {
			return nil, settingsErrorf("defaultSchema", "%v", err)
		}
	}

	if out.OTelMode == "" {
		out.OTelMode = otelModeAuto
	}
	switch out.OTelMode {
	case otelModeAuto, otelModeOn, otelModeOff:
	default:
		return nil, settingsErrorf("otelMode", "%q is not auto, on or off", out.OTelMode)
	}

	type bound struct {
		name          string
		in            *flexInt
		def, min, max int64
		set           func(int64)
	}
	bounds := []bound{
		{"queryTimeoutSeconds", j.QueryTimeoutSeconds, int64(defaultQueryTimeout / time.Second), 1, 3600, func(v int64) { out.QueryTimeout = time.Duration(v) * time.Second }},
		{"transactionTimeoutMs", j.TransactionTimeoutMs, defaultTransactionTimeout.Milliseconds(), minTransactionTimeoutMs, maxTransactionTimeoutMs, func(v int64) { out.TransactionTimeout = time.Duration(v) * time.Millisecond }},
		{"sqlPageRows", j.SQLPageRows, defaultSQLPageRows, 1, math.MaxInt32, func(v int64) { out.SQLPageRows = v }},
		{"sqlPageBytes", j.SQLPageBytes, defaultSQLPageBytes, 1, math.MaxInt64, func(v int64) { out.SQLPageBytes = v }},
		{"sqlMaxRows", j.SQLMaxRows, defaultSQLMaxRows, 1, math.MaxInt32, func(v int64) { out.SQLMaxRows = v }},
		{"sqlStatementMemoryBytes", j.SQLStatementMemoryBytes, defaultSQLStatementMemory, 1, math.MaxInt64, func(v int64) { out.SQLStatementMemoryBytes = v }},
		{"traceMaxSpans", j.TraceMaxSpans, defaultTraceMaxSpans, 1, math.MaxInt32, func(v int64) { out.TraceMaxSpans = int(v) }},
		{"traceMaxBytes", j.TraceMaxBytes, defaultTraceMaxBytes, 1, math.MaxInt64, func(v int64) { out.TraceMaxBytes = v }},
		{"searchLimit", j.SearchLimit, defaultSearchLimit, 1, maxSearchLimit, func(v int64) { out.SearchLimit = int(v) }},
		{"searchMaxCandidates", j.SearchMaxCandidates, defaultSearchCandidates, 1, maxSearchCandidates, func(v int64) { out.SearchMaxCandidates = int(v) }},
		{"searchMaxBytes", j.SearchMaxBytes, defaultSearchMaxBytes, 1, math.MaxInt64, func(v int64) { out.SearchMaxBytes = v }},
		{"logsLimit", j.LogsLimit, defaultLogsLimit, 1, math.MaxInt32, func(v int64) { out.LogsLimit = int(v) }},
	}
	for _, b := range bounds {
		v := b.def
		if b.in != nil {
			v = b.in.v
		}
		if v < b.min || v > b.max {
			return nil, settingsErrorf(b.name, "%d is outside %d..%d", v, b.min, b.max)
		}
		b.set(v)
	}

	tags, err := parseTransactionTags(j.TransactionTags)
	if err != nil {
		return nil, err
	}
	out.TransactionTags = tags
	return out, nil
}

// parseTransactionTags applies the record layer's and FDB's tag limits; a
// comma would split the tag in the DSN's comma-separated transaction_tags.
func parseTransactionTags(in []string) ([]string, error) {
	var tags []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.Contains(t, ",") {
			return nil, settingsErrorf("transactionTags", "tag %q contains a comma", t)
		}
		if len(t) > maxTagBytes {
			return nil, settingsErrorf("transactionTags", "tag %q is longer than %d bytes", t, maxTagBytes)
		}
		tags = append(tags, t)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	uniq, err := recordlayer.ValidateTags(tags)
	if err != nil {
		return nil, settingsErrorf("transactionTags", "%v", err)
	}
	return uniq, nil
}
