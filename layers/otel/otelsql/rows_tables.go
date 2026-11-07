package otelsql

import (
	"crypto/sha256"
	"fmt"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// ScopeLog scopes a log record's own attributes; resource and scope
// attributes use ScopeResource and ScopeScope.
const ScopeLog = "log"

// Log is one OTLP log record with its resource and scope, the receiver's
// identity and effective time, and the envelope bytes stored in the otlp
// column (the exporter's single-log ResourceLogs).
type Log struct {
	Resource          *resourcepb.Resource
	ResourceSchemaURL string
	Scope             *commonpb.InstrumentationScope
	ScopeSchemaURL    string
	Log               *logspb.LogRecord
	EventID           string
	// EffectiveNs and TimeSource are the time the log is ordered by and
	// where it came from (event, observed or receiver time).
	EffectiveNs int64
	TimeSource  string
	OTLP        []byte
}

// BuildLogCompiled returns the LOGS record of l. Its attribute arrays are
// canonical sets as a span's (Span), under the resource, scope and log
// scopes. An all-zero or empty trace or span ID is NULL, so an uncorrelated
// log has NULL trace_id and span_id. event_name is projected up to
// MaxIndexedStringBytes, since LOGS_EVENT holds it in a key; a longer one is
// counted by LogProjection. body_text is a string body.
func BuildLogCompiled(l Log) (*otelv1pb.LOGS, error) {
	lr := l.Log
	if lr == nil {
		return nil, fmt.Errorf("otelsql: nil log record")
	}
	if l.EventID == "" {
		return nil, fmt.Errorf("otelsql: log without event ID")
	}
	r := &otelv1pb.LOGS{
		EVENT_ID:            proto.String(l.EventID),
		TRACE_ID:            nonZeroID(lr.TraceId),
		SPAN_ID:             nonZeroID(lr.SpanId),
		FLAGS:               proto.Int64(int64(lr.Flags)),
		TIME_NS:             proto.Int64(int64(lr.TimeUnixNano)),         //nolint:gosec // admission bounds times by MaxInt64
		OBSERVED_NS:         proto.Int64(int64(lr.ObservedTimeUnixNano)), //nolint:gosec // as TIME_NS
		EFFECTIVE_NS:        proto.Int64(l.EffectiveNs),
		TIME_SOURCE:         nonEmptyString(l.TimeSource),
		SEVERITY_NUMBER:     proto.Int32(int32(lr.SeverityNumber)),
		SEVERITY_TEXT:       nonEmptyString(lr.SeverityText),
		BODY_TEXT:           nonEmptyString(lr.GetBody().GetStringValue()),
		RESOURCE_SCHEMA_URL: nonEmptyString(l.ResourceSchemaURL),
		SCOPE_SCHEMA_URL:    nonEmptyString(l.ScopeSchemaURL),
		DROPPED_ATTRIBUTES:  proto.Int64(int64(lr.DroppedAttributesCount)),
		OTLP:                nonEmptyBytes(l.OTLP),
	}
	if sc := l.Scope; sc != nil {
		r.SCOPE_NAME = nonEmptyString(sc.Name)
		r.SCOPE_VERSION = nonEmptyString(sc.Version)
	}
	if len(lr.EventName) <= MaxIndexedStringBytes {
		r.EVENT_NAME = nonEmptyString(lr.EventName)
	}
	sets := logSets(l)
	r.SERVICE_NAME = nonEmptyString(serviceName(sets[0]))
	attrs := attrLists{long: true}
	attrs.add(ScopeResource, sets[0])
	attrs.add(ScopeScope, sets[1])
	attrs.add(ScopeLog, sets[2])
	if len(attrs.str) > 0 {
		r.STR_ATTRS = &otelv1pb.LOGS_STR_ATTRS{Values: attrs.str}
	}
	if len(attrs.longStr) > 0 {
		r.LONG_STR_ATTRS = &otelv1pb.LOGS_LONG_STR_ATTRS{Values: attrs.longStr}
	}
	if len(attrs.ints) > 0 {
		r.INT_ATTRS = &otelv1pb.LOGS_INT_ATTRS{Values: attrs.ints}
	}
	if len(attrs.doubles) > 0 {
		r.DOUBLE_ATTRS = &otelv1pb.LOGS_DOUBLE_ATTRS{Values: attrs.doubles}
	}
	if len(attrs.bools) > 0 {
		r.BOOL_ATTRS = &otelv1pb.LOGS_BOOL_ATTRS{Values: attrs.bools}
	}
	if len(attrs.bytes) > 0 {
		r.BYTES_ATTRS = &otelv1pb.LOGS_BYTES_ATTRS{Values: attrs.bytes}
	}
	if len(attrs.anys) > 0 {
		r.ANY_ATTRS = &otelv1pb.LOGS_ANY_ATTRS{Values: attrs.anys}
	}
	return r, nil
}

// LogProjection is what l's LOGS row leaves out of the equality indexes,
// an event name too long for LOGS_EVENT included.
func LogProjection(l Log) Projection {
	sets := logSets(l)
	p := projectionOf(sets[:]...)
	if len(l.Log.GetEventName()) > MaxIndexedStringBytes {
		p.Unindexed++
	}
	return p
}

// logSets are the canonical resource, scope and log attribute sets of l.
func logSets(l Log) [3]attrSet {
	return [3]attrSet{
		canonicalSet(l.Resource.GetAttributes(), false),
		canonicalSet(l.Scope.GetAttributes(), false),
		canonicalSet(l.Log.GetAttributes(), false),
	}
}

// nonZeroID is id, or nil (NULL) when it is empty or all zero bytes: OTLP's
// invalid trace and span IDs.
func nonZeroID(id []byte) []byte {
	for _, b := range id {
		if b != 0 {
			return id
		}
	}
	return nil
}

// MessageTextShards is the number of MESSAGES_TEXT groups per role: a
// message's shard is its writer's (MessageShard).
const MessageTextShards = 16

// MessageShard is the shard column of writer's messages.
func MessageShard(writer int64) int64 { return writer % MessageTextShards }

// MessageHashBytes is the length of MESSAGES.content_hash.
const MessageHashBytes = 16

// messageHashDomain separates content hashes from other SHA-256 uses; the
// store is the tenant, so the tenant is not hashed.
const messageHashDomain = "otel_msg_v2\x00"

// MessageContentHash is the content_hash of an element of encoding:
// SHA-256(domain ‖ encoding ‖ 0 ‖ element), truncated to MessageHashBytes.
func MessageContentHash(encoding string, element []byte) []byte {
	h := sha256.New()
	h.Write([]byte(messageHashDomain))
	h.Write([]byte(encoding))
	h.Write([]byte{0})
	h.Write(element)
	return h.Sum(nil)[:MessageHashBytes]
}

// MessagePart is one MESSAGES row. Every part has the message's writer, seq
// and shard; part 0 also carries the whole message's columns.
type MessagePart struct {
	Writer, Seq, Part int64
	// Text is this part's searchable text segment, or nil for a part
	// without one; Role and Content are then NULL and the part has no
	// MESSAGES_TEXT entries.
	Text *MessageText
	// Raw is this part's run of the element's bytes (NULL when nil).
	Raw []byte
	// Part 0 only.
	ContentHash     []byte
	Encoding        string
	RawBytes        int64
	Parts           int64
	UnindexedTokens int64 // NULL when 0
	// Large lists a message whose text spans several parts in MESSAGES_LARGE.
	Large bool
}

// MessageText is a part's text segment: the message's normalized role (the
// MESSAGES_TEXT group with the shard) and the tokens skipped before it.
type MessageText struct {
	Role, Content string
	TokenOffset   int64 // NULL when 0
}

// BuildMessageCompiled returns the MESSAGES record of p; a part other than
// part 0 that sets a whole-message column is refused.
func BuildMessageCompiled(p MessagePart) (*otelv1pb.MESSAGES, error) {
	if p.Writer <= 0 || p.Seq <= 0 || p.Part < 0 {
		return nil, fmt.Errorf("otelsql: message %d.%d part %d: writer and seq must be positive, part not negative", p.Writer, p.Seq, p.Part)
	}
	r := &otelv1pb.MESSAGES{
		WRITER: proto.Int64(p.Writer),
		SEQ:    proto.Int64(p.Seq),
		PART:   proto.Int64(p.Part),
		SHARD:  proto.Int64(MessageShard(p.Writer)),
		RAW:    p.Raw,
	}
	if t := p.Text; t != nil {
		r.ROLE, r.CONTENT = proto.String(t.Role), proto.String(t.Content)
		if t.TokenOffset != 0 {
			r.TOKEN_OFFSET = proto.Int64(t.TokenOffset)
		}
	}
	if p.Part != 0 {
		if p.ContentHash != nil || p.Encoding != "" || p.RawBytes != 0 || p.Parts != 0 || p.UnindexedTokens != 0 || p.Large {
			return nil, fmt.Errorf("otelsql: message %d.%d part %d sets a part-0 column", p.Writer, p.Seq, p.Part)
		}
		return r, nil
	}
	r.CONTENT_HASH = p.ContentHash
	r.ENCODING = proto.String(p.Encoding)
	r.RAW_BYTES = proto.Int64(p.RawBytes)
	r.PARTS = proto.Int64(p.Parts)
	if p.UnindexedTokens != 0 {
		r.UNINDEXED_TOKENS = proto.Int64(p.UnindexedTokens)
	}
	if p.Large {
		r.LARGE = &otelv1pb.MESSAGES_LARGE{Values: []int64{1}}
	}
	return r, nil
}

// BuildMessageTraceCompiled returns the MESSAGE_TRACES marker that message
// (writer, seq) occurs in trace.
func BuildMessageTraceCompiled(writer, seq int64, trace []byte) *otelv1pb.MESSAGE_TRACES {
	return &otelv1pb.MESSAGE_TRACES{WRITER: proto.Int64(writer), SEQ: proto.Int64(seq), TRACE_ID: trace}
}

// STORE_META row names.
const (
	// StoreMetaLayout's str_value is the store's message layout; a writer
	// with another one refuses the store.
	StoreMetaLayout = "layout"
	// StoreMetaMessageWriter's int_value is the last message writer allocated.
	StoreMetaMessageWriter = "message_writer"
	// StoreMetaContinuationHMAC's bytes_value is the continuation-token key.
	StoreMetaContinuationHMAC = "continuation_hmac"
)

// StoreMeta is one STORE_META row; a nil value column is NULL.
type StoreMeta struct {
	Name  string
	Int   *int64
	Bytes []byte
	Str   *string
}

// BuildStoreMetaCompiled returns the STORE_META record of m.
func BuildStoreMetaCompiled(m StoreMeta) (*otelv1pb.STORE_META, error) {
	if m.Name == "" {
		return nil, fmt.Errorf("otelsql: store_meta row without a name")
	}
	return &otelv1pb.STORE_META{NAME: proto.String(m.Name), INT_VALUE: m.Int, BYTES_VALUE: m.Bytes, STR_VALUE: m.Str}, nil
}
