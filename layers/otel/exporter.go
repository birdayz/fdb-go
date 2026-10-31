package otel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"gopkg.in/yaml.v3"
)

var _ sdktrace.SpanExporter = (*Exporter)(nil)

const (
	// Live runs use a fixed synthetic corpus identity so every handle and tenant
	// of one run agrees on the manifest; the seed keeps it distinct from fixtures.
	exporterSeed    uint64 = 0x6f746c706c697665
	exporterCount          = 100000000
	exporterTenants        = exporterCount / 2

	// exportSpanBudget is the conservative per-transaction estimate shared with
	// prepareIngestBatch; the 500KB actual-size guard runs inside the save.
	exportSpanBudget = 300000

	// Posting caps enforced by postingsWithFormat; larger values stay unindexed.
	maxIndexedKeyBytes   = 1024
	maxIndexedValueBytes = 64000

	// fidelityField is an unknown ResourceSpans field holding the original
	// resource/scope/span attribute lists when the indexed copy was canonicalized.
	fidelityField protowire.Number = 536870911
)

// ExporterConfig is shared by the SDK exporter and the Collector exporter.
type ExporterConfig struct {
	ClusterFile         string `json:"cluster_file" yaml:"cluster_file"`
	Run                 string `json:"run" yaml:"run"`
	Tenant              int    `json:"tenant" yaml:"tenant"`
	Order               string `json:"order" yaml:"order"`
	HashBytes           int    `json:"hash_bytes" yaml:"hash_bytes"`
	PostingValueFormat  string `json:"posting_value_format" yaml:"posting_value_format"`
	RecordCompression   string `json:"record_compression" yaml:"record_compression"`
	BulkIndexConflicts  bool   `json:"bulk_index_conflicts" yaml:"bulk_index_conflicts"`
	EnableMessageDeltas bool   `json:"enable_message_deltas" yaml:"enable_message_deltas"`
	// EnableMessageFullText decomposes GenAI messages (as EnableMessageDeltas
	// does) and indexes their text and span back references for search.
	EnableMessageFullText bool  `json:"enable_message_full_text" yaml:"enable_message_full_text"`
	BatchSize             int   `json:"batch_size" yaml:"batch_size"`
	Workers               int   `json:"workers" yaml:"workers"`
	MaxCandidates         int   `json:"max_candidates" yaml:"max_candidates"`
	MaxBytes              int64 `json:"max_bytes" yaml:"max_bytes"`
	K                     int   `json:"k" yaml:"k"`
}

func DefaultExporterConfig() ExporterConfig {
	return ExporterConfig{Order: "time", HashBytes: 8, PostingValueFormat: "tuple", RecordCompression: "brotli-4", BulkIndexConflicts: true, BatchSize: 64, Workers: 8, MaxCandidates: 20000, MaxBytes: 4 << 20, K: 100}
}

// UnmarshalJSON starts from DefaultExporterConfig so omitted fields keep defaults.
func (c *ExporterConfig) UnmarshalJSON(data []byte) error {
	type plain ExporterConfig
	p := plain(DefaultExporterConfig())
	if err := json.Unmarshal(data, &p); err != nil {
		return wrap("config", "exporter JSON", err)
	}
	*c = ExporterConfig(p)
	return nil
}

// UnmarshalYAML starts from DefaultExporterConfig so omitted fields keep defaults.
func (c *ExporterConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain ExporterConfig
	p := plain(DefaultExporterConfig())
	if err := node.Decode(&p); err != nil {
		return wrap("config", "exporter YAML", err)
	}
	*c = ExporterConfig(p)
	return nil
}

func (c ExporterConfig) Validate() error {
	if c.ClusterFile == "" {
		return fail("config", "exporter", "explicit owned FDB cluster file required")
	}
	if c.BatchSize < 1 || c.BatchSize > 64 {
		return fail("config", "exporter", "batch_size must be 1..64")
	}
	if c.Workers < 1 || c.Workers > 16 {
		return fail("config", "exporter", "workers must be 1..16")
	}
	if c.Tenant < 0 || c.Tenant >= exporterTenants {
		return failf("config", "exporter", "tenant must be 0..%d", exporterTenants-1)
	}
	return c.backendConfig().Validate()
}

// backendConfig maps the live exporter onto the batch-save FDB backend. Workers
// is the exporter's own pool; the backend value is unused by live saves.
func (c ExporterConfig) backendConfig() Config {
	return Config{Run: c.Run, Backend: "fdb", Seed: exporterSeed, Count: exporterCount, Tenants: exporterTenants, Attributes: 4, MatchEvery: 1, Batch: c.BatchSize, SaveMode: "batch", RecordCompression: c.RecordCompression, PostingValueFormat: c.PostingValueFormat, BulkIndexConflicts: c.BulkIndexConflicts, Workers: 1, CHBatch: 8192, CHBatchBytes: 8 << 20, CHIndexGranularity: 8192, CHQueryTransport: "http", Tenant: c.Tenant, K: c.K, Iterations: 1, MaxCandidates: c.MaxCandidates, MaxBytes: c.MaxBytes, HashBytes: c.HashBytes, Order: c.Order}
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
	MessageFTS          MessageFTSStats   `json:"message_fts,omitempty"`
}

// SpanNotFoundError reports a LoadSpanOTLP identity absent from the tenant.
type SpanNotFoundError struct {
	*OtelError
	Trace string
	Span  string
}

// Exporter stores OTLP spans as Record Layer Envelope records in a ready run,
// so the FDBBackend query paths serve live data through the same handle.
type Exporter struct {
	cfg      ExporterConfig
	backend  *FDBBackend
	sequence atomic.Int64
	queryMu  sync.Mutex // FDBBackend query work accounting is not concurrency-safe.

	mu     sync.Mutex
	closed bool
	active sync.WaitGroup
	done   chan struct{}
}

func NewExporter(ctx context.Context, cfg ExporterConfig) (*Exporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	b, err := OpenFDBContext(ctx, cfg.backendConfig(), cfg.ClusterFile)
	if err != nil {
		return nil, err
	}
	if err := b.initializeLiveRun(ctx); err != nil {
		b.Close()
		return nil, err
	}
	return &Exporter{cfg: cfg, backend: b, done: make(chan struct{})}, nil
}

// initializeLiveRun publishes a ready manifest for a fresh run or verifies the
// existing one. Concurrent openers conflict on the owner read and retry.
func (b *FDBBackend) initializeLiveRun(ctx context.Context) error {
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		key := b.root.Sub("owner").FDBKey()
		wire, err := rc.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		if len(wire) != 0 {
			if err := b.checkManifestBytes(wire, true); err != nil {
				return nil, err
			}
		} else {
			m, err := json.Marshal(manifest(b.cfg, "ready"))
			if err != nil {
				return nil, err
			}
			rc.Transaction().Set(key, m)
		}
		_, err = b.openConfigured(rc)
		return nil, err
	})
	return err
}

func (e *Exporter) Config() ExporterConfig { return e.cfg }

func (e *Exporter) begin(op string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fail("operation", op, "exporter is shut down")
	}
	e.active.Add(1)
	return nil
}

// Shutdown rejects new work, waits for active calls, then closes the database.
// On context expiry the close still completes once active calls return.
func (e *Exporter) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		go func() {
			e.active.Wait()
			e.backend.Close()
			close(e.done)
		}()
	}
	e.mu.Unlock()
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return wrap("canceled", "exporter shutdown", ctx.Err())
	}
}

// ExportSpans implements sdktrace.SpanExporter.
func (e *Exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	_, err := e.ExportResourceSpans(ctx, ResourceSpansFromReadOnly(spans))
	return err
}

// ConsumeTraces accepts OTLP ResourceSpans from a Collector or OTLP receiver.
func (e *Exporter) ConsumeTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	_, err := e.ExportResourceSpans(ctx, spans)
	return err
}

// ExportResourceSpans normalizes every span before writing, so an inadmissible
// span fails the call without partial writes; batches then commit independently.
func (e *Exporter) ExportResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans) (ExportStats, error) {
	if err := e.begin("export"); err != nil {
		return ExportStats{}, err
	}
	defer e.active.Done()
	var deltaBlobs []*MessageBlob
	var deltaStats MessageDeltaStats
	decompose := e.cfg.EnableMessageDeltas || e.cfg.EnableMessageFullText
	if decompose && len(spans) > 0 {
		var err error
		spans, deltaBlobs, deltaStats, err = DecomposeResourceSpans(ctx, spans, e.cfg.Tenant)
		if err != nil {
			return ExportStats{}, err
		}
	}
	items, stats, err := e.prepareExport(ctx, spans)
	stats.MessageDeltas = deltaStats
	if err != nil {
		return stats, err
	}
	if len(deltaBlobs) > 0 {
		if e.cfg.EnableMessageFullText {
			_, fts, err := PersistAndIndexMessageBlobsDB(ctx, e.backend.db, e.backend.root, e.cfg.Tenant, deltaBlobs)
			stats.MessageFTS = fts
			if err != nil {
				return stats, err
			}
		} else if _, err := PersistMessageBlobsDB(ctx, e.backend.db, e.backend.root, e.cfg.Tenant, deltaBlobs); err != nil {
			return stats, err
		}
	}
	if err = e.commitExport(ctx, partitionExport(items, e.cfg.BatchSize), &stats); err != nil || !e.cfg.EnableMessageFullText {
		return stats, err
	}
	// References follow the committed records; a failed call is repaired by re-export.
	refs, err := IndexMessageSpansDB(ctx, e.backend.db, e.backend.root, e.cfg.Tenant, e.cfg.Order, spans)
	stats.MessageFTS.SpanRefs += refs.SpanRefs
	stats.MessageFTS.Bytes += refs.Bytes
	stats.MessageFTS.Transactions += refs.Transactions
	return stats, err
}

func (e *Exporter) commitExport(ctx context.Context, batches []preparedBatch, stats *ExportStats) error {
	if len(batches) == 0 {
		return nil
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan preparedBatch)
	var mu sync.Mutex
	var firstErr error
	committed := 0
	var workers sync.WaitGroup
	for range min(e.cfg.Workers, len(batches)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				if workCtx.Err() != nil {
					continue
				}
				// saveIngestBatch carries the ingest guards and fresh Runs on 1031.
				err := e.backend.saveIngestBatch(workCtx, batch, nil)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					committed++
					stats.Spans += int64(len(batch.messages))
					stats.Batches++
					stats.LogicalBytes += batch.logicalBytes
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, batch := range batches {
		select {
		case <-workCtx.Done():
			break feed
		case jobs <- batch:
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}
	if committed != len(batches) {
		return wrap("canceled", "export", ctx.Err())
	}
	return nil
}

func partitionExport(items []exportSpan, size int) []preparedBatch {
	var out []preparedBatch
	cur := preparedBatch{}
	for i, it := range items {
		if len(cur.messages) > 0 && (len(cur.messages) == size || cur.estimate+it.cost > exportSpanBudget) {
			out = append(out, cur)
			cur = preparedBatch{first: i, next: i}
		}
		cur.messages = append(cur.messages, it.message)
		cur.logicalBytes += it.logical
		cur.estimate += it.cost
		cur.next = i + 1
	}
	if len(cur.messages) > 0 {
		out = append(out, cur)
	}
	return out
}

type exportSpan struct {
	key        string
	message    proto.Message
	logical    int64
	cost       int
	normalized bool
	unindexed  int64
}

// canonicalSet is one scope's indexed attribute list: unique keys (last value
// wins, first position kept), string/int values, and posting caps enforced.
type canonicalSet struct {
	indexed   []*commonpb.KeyValue
	changed   bool
	unindexed int64
}

func canonicalAttributes(kvs []*commonpb.KeyValue) canonicalSet {
	var out canonicalSet
	var order []string
	latest := make(map[string]*commonpb.KeyValue, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			out.changed = true
			continue
		}
		if _, seen := latest[kv.Key]; seen {
			out.changed = true
		} else {
			order = append(order, kv.Key)
		}
		_, value := CanonicalValue(kv.GetValue())
		if len(kv.Key) > maxIndexedKeyBytes || len(value) > maxIndexedValueBytes {
			out.changed = true
			latest[kv.Key] = nil
			continue
		}
		indexed := kv
		switch kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue, *commonpb.AnyValue_IntValue:
		default:
			// Every non-int canonical value is a string.
			out.changed = true
			indexed = &commonpb.KeyValue{Key: kv.Key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
		}
		latest[kv.Key] = indexed
	}
	for _, key := range order {
		if kv := latest[key]; kv != nil {
			out.indexed = append(out.indexed, kv)
		} else {
			out.unindexed++
		}
	}
	return out
}

// CanonicalValue is the indexed (type, value) of an OTLP attribute value:
// ints stay "int"; bool, double, bytes, arrays and kvlists become "string".
func CanonicalValue(v *commonpb.AnyValue) (string, string) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return "string", x.StringValue
	case *commonpb.AnyValue_IntValue:
		return "int", strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return "string", strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return "string", formatDouble(x.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return "string", base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		return "string", string(appendCanonicalJSON(nil, v))
	default:
		return "string", ""
	}
}

// CanonicalAttribute builds the query Attribute matching an exported attribute.
func CanonicalAttribute(scope, key string, v *commonpb.AnyValue) Attribute {
	typ, value := CanonicalValue(v)
	return Attribute{Scope: scope, Key: key, Type: typ, Value: value}
}

func formatDouble(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func appendJSONString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // strings always encode; invalid UTF-8 becomes U+FFFD.
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

// appendCanonicalJSON keeps array and kvlist order (including repeated keys)
// so the representation is deterministic for equal OTLP values.
func appendCanonicalJSON(dst []byte, v *commonpb.AnyValue) []byte {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return appendJSONString(dst, x.StringValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.AppendInt(dst, x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.AppendBool(dst, x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		if math.IsNaN(x.DoubleValue) || math.IsInf(x.DoubleValue, 0) {
			return appendJSONString(dst, formatDouble(x.DoubleValue))
		}
		return strconv.AppendFloat(dst, x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return appendJSONString(dst, base64.StdEncoding.EncodeToString(x.BytesValue))
	case *commonpb.AnyValue_ArrayValue:
		dst = append(dst, '[')
		for i, item := range x.ArrayValue.GetValues() {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendCanonicalJSON(dst, item)
		}
		return append(dst, ']')
	case *commonpb.AnyValue_KvlistValue:
		dst = append(dst, '{')
		for i, kv := range x.KvlistValue.GetValues() {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, kv.GetKey())
			dst = append(dst, ':')
			dst = appendCanonicalJSON(dst, kv.GetValue())
		}
		return append(dst, '}')
	default:
		return append(dst, "null"...)
	}
}

// prepareExport splits every span into its own single-span ResourceSpans with
// canonical indexed attributes; later duplicates of one identity replace earlier.
func (e *Exporter) prepareExport(ctx context.Context, in []*tracepb.ResourceSpans) ([]exportSpan, ExportStats, error) {
	var stats ExportStats
	var out []exportSpan
	position := map[string]int{}
	for _, rs := range in {
		if rs == nil {
			continue
		}
		resourceSet := canonicalAttributes(rs.GetResource().GetAttributes())
		resource := rs.Resource
		if resourceSet.changed {
			resource = proto.Clone(rs.Resource).(*resourcepb.Resource)
			resource.Attributes = resourceSet.indexed
		}
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			scopeSet := canonicalAttributes(ss.GetScope().GetAttributes())
			scope := ss.Scope
			if scopeSet.changed {
				scope = proto.Clone(ss.Scope).(*commonpb.InstrumentationScope)
				scope.Attributes = scopeSet.indexed
			}
			for _, sp := range ss.Spans {
				if sp == nil {
					continue
				}
				if err := ctx.Err(); err != nil {
					return nil, stats, wrap("canceled", "export preparation", err)
				}
				stats.ReceivedSpans++
				spanSet := canonicalAttributes(sp.Attributes)
				span := sp
				if spanSet.changed {
					span = proto.Clone(sp).(*tracepb.Span)
					span.Attributes = spanSet.indexed
				}
				single := &tracepb.ResourceSpans{Resource: resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{span}}}}
				// Preserve wrapper unknown fields; a foreign fidelity field would be ambiguous.
				rsUnknown, _, _, err := splitFidelity(rs.ProtoReflect().GetUnknown())
				if err != nil {
					return nil, stats, err
				}
				single.ProtoReflect().SetUnknown(rsUnknown)
				single.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
				changed := [3]bool{resourceSet.changed, scopeSet.changed, spanSet.changed}
				if changed != [3]bool{} {
					payload, err := fidelityPayload(changed, rs.GetResource().GetAttributes(), ss.GetScope().GetAttributes(), sp.Attributes)
					if err != nil {
						return nil, stats, err
					}
					unknown := append([]byte(nil), rsUnknown...)
					unknown = protowire.AppendTag(unknown, fidelityField, protowire.BytesType)
					single.ProtoReflect().SetUnknown(protowire.AppendBytes(unknown, payload))
				}
				item, err := e.envelope(single, resourceSet.indexed, scopeSet.indexed, spanSet.indexed)
				if err != nil {
					return nil, stats, err
				}
				item.normalized = changed != [3]bool{}
				item.unindexed = resourceSet.unindexed + scopeSet.unindexed + spanSet.unindexed
				if i, ok := position[item.key]; ok {
					stats.DuplicateSpans++
					out[i] = item
				} else {
					position[item.key] = len(out)
					out = append(out, item)
				}
			}
		}
	}
	for _, item := range out {
		if item.normalized {
			stats.NormalizedSpans++
		}
		stats.UnindexedAttributes += item.unindexed
	}
	return out, stats, nil
}

func fidelityPayload(changed [3]bool, lists ...[]*commonpb.KeyValue) ([]byte, error) {
	arr := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(lists))}
	for i, attrs := range lists {
		arr.Values[i] = &commonpb.AnyValue{}
		if changed[i] {
			arr.Values[i].Value = &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: attrs}}
		}
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(arr)
	if err != nil {
		return nil, wrap("admission", "OTLP attribute fidelity", err)
	}
	return wire, nil
}

// splitFidelity separates the fidelity field from other unknown fields.
func splitFidelity(unknown []byte) (rest, payload []byte, found bool, err error) {
	for len(unknown) > 0 {
		num, typ, n := protowire.ConsumeTag(unknown)
		if n < 0 {
			return nil, nil, false, wrap("corruption", "OTLP unknown fields", protowire.ParseError(n))
		}
		m := protowire.ConsumeFieldValue(num, typ, unknown[n:])
		if m < 0 {
			return nil, nil, false, wrap("corruption", "OTLP unknown fields", protowire.ParseError(m))
		}
		if num == fidelityField && typ == protowire.BytesType {
			payload, _ = protowire.ConsumeBytes(unknown[n:])
			found = true
		} else {
			rest = append(rest, unknown[:n+m]...)
		}
		unknown = unknown[n+m:]
	}
	return rest, payload, found, nil
}

func validSpanIdentity(b []byte, size int) bool {
	if len(b) != size {
		return false
	}
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

func (e *Exporter) envelope(single *tracepb.ResourceSpans, lists ...[]*commonpb.KeyValue) (exportSpan, error) {
	sp := single.ScopeSpans[0].Spans[0]
	if !validSpanIdentity(sp.TraceId, 16) || !validSpanIdentity(sp.SpanId, 8) {
		return exportSpan{}, fail("admission", "OTLP identity", "expected nonzero 16-byte trace and 8-byte span identifiers")
	}
	if sp.StartTimeUnixNano > math.MaxInt64 {
		return exportSpan{}, fail("admission", "OTLP start time", "start_time_unix_nano exceeds int64")
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(single)
	if err != nil {
		return exportSpan{}, wrap("admission", "OTLP encode", err)
	}
	cost := 2048 + len(wire)
	for _, list := range lists {
		for _, kv := range list {
			_, value := CanonicalValue(kv.Value)
			cost += 2 * (len(kv.Key) + len(value) + 256)
		}
	}
	if cost > exportSpanBudget {
		return exportSpan{}, fail("admission", "export batch", "single span exceeds conservative 300KB transaction estimate")
	}
	d, err := envelopeType()
	if err != nil {
		return exportSpan{}, err
	}
	trace, span := hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId)
	m := dynamicpb.NewMessage(d)
	f := d.Fields()
	m.Set(f.ByName("ordinal"), protoreflect.ValueOfInt64(e.sequence.Add(1)))
	m.Set(f.ByName("tenant"), protoreflect.ValueOfInt64(int64(e.cfg.Tenant)))
	m.Set(f.ByName("trace_id"), protoreflect.ValueOfString(trace))
	m.Set(f.ByName("span_id"), protoreflect.ValueOfString(span))
	m.Set(f.ByName("start_ns"), protoreflect.ValueOfInt64(int64(sp.StartTimeUnixNano)))
	m.Set(f.ByName("otlp"), protoreflect.ValueOfBytes(wire))
	return exportSpan{key: trace + span, message: m, logical: int64(len(wire)), cost: cost}, nil
}

// restoreOTLP returns the stored span with its original attribute lists.
func restoreOTLP(wire []byte) (*tracepb.ResourceSpans, error) {
	rs := &tracepb.ResourceSpans{}
	if err := proto.Unmarshal(wire, rs); err != nil {
		return nil, wrap("corruption", "OTLP decode", err)
	}
	if len(rs.ScopeSpans) != 1 || len(rs.ScopeSpans[0].Spans) != 1 {
		return nil, fail("corruption", "OTLP decode", "expected one stored span")
	}
	rest, payload, found, err := splitFidelity(rs.ProtoReflect().GetUnknown())
	if err != nil {
		return nil, err
	}
	rs.ProtoReflect().SetUnknown(rest)
	if !found {
		return rs, nil
	}
	var arr commonpb.ArrayValue
	if err := proto.Unmarshal(payload, &arr); err != nil {
		return nil, wrap("corruption", "OTLP attribute fidelity", err)
	}
	if len(arr.Values) != 3 {
		return nil, fail("corruption", "OTLP attribute fidelity", "expected resource, scope and span lists")
	}
	if kv := arr.Values[0].GetKvlistValue(); kv != nil {
		if rs.Resource == nil {
			rs.Resource = &resourcepb.Resource{}
		}
		rs.Resource.Attributes = kv.Values
	}
	if kv := arr.Values[1].GetKvlistValue(); kv != nil {
		if rs.ScopeSpans[0].Scope == nil {
			rs.ScopeSpans[0].Scope = &commonpb.InstrumentationScope{}
		}
		rs.ScopeSpans[0].Scope.Attributes = kv.Values
	}
	if kv := arr.Values[2].GetKvlistValue(); kv != nil {
		rs.ScopeSpans[0].Spans[0].Attributes = kv.Values
	}
	return rs, nil
}

func (e *Exporter) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	if err := e.begin("query"); err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.Query(ctx, a)
}

func (e *Exporter) Trace(ctx context.Context, traceID string) ([]SpanHit, error) {
	if err := e.begin("trace"); err != nil {
		return []SpanHit{}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.Trace(ctx, traceID)
}

func (e *Exporter) Span(ctx context.Context, spanID string) ([]SpanMatch, error) {
	if err := e.begin("span"); err != nil {
		return []SpanMatch{}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.Span(ctx, spanID)
}

func (e *Exporter) Time(ctx context.Context, interval TimeRange) (QueryResult, error) {
	if err := e.begin("time"); err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.Time(ctx, interval)
}

// LoadSpanOTLP returns one stored span as a single-span ResourceSpans with the
// original (pre-canonicalization) resource, scope and span attributes.
func (e *Exporter) LoadSpanOTLP(ctx context.Context, traceID, spanID string) (*tracepb.ResourceSpans, error) {
	if !validIdentity(traceID, 16) || !validIdentity(spanID, 8) {
		return nil, fail("config", "load span", "expected lowercase hex 16-byte trace and 8-byte span identifiers")
	}
	if err := e.begin("load span"); err != nil {
		return nil, err
	}
	defer e.active.Done()
	var out *tracepb.ResourceSpans
	b := e.backend
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		out = nil
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		record, err := store.LoadRecord(tuple.Tuple{int64(e.cfg.Tenant), traceID, spanID})
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, &SpanNotFoundError{OtelError: &OtelError{Kind: "operation", Op: "load span", Message: "span not found"}, Trace: traceID, Span: spanID}
		}
		if _, err := decodeRecord(record.Record); err != nil {
			return nil, err
		}
		r := record.Record.ProtoReflect()
		out, err = restoreOTLP(r.Get(r.Descriptor().Fields().ByName("otlp")).Bytes())
		if err != nil || !(e.cfg.EnableMessageDeltas || e.cfg.EnableMessageFullText) {
			return nil, err
		}
		reconstructed, err := ReconstructResourceSpans(ctx, rc, b.root, e.cfg.Tenant, []*tracepb.ResourceSpans{out})
		if err != nil {
			return nil, err
		}
		out = reconstructed[0]
		return nil, nil
	})
	return out, err
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
