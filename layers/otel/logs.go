package otel

import (
	"crypto/rand"
	"encoding/hex"
	"math"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	maxLogRawBytes = 8 << 20
	maxLogEventID  = 256
	// Larger singletons must also pass expanded-write admission, reserving
	// space below FDB's 10MB limit for native conflicts and replacement clears.
	logBatchBudget      = 2 << 20
	maxLogTxnWriteBytes = 8_000_000

	// LogScopeRecord scopes log-record attributes; LogScopeIntrinsic holds
	// indexed top-level fields (event_name) under a reserved scope.
	LogScopeRecord    = "log"
	LogScopeIntrinsic = "intrinsic"

	LogTimeEvent    = "event"
	LogTimeObserved = "observed"
	LogTimeReceiver = "receiver"
)

var logAttributeScopes = []string{"resource", "scope", LogScopeRecord, LogScopeIntrinsic}

// LogRecord retains authoritative OTLP bytes with detached projections; construct a new record to edit them.
// EventID is editable receiver metadata; ingestion chooses event, observed, then receiver time.
type LogRecord struct {
	EventID                string               `json:"event_id"`
	Timestamp              int64                `json:"time_unix_nano"`
	ObservedTimestamp      int64                `json:"observed_time_unix_nano"`
	TraceID                string               `json:"trace_id,omitempty"`
	SpanID                 string               `json:"span_id,omitempty"`
	Flags                  uint32               `json:"flags,omitempty"`
	SeverityText           string               `json:"severity_text,omitempty"`
	SeverityNumber         int32                `json:"severity_number,omitempty"`
	EventName              string               `json:"event_name,omitempty"`
	Body                   *commonpb.AnyValue   `json:"body,omitempty"`
	DroppedAttributesCount uint32               `json:"dropped_attributes_count,omitempty"`
	ResourceSchemaURL      string               `json:"resource_schema_url,omitempty"`
	ResourceAttributes     []*commonpb.KeyValue `json:"resource_attributes,omitempty"`
	ScopeName              string               `json:"scope_name,omitempty"`
	ScopeVersion           string               `json:"scope_version,omitempty"`
	ScopeSchemaURL         string               `json:"scope_schema_url,omitempty"`
	ScopeAttributes        []*commonpb.KeyValue `json:"scope_attributes,omitempty"`
	Attributes             []*commonpb.KeyValue `json:"attributes,omitempty"`
	EffectiveTime          int64                `json:"effective_time_unix_nano"`
	TimeSource             string               `json:"time_source"`
	original               *logspb.ResourceLogs
}

// OTLP returns a detached envelope preserving all stored/converted protobuf fields.
// For constructed records it builds an envelope and validates hex identities.
func (l LogRecord) OTLP() (*logspb.ResourceLogs, error) {
	rl, err := toResourceLogs(l)
	if err != nil {
		return nil, err
	}
	return proto.Clone(rl).(*logspb.ResourceLogs), nil
}

// LogMatch is one verified log witness from a log equality posting.
type LogMatch struct {
	EventID string `json:"event_id"`
	Trace   string `json:"trace_id,omitempty"`
	Span    string `json:"span_id,omitempty"`
	Time    int64  `json:"time_unix_nano"`
}

// LogExportStats describes one ExportLogs call. EventIDs is aligned with the
// input, including generated IDs; committed counters cover committed batches.
type LogExportStats struct {
	ReceivedLogs        int64    `json:"received_logs"`
	DuplicateLogs       int64    `json:"duplicate_logs"`
	GeneratedIDs        int64    `json:"generated_ids"`
	Logs                int64    `json:"logs"`
	Batches             int64    `json:"batches"`
	Replaced            int64    `json:"replaced"`
	Correlated          int64    `json:"correlated"`
	LogicalBytes        int64    `json:"logical_bytes"`
	StoredBytes         int64    `json:"stored_bytes"`
	UnindexedAttributes int64    `json:"unindexed_attributes"`
	EventIDs            []string `json:"event_ids"`
}

// newEventID is a random RFC 4122 version 4 UUID in lowercase hex.
func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", wrap("operation", "log event ID", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:]), nil
}

func nonNilKeyValues(kvs []*commonpb.KeyValue) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	for _, kv := range kvs {
		if kv != nil {
			out = append(out, kv)
		}
	}
	return out
}

// toResourceLogs is the stored single-record OTLP form of l.
func toResourceLogs(l LogRecord) (*logspb.ResourceLogs, error) {
	if l.original != nil {
		return l.original, nil
	}
	rec := &logspb.LogRecord{
		TimeUnixNano:           uint64(l.Timestamp),
		ObservedTimeUnixNano:   uint64(l.ObservedTimestamp),
		SeverityNumber:         logspb.SeverityNumber(l.SeverityNumber),
		SeverityText:           l.SeverityText,
		Body:                   l.Body,
		Attributes:             nonNilKeyValues(l.Attributes),
		DroppedAttributesCount: l.DroppedAttributesCount,
		Flags:                  l.Flags,
		EventName:              l.EventName,
	}
	var err error
	if l.TraceID != "" {
		if rec.TraceId, err = hex.DecodeString(l.TraceID); err != nil {
			return nil, wrap("admission", "log trace identity", err)
		}
	}
	if l.SpanID != "" {
		if rec.SpanId, err = hex.DecodeString(l.SpanID); err != nil {
			return nil, wrap("admission", "log span identity", err)
		}
	}
	return &logspb.ResourceLogs{
		Resource:  &resourcepb.Resource{Attributes: nonNilKeyValues(l.ResourceAttributes)},
		SchemaUrl: l.ResourceSchemaURL,
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: l.ScopeName, Version: l.ScopeVersion, Attributes: nonNilKeyValues(l.ScopeAttributes)},
			SchemaUrl:  l.ScopeSchemaURL,
			LogRecords: []*logspb.LogRecord{rec},
		}},
	}, nil
}

// fromResourceLogs restores a stored record; it rejects any other shape.
func fromResourceLogs(eventID string, effective int64, source string, rl *logspb.ResourceLogs) (LogRecord, error) {
	if len(rl.GetScopeLogs()) != 1 || len(rl.ScopeLogs[0].GetLogRecords()) != 1 || rl.ScopeLogs[0].LogRecords[0] == nil {
		return LogRecord{}, fail("corruption", "log decode", "expected one stored log record")
	}
	l := projectLog(rl)
	if l.Timestamp < 0 || l.ObservedTimestamp < 0 {
		return LogRecord{}, fail("corruption", "log decode", "stored timestamp exceeds int64")
	}
	l.EventID, l.EffectiveTime, l.TimeSource = eventID, effective, source
	if l.TraceID != "" && !validIdentity(l.TraceID, 16) {
		return LogRecord{}, fail("corruption", "log decode", "invalid stored trace identity")
	}
	if l.SpanID != "" && (l.TraceID == "" || !validIdentity(l.SpanID, 8)) {
		return LogRecord{}, fail("corruption", "log decode", "invalid stored span identity")
	}
	return l, nil
}

// projectLog keeps mutable query projections separate from the retained source.
// rl is an owned, single-log envelope and is never mutated after this call.
func projectLog(rl *logspb.ResourceLogs) LogRecord {
	projection := proto.Clone(rl).(*logspb.ResourceLogs)
	sl := projection.ScopeLogs[0]
	rec := sl.LogRecords[0]
	id := func(b []byte) string {
		if len(b) == 0 || zeroID(b) {
			return ""
		}
		return hex.EncodeToString(b)
	}
	clampTime := func(t uint64) int64 {
		if t > math.MaxInt64 {
			return -1 // rejected at admission rather than silently clamped
		}
		return int64(t)
	}
	return LogRecord{
		original:               rl,
		Timestamp:              clampTime(rec.TimeUnixNano),
		ObservedTimestamp:      clampTime(rec.ObservedTimeUnixNano),
		TraceID:                id(rec.TraceId),
		SpanID:                 id(rec.SpanId),
		Flags:                  rec.Flags,
		SeverityText:           rec.SeverityText,
		SeverityNumber:         int32(rec.SeverityNumber),
		EventName:              rec.EventName,
		Body:                   rec.Body,
		DroppedAttributesCount: rec.DroppedAttributesCount,
		ResourceSchemaURL:      projection.SchemaUrl,
		ResourceAttributes:     projection.GetResource().GetAttributes(),
		ScopeName:              sl.GetScope().GetName(),
		ScopeVersion:           sl.GetScope().GetVersion(),
		ScopeSchemaURL:         sl.SchemaUrl,
		ScopeAttributes:        sl.GetScope().GetAttributes(),
		Attributes:             rec.Attributes,
	}
}

// LogRecordsFromResourceLogs owns each log envelope and exposes detached projections without an EventID.
// Empty/all-zero trace or span IDs remain in source bytes but are uncorrelated in indexes.
func LogRecordsFromResourceLogs(in []*logspb.ResourceLogs) []LogRecord {
	var out []LogRecord
	for _, rl := range in {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				if r == nil {
					continue
				}
				// Copy only this record's ancestors, not sibling scopes or logs.
				scope := &logspb.ScopeLogs{Scope: sl.Scope, SchemaUrl: sl.SchemaUrl, LogRecords: []*logspb.LogRecord{r}}
				scope.ProtoReflect().SetUnknown(sl.ProtoReflect().GetUnknown())
				envelope := &logspb.ResourceLogs{Resource: rl.Resource, SchemaUrl: rl.SchemaUrl, ScopeLogs: []*logspb.ScopeLogs{scope}}
				envelope.ProtoReflect().SetUnknown(rl.ProtoReflect().GetUnknown())
				out = append(out, projectLog(proto.Clone(envelope).(*logspb.ResourceLogs)))
			}
		}
	}
	return out
}
