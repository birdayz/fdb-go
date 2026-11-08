package plugin

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"fdb.dev/layers/otel"
)

// Query types (DataQuery.queryType).
const (
	queryTypeSQL         = "sql"
	queryTypeTrace       = "trace"
	queryTypeTraceSearch = "traceSearch"
	queryTypeLogs        = "logs"
)

// Search modes (traceSearch.mode).
const (
	modeSameSpan    = "sameSpan"
	modeExistential = "existential"
	modeAttribute   = "attribute"
	modeTime        = "time"
)

// queryModel is the DataQuery JSON the query editor emits.
type queryModel struct {
	QueryType string `json:"queryType"`
	Tenant    string `json:"tenant"`

	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`

	Mode           string      `json:"mode"`
	SpanPredicates []predicate `json:"spanPredicates"`
	LogPredicates  []predicate `json:"logPredicates"`
	Limit          *int        `json:"limit"`

	Attribute *predicate `json:"attribute"`
}

// predicate is a typed attribute equality {scope, key, type, value}.
type predicate struct {
	Scope string `json:"scope"`
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// QueryError is a malformed query; Grafana shows it on the panel.
type QueryError struct {
	Field  string
	Reason string
}

func (e *QueryError) Error() string { return fmt.Sprintf("invalid query: %s: %s", e.Field, e.Reason) }

func queryErrorf(field, format string, args ...any) error {
	return &QueryError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

func parseQuery(q backend.DataQuery) (*queryModel, error) {
	var m queryModel
	if err := json.Unmarshal(q.JSON, &m); err != nil {
		return nil, queryErrorf("query", "%v", err)
	}
	m.Tenant = strings.TrimSpace(m.Tenant)
	return &m, nil
}

// normalizeTraceID accepts 32 hex digits, or 16 for a 64-bit trace ID
// (left-padded as the query editor does), and returns lowercase hex.
func normalizeTraceID(field, s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 16 && isHex(s) {
		s = strings.Repeat("0", 16) + s
	}
	if len(s) != 32 || !isHex(s) {
		return "", queryErrorf(field, "%q is not a 32-digit hex trace ID", s)
	}
	if strings.Trim(s, "0") == "" {
		return "", queryErrorf(field, "the all-zero trace ID is invalid")
	}
	return s, nil
}

// normalizeSpanID accepts 16 hex digits and returns lowercase hex.
func normalizeSpanID(field, s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 16 || !isHex(s) {
		return "", queryErrorf(field, "%q is not a 16-digit hex span ID", s)
	}
	if strings.Trim(s, "0") == "" {
		return "", queryErrorf(field, "the all-zero span ID is invalid")
	}
	return s, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// Attribute signals and the scopes each one's indexes cover.
const (
	signalSpan = "span"
	signalLog  = "log"
)

var (
	spanScopes = []string{"resource", "scope", "span"}
	logScopes  = []string{"resource", "scope", otel.LogScopeRecord}
	attrTypes  = []string{"string", "int", "double", "bool"}
)

// scopeEventName is the logs predicate scope for the indexed event_name field.
const scopeEventName = "event_name"

// toAttribute validates a predicate for signal and converts it. Values are
// passed through; the reader refuses non-canonical ones by name.
func (p predicate) toAttribute(field, signal string) (otel.Attribute, error) {
	if signal == signalLog && p.Scope == scopeEventName {
		if p.Key != "" && p.Key != "event_name" || p.Type != "" && p.Type != "string" {
			return otel.Attribute{}, queryErrorf(field, "an event_name predicate has key event_name and type string")
		}
		return otel.Attribute{Scope: otel.LogScopeIntrinsic, Key: "event_name", Type: "string", Value: p.Value}, nil
	}
	scopes := spanScopes
	if signal == signalLog {
		scopes = logScopes
	}
	if !slices.Contains(scopes, p.Scope) {
		extra := ""
		if signal == signalLog {
			extra = " or " + scopeEventName
		}
		return otel.Attribute{}, queryErrorf(field, "scope %q is not %s%s", p.Scope, strings.Join(scopes, ", "), extra)
	}
	if !slices.Contains(attrTypes, p.Type) {
		return otel.Attribute{}, queryErrorf(field, "type %q is not string, int, double or bool", p.Type)
	}
	if p.Key == "" {
		return otel.Attribute{}, queryErrorf(field, "attribute key is empty")
	}
	return otel.Attribute{Scope: p.Scope, Key: p.Key, Type: p.Type, Value: p.Value}, nil
}

// timeRange is the request's range in ns, as OTEL_V1 stores time.
func timeRange(tr backend.TimeRange) otel.TimeRange {
	return otel.TimeRange{Start: tr.From.UnixNano(), End: tr.To.UnixNano()}
}
