package otel

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"gopkg.in/yaml.v3"
)

const (
	// Live runs use a fixed synthetic corpus identity so every handle and tenant
	// of one run agrees on the manifest; the seed keeps it distinct from fixtures.
	exporterSeed    uint64 = 0x6f746c706c697665
	exporterCount          = 100000000
	exporterTenants        = exporterCount / 2

	// exportSpanBudget bounds a span batch's conservative estimate (spanCost)
	// and so every admitted span; the 500KB actual-size guard runs inside the save.
	exportSpanBudget = 300000

	// Decoded projection caps; larger values stay unindexed. Encoded covering
	// values must additionally fit FDB's value limit before any export writes.
	maxIndexedKeyBytes   = 1024
	maxIndexedValueBytes = 64000
)

// ExporterConfig is shared by the SDK exporter and the Collector exporter.
type ExporterConfig struct {
	ClusterFile string `json:"cluster_file" yaml:"cluster_file"`
	Run         string `json:"run" yaml:"run"`
	Tenant      int    `json:"tenant" yaml:"tenant"`
	// RecordCompression is none, zlib-1 or zlib-default: SQL and Java read
	// the store's records, and neither reads Brotli (D2).
	RecordCompression  string `json:"record_compression" yaml:"record_compression"`
	BulkIndexConflicts bool   `json:"bulk_index_conflicts" yaml:"bulk_index_conflicts"`
	// EnableMessageDeltas stores each GenAI message once per tenant as MESSAGES
	// rows and replaces the span attribute with a manifest of writer/sequence IDs.
	EnableMessageDeltas bool `json:"enable_message_deltas" yaml:"enable_message_deltas"`
	// EnableMessageFullText decomposes GenAI messages (as EnableMessageDeltas
	// does) and indexes their text and span back references for search.
	EnableMessageFullText bool  `json:"enable_message_full_text" yaml:"enable_message_full_text"`
	BatchSize             int   `json:"batch_size" yaml:"batch_size"`
	Workers               int   `json:"workers" yaml:"workers"`
	MaxCandidates         int   `json:"max_candidates" yaml:"max_candidates"`
	MaxBytes              int64 `json:"max_bytes" yaml:"max_bytes"`
	K                     int   `json:"k" yaml:"k"`
	// MaxInFlightTransactions bounds this Exporter's concurrent write
	// transactions across all export calls (1..1024).
	MaxInFlightTransactions int `json:"max_inflight_transactions" yaml:"max_inflight_transactions"`
	// TransactionTimeout covers GRV, commit and all retries: a Go duration, 1s..5m, default 10s.
	// Caller cancellation also bounds work except a dispatched commit; there is no retry limit.
	TransactionTimeout time.Duration `json:"transaction_timeout" yaml:"transaction_timeout"`
}

func DefaultExporterConfig() ExporterConfig {
	return ExporterConfig{RecordCompression: "zlib-1", BulkIndexConflicts: true, BatchSize: 64, Workers: 8, MaxCandidates: 20000, MaxBytes: 4 << 20, K: 100, MaxInFlightTransactions: defaultMaxInFlightTransactions, TransactionTimeout: defaultExporterTxnTimeout}
}

// removedExporterKeys are the configuration keys of the custom store that
// the relational store has no use for (D2); a document naming one is refused.
var removedExporterKeys = map[string]bool{"order": true, "hash_bytes": true, "posting_value_format": true}

// exporterConfigKeys are ExporterConfig's JSON and YAML keys.
var exporterConfigKeys = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(ExporterConfig{})
	for i := 0; i < t.NumField(); i++ {
		out[t.Field(i).Tag.Get("json")] = true
	}
	return out
}()

// RemovedConfigKeyError is the cause of the ConfigError that refuses a key
// the relational store removed: order, hash_bytes or posting_value_format.
type RemovedConfigKeyError struct{ Key string }

func (e *RemovedConfigKeyError) Error() string {
	return e.Key + " was removed: the relational store has one time order and no posting values"
}

// checkExporterKey refuses a key ExporterConfig does not have, naming the
// removed ones as such.
func checkExporterKey(op, key string) error {
	switch {
	case removedExporterKeys[key]:
		return typedError("config", op, "", &RemovedConfigKeyError{Key: key})
	case !exporterConfigKeys[key]:
		return failf("config", op, "unknown key %q", key)
	}
	return nil
}

// exporterConfigFields is ExporterConfig without its JSON methods.
type exporterConfigFields ExporterConfig

// exporterConfigJSON writes TransactionTimeout as a Go duration string; the
// outer field shadows the embedded one.
type exporterConfigJSON struct {
	exporterConfigFields
	TransactionTimeout string `json:"transaction_timeout"`
}

func (c ExporterConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(exporterConfigJSON{exporterConfigFields(c), c.TransactionTimeout.String()})
}

// UnmarshalJSON starts from DefaultExporterConfig so omitted fields keep
// defaults. Removed and unknown keys are refused.
func (c *ExporterConfig) UnmarshalJSON(data []byte) error {
	const op = "exporter JSON"
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return wrap("config", op, err)
	}
	for k := range keys {
		if err := checkExporterKey(op, k); err != nil {
			return err
		}
	}
	d := DefaultExporterConfig()
	p := exporterConfigJSON{exporterConfigFields(d), d.TransactionTimeout.String()}
	if err := json.Unmarshal(data, &p); err != nil {
		return wrap("config", op, err)
	}
	timeout, err := time.ParseDuration(p.TransactionTimeout)
	if err != nil {
		return wrap("config", op, fmt.Errorf("transaction_timeout: %w", err))
	}
	*c = ExporterConfig(p.exporterConfigFields)
	c.TransactionTimeout = timeout
	return nil
}

// UnmarshalYAML starts from DefaultExporterConfig so omitted fields keep
// defaults. Removed and unknown keys are refused.
func (c *ExporterConfig) UnmarshalYAML(node *yaml.Node) error {
	const op = "exporter YAML"
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := checkExporterKey(op, node.Content[i].Value); err != nil {
				return err
			}
		}
	}
	type plain ExporterConfig
	p := plain(DefaultExporterConfig())
	if err := node.Decode(&p); err != nil {
		return wrap("config", op, err)
	}
	*c = ExporterConfig(p)
	return nil
}

func (c ExporterConfig) Validate() error {
	if c.ClusterFile == "" {
		return fail("config", "exporter", "explicit owned FDB cluster file required")
	}
	switch c.RecordCompression {
	case "none", "zlib-1", "zlib-default":
	default:
		// D2: SQL and Java read the store's records; neither reads Brotli.
		return failf("config", "exporter", "record_compression must be none, zlib-1 or zlib-default, not %q: SQL and Java read the records", c.RecordCompression)
	}
	if c.BatchSize < 1 || c.BatchSize > 64 {
		return fail("config", "exporter", "batch_size must be 1..64")
	}
	if c.Workers < 1 || c.Workers > 16 {
		return fail("config", "exporter", "workers must be 1..16")
	}
	if c.MaxInFlightTransactions < 1 || c.MaxInFlightTransactions > maxInFlightTransactionsLimit {
		return failf("config", "exporter", "max_inflight_transactions must be 1..%d", maxInFlightTransactionsLimit)
	}
	if c.TransactionTimeout < minExporterTxnTimeout || c.TransactionTimeout > maxExporterTxnTimeout {
		return failf("config", "exporter", "transaction_timeout must be %s..%s", minExporterTxnTimeout, maxExporterTxnTimeout)
	}
	if c.Tenant < 0 || c.Tenant >= exporterTenants {
		return failf("config", "exporter", "tenant must be 0..%d", exporterTenants-1)
	}
	return c.backendConfig().Validate()
}

// backendConfig is the read configuration of the exporter's SpanReader:
// run, tenant, K, budgets and message layout. The store has one time order
// (D2); the remaining fields only satisfy Config.Validate.
func (c ExporterConfig) backendConfig() Config {
	return Config{Run: c.Run, Backend: "sql", Seed: exporterSeed, Count: exporterCount, Tenants: exporterTenants, Attributes: 4, MatchEvery: 1, Batch: c.BatchSize, SaveMode: "batch", RecordCompression: c.RecordCompression, BulkIndexConflicts: c.BulkIndexConflicts, Workers: 1, CHBatch: 8192, CHBatchBytes: 8 << 20, CHIndexGranularity: 8192, CHQueryTransport: "http", Tenant: c.Tenant, K: c.K, Iterations: 1, MaxCandidates: c.MaxCandidates, MaxBytes: c.MaxBytes, Order: "time", Messages: c.messageLayout()}
}

// messageLayout is the stored message layout the flags select.
func (c ExporterConfig) messageLayout() string {
	switch {
	case c.EnableMessageFullText:
		return messagesFullText
	case c.EnableMessageDeltas:
		return messagesDeltas
	}
	return messagesNone
}

// ExportStats describes one ExportResourceSpans call. Committed counters cover
// only batches whose transactions committed, including on a partial failure.
type ExportStats struct {
	ReceivedSpans       int64             `json:"received_spans"`
	DuplicateSpans      int64             `json:"duplicate_spans"`
	Spans               int64             `json:"spans"`
	Batches             int64             `json:"batches"`
	LogicalBytes        int64             `json:"logical_bytes"`
	NormalizedSpans     int64             `json:"normalized_spans"`
	UnindexedAttributes int64             `json:"unindexed_attributes"`
	MessageDeltas       MessageDeltaStats `json:"message_deltas,omitempty"`
	Messages            MessageStoreStats `json:"messages,omitempty"`
}

// SpanNotFoundError reports a LoadSpanOTLP identity absent from the tenant.
type SpanNotFoundError struct {
	*OtelError
	Trace string
	Span  string
}

// CanonicalValue is the typed (D1) (type, value) of an OTLP attribute value
// as an Attribute names it: string, int, bool and double (otelsql.FormatDouble's
// text) are the queryable types; bytes (base64) and arrays, kvlists and
// unset values (canonical JSON) are reported as "bytes" and "json", which no
// equality query accepts.
func CanonicalValue(v *commonpb.AnyValue) (string, string) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return "string", x.StringValue
	case *commonpb.AnyValue_IntValue:
		return "int", strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return "bool", strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return "double", otelsql.FormatDouble(x.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return "bytes", base64.StdEncoding.EncodeToString(x.BytesValue)
	default:
		return "json", string(otelsql.CanonicalJSON(v))
	}
}

// CanonicalAttribute builds the query Attribute matching an exported attribute.
func CanonicalAttribute(scope, key string, v *commonpb.AnyValue) Attribute {
	typ, value := CanonicalValue(v)
	return Attribute{Scope: scope, Key: key, Type: typ, Value: value}
}

// ResourceSpansFromReadOnly converts SDK spans to OTLP, grouping by resource
// and instrumentation scope in first-appearance order.
func ResourceSpansFromReadOnly(spans []sdktrace.ReadOnlySpan) []*tracepb.ResourceSpans {
	type scopeKey struct {
		resource attribute.Distinct
		schema   string
		scope    instrumentation.Scope
	}
	type resourceKey struct {
		resource attribute.Distinct
		schema   string
	}
	var out []*tracepb.ResourceSpans
	resources := map[resourceKey]*tracepb.ResourceSpans{}
	scopes := map[scopeKey]*tracepb.ScopeSpans{}
	for _, s := range spans {
		if s == nil {
			continue
		}
		res := s.Resource()
		rk := resourceKey{res.Equivalent(), res.SchemaURL()}
		rs, ok := resources[rk]
		if !ok {
			rs = &tracepb.ResourceSpans{SchemaUrl: res.SchemaURL()}
			if res != nil {
				rs.Resource = &resourcepb.Resource{Attributes: otlpIterator(res.Iter())}
			}
			resources[rk] = rs
			out = append(out, rs)
		}
		scope := s.InstrumentationScope()
		sk := scopeKey{rk.resource, rk.schema, scope}
		ss, ok := scopes[sk]
		if !ok {
			ss = &tracepb.ScopeSpans{SchemaUrl: scope.SchemaURL}
			if scope != (instrumentation.Scope{}) {
				ss.Scope = &commonpb.InstrumentationScope{Name: scope.Name, Version: scope.Version, Attributes: otlpIterator(scope.Attributes.Iter())}
			}
			scopes[sk] = ss
			rs.ScopeSpans = append(rs.ScopeSpans, ss)
		}
		ss.Spans = append(ss.Spans, otlpSpan(s))
	}
	return out
}

func otlpSpan(s sdktrace.ReadOnlySpan) *tracepb.Span {
	sc := s.SpanContext()
	tid, sid := sc.TraceID(), sc.SpanID()
	st := s.Status()
	out := &tracepb.Span{
		TraceId:                tid[:],
		SpanId:                 sid[:],
		TraceState:             sc.TraceState().String(),
		Name:                   s.Name(),
		Kind:                   otlpSpanKind(s.SpanKind()),
		StartTimeUnixNano:      uint64(max(0, s.StartTime().UnixNano())),
		EndTimeUnixNano:        uint64(max(0, s.EndTime().UnixNano())),
		Attributes:             otlpKeyValues(s.Attributes()),
		DroppedAttributesCount: clampCount(s.DroppedAttributes()),
		DroppedEventsCount:     clampCount(s.DroppedEvents()),
		DroppedLinksCount:      clampCount(s.DroppedLinks()),
		Status:                 &tracepb.Status{Code: otlpStatusCode(st.Code), Message: st.Description},
	}
	parent := s.Parent()
	if psid := parent.SpanID(); psid.IsValid() {
		out.ParentSpanId = psid[:]
	}
	out.Flags = otlpFlags(sc.TraceFlags(), parent)
	for _, ev := range s.Events() {
		out.Events = append(out.Events, &tracepb.Span_Event{Name: ev.Name, TimeUnixNano: uint64(max(0, ev.Time.UnixNano())), Attributes: otlpKeyValues(ev.Attributes), DroppedAttributesCount: clampCount(ev.DroppedAttributeCount)})
	}
	for _, l := range s.Links() {
		ltid, lsid := l.SpanContext.TraceID(), l.SpanContext.SpanID()
		out.Links = append(out.Links, &tracepb.Span_Link{TraceId: ltid[:], SpanId: lsid[:], TraceState: l.SpanContext.TraceState().String(), Attributes: otlpKeyValues(l.Attributes), DroppedAttributesCount: clampCount(l.DroppedAttributeCount), Flags: otlpFlags(l.SpanContext.TraceFlags(), l.SpanContext)})
	}
	return out
}

func otlpFlags(tf trace.TraceFlags, sc trace.SpanContext) uint32 {
	flags := uint32(tf) | uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	if sc.IsRemote() {
		flags |= uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	}
	return flags
}

func clampCount(v int) uint32 {
	if v < 0 {
		return 0
	}
	if int64(v) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func otlpStatusCode(c codes.Code) tracepb.Status_StatusCode {
	switch c {
	case codes.Ok:
		return tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		return tracepb.Status_STATUS_CODE_ERROR
	default:
		return tracepb.Status_STATUS_CODE_UNSET
	}
}

func otlpSpanKind(k trace.SpanKind) tracepb.Span_SpanKind {
	switch k {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

func otlpIterator(it attribute.Iterator) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	for it.Next() {
		kv := it.Attribute()
		out = append(out, &commonpb.KeyValue{Key: string(kv.Key), Value: otlpValue(kv.Value)})
	}
	return out
}

func otlpKeyValues(kvs []attribute.KeyValue) []*commonpb.KeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, &commonpb.KeyValue{Key: string(kv.Key), Value: otlpValue(kv.Value)})
	}
	return out
}

func otlpArray[T any](values []T, item func(T) *commonpb.AnyValue) *commonpb.AnyValue {
	out := make([]*commonpb.AnyValue, len(values))
	for i, v := range values {
		out[i] = item(v)
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: out}}}
}

func otlpValue(v attribute.Value) *commonpb.AnyValue {
	boolValue := func(b bool) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
	}
	intValue := func(n int64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
	}
	doubleValue := func(f float64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
	}
	stringValue := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	switch v.Type() {
	case attribute.BOOL:
		return boolValue(v.AsBool())
	case attribute.INT64:
		return intValue(v.AsInt64())
	case attribute.FLOAT64:
		return doubleValue(v.AsFloat64())
	case attribute.STRING:
		return stringValue(v.AsString())
	case attribute.BOOLSLICE:
		return otlpArray(v.AsBoolSlice(), boolValue)
	case attribute.INT64SLICE:
		return otlpArray(v.AsInt64Slice(), intValue)
	case attribute.FLOAT64SLICE:
		return otlpArray(v.AsFloat64Slice(), doubleValue)
	case attribute.STRINGSLICE:
		return otlpArray(v.AsStringSlice(), stringValue)
	case attribute.BYTESLICE:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: v.AsByteSlice()}}
	case attribute.SLICE:
		return otlpArray(v.AsSlice(), otlpValue)
	case attribute.MAP:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: otlpKeyValues(v.AsMap())}}}
	default:
		return &commonpb.AnyValue{}
	}
}

// Message storage layouts, part of a run's stored layout identity.
const (
	messagesNone     = ""
	messagesDeltas   = "deltas"      // MESSAGES rows; spans carry manifests
	messagesFullText = "fulltext_v2" // plus the message TEXT index and span back references
)
