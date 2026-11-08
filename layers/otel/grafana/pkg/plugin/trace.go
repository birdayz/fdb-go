package plugin

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
)

// Tempo's names (grafana-tempo-datasource trace_transform.go).
const (
	resourceNoServiceName  = "OTLPResourceNoServiceName"
	serviceNameKey         = "service.name"
	serviceNamespaceKey    = "service.namespace"
	serviceNamespaceAltKey = "service.namespace.name"
)

// keyValue, traceLog and traceReference are Tempo's JSON shapes for tags,
// span logs (events) and references (links).
type keyValue struct {
	Value any    `json:"value"`
	Key   string `json:"key"`
}

type traceLog struct {
	Timestamp float64     `json:"timestamp"` // ms
	Fields    []*keyValue `json:"fields"`
	Name      string      `json:"name,omitempty"`
}

type traceReference struct {
	SpanID  string      `json:"spanID"`
	TraceID string      `json:"traceID"`
	Tags    []*keyValue `json:"tags"`
}

func (d *Datasource) queryTrace(ctx context.Context, pc backend.PluginContext, q backend.DataQuery) (backend.DataResponse, error) {
	m, err := parseQuery(q)
	if err != nil {
		return backend.DataResponse{}, err
	}
	tenant, err := d.tenant(m.Tenant)
	if err != nil {
		return backend.DataResponse{}, err
	}
	id, err := normalizeTraceID("traceId", m.TraceID)
	if err != nil {
		return backend.DataResponse{}, err
	}
	var rss []*tracepb.ResourceSpans
	var stats otel.TraceReadStats
	err = d.withReader(ctx, tenant, func(r otelReader) error {
		var err error
		rss, stats, err = r.TraceOTLP(ctx, id, d.traceBudget())
		return err
	})
	if err != nil {
		return backend.DataResponse{}, err
	}
	frame, err := traceFrame(rss)
	if err != nil {
		return backend.DataResponse{}, err
	}
	frame.RefID = q.RefID
	spanField, _ := frame.FieldByName("spanID")
	addLinks(spanField, spanLogsLinks(linkTarget(pc, d.inst), tenant)...)
	if len(rss) == 0 {
		frame.AppendNotices(data.Notice{Severity: data.NoticeSeverityInfo, Text: fmt.Sprintf("trace %s not found in tenant %s", id, tenant)})
	}
	if stats.Truncated || stats.Reason != "" {
		frame.AppendNotices(data.Notice{Severity: data.NoticeSeverityWarning, Text: truncationNotice(stats, d.cfg)})
	}
	return backend.DataResponse{Frames: data.Frames{frame}}, nil
}

// linkTarget is the datasource links point at: the request's.
func linkTarget(pc backend.PluginContext, fallback backend.DataSourceInstanceSettings) backend.DataSourceInstanceSettings {
	if pc.DataSourceInstanceSettings != nil {
		return *pc.DataSourceInstanceSettings
	}
	return fallback
}

func truncationNotice(stats otel.TraceReadStats, cfg *Settings) string {
	var parts []string
	for _, r := range strings.Split(stats.Reason, ",") {
		switch strings.TrimSpace(r) {
		case otel.TraceReasonMaxSpans:
			parts = append(parts, fmt.Sprintf("more than %d spans (traceMaxSpans)", cfg.TraceMaxSpans))
		case otel.TraceReasonMaxDecodedBytes:
			parts = append(parts, fmt.Sprintf("more than %d bytes to return (traceMaxBytes)", cfg.TraceMaxBytes))
		case otel.TraceReasonMaxBytes:
			parts = append(parts, "the stored-bytes read budget was reached")
		case otel.TraceReasonMessages:
			parts = append(parts, "GenAI messages are shown as their stored manifests")
		case "":
		default:
			parts = append(parts, r)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "budget reached")
	}
	return fmt.Sprintf("trace budget reached, %d spans returned (%d bytes read, %d returned): %s",
		stats.Spans, stats.Bytes, stats.DecodedBytes, strings.Join(parts, "; "))
}

// spanEntry is one span with the resource and scope it was exported under.
type spanEntry struct {
	span           *tracepb.Span
	scope          *commonpb.InstrumentationScope
	scopeSchemaURL string
	resource       *resourcepb.Resource
	rsSchemaURL    string
}

// traceFrame is TraceView's frame with Tempo's field set, rows ordered by
// start time, then span ID. Tempo's deviations kept on purpose: bytes values
// render as base64, and dropped counts, flags, entity refs and schema URLs
// appear as otel.* tags instead of vanishing.
func traceFrame(rss []*tracepb.ResourceSpans) (*data.Frame, error) {
	var spans []spanEntry
	for _, rs := range rss {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				spans = append(spans, spanEntry{span: s, scope: ss.GetScope(), scopeSchemaURL: ss.GetSchemaUrl(), resource: rs.GetResource(), rsSchemaURL: rs.GetSchemaUrl()})
			}
		}
	}
	slices.SortStableFunc(spans, func(a, b spanEntry) int {
		if c := cmp.Compare(a.span.GetStartTimeUnixNano(), b.span.GetStartTimeUnixNano()); c != 0 {
			return c
		}
		return strings.Compare(hex.EncodeToString(a.span.GetSpanId()), hex.EncodeToString(b.span.GetSpanId()))
	})

	frame := data.NewFrame("Trace",
		data.NewField("traceID", nil, []string{}),
		data.NewField("spanID", nil, []string{}),
		data.NewField("parentSpanID", nil, []string{}),
		data.NewField("operationName", nil, []string{}),
		data.NewField("serviceName", nil, []string{}),
		data.NewField("serviceNamespace", nil, []string{}),
		data.NewField("kind", nil, []string{}),
		data.NewField("statusCode", nil, []int64{}),
		data.NewField("statusMessage", nil, []string{}),
		data.NewField("instrumentationLibraryName", nil, []string{}),
		data.NewField("instrumentationLibraryVersion", nil, []string{}),
		data.NewField("traceState", nil, []string{}),
		data.NewField("serviceTags", nil, []json.RawMessage{}),
		data.NewField("startTime", nil, []float64{}),
		data.NewField("duration", nil, []float64{}),
		data.NewField("logs", nil, []json.RawMessage{}),
		data.NewField("references", nil, []json.RawMessage{}),
		data.NewField("tags", nil, []json.RawMessage{}),
	)
	frame.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeTrace}
	for _, e := range spans {
		row, err := spanRow(e)
		if err != nil {
			return nil, err
		}
		frame.AppendRow(row...)
	}
	return frame, nil
}

func spanRow(e spanEntry) ([]any, error) {
	s := e.span
	serviceName, serviceNamespace, serviceTags := resourceToProcess(e.resource, e.rsSchemaURL)
	serviceTagsJSON, err := json.Marshal(serviceTags)
	if err != nil {
		return nil, fmt.Errorf("marshal service tags: %w", err)
	}
	tags := append(kvTags(s.GetAttributes()), kvTags(e.scope.GetAttributes())...)
	tags = appendCount(tags, "otel.span.dropped_attributes_count", s.GetDroppedAttributesCount())
	tags = appendCount(tags, "otel.span.dropped_events_count", s.GetDroppedEventsCount())
	tags = appendCount(tags, "otel.span.dropped_links_count", s.GetDroppedLinksCount())
	tags = appendCount(tags, "otel.span.flags", s.GetFlags())
	tags = appendCount(tags, "otel.scope.dropped_attributes_count", e.scope.GetDroppedAttributesCount())
	tags = appendString(tags, "otel.scope.schema_url", e.scopeSchemaURL)
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("marshal span tags: %w", err)
	}
	logsJSON, err := json.Marshal(eventsToLogs(s.GetEvents()))
	if err != nil {
		return nil, fmt.Errorf("marshal span logs: %w", err)
	}
	refsJSON, err := json.Marshal(linksToReferences(s.GetLinks()))
	if err != nil {
		return nil, fmt.Errorf("marshal span links: %w", err)
	}
	start, end := s.GetStartTimeUnixNano(), s.GetEndTimeUnixNano()
	return []any{
		hex.EncodeToString(s.GetTraceId()),
		hex.EncodeToString(s.GetSpanId()),
		hex.EncodeToString(s.GetParentSpanId()),
		s.GetName(),
		serviceName,
		serviceNamespace,
		spanKind(s.GetKind()),
		int64(s.GetStatus().GetCode()),
		s.GetStatus().GetMessage(),
		e.scope.GetName(),
		e.scope.GetVersion(),
		s.GetTraceState(),
		json.RawMessage(serviceTagsJSON),
		float64(start) / 1e6,
		float64(int64(end)-int64(start)) / 1e6,
		json.RawMessage(logsJSON),
		json.RawMessage(refsJSON),
		json.RawMessage(tagsJSON),
	}, nil
}

// resourceToProcess is Tempo's: service name and namespace (with the
// service.namespace.name fallback) and every resource attribute as a tag.
func resourceToProcess(r *resourcepb.Resource, schemaURL string) (string, string, []*keyValue) {
	serviceName := resourceNoServiceName
	var ns, nsAlt string
	var tags []*keyValue
	for _, a := range r.GetAttributes() {
		switch a.GetKey() {
		case serviceNameKey:
			serviceName = a.GetValue().GetStringValue()
		case serviceNamespaceKey:
			ns = a.GetValue().GetStringValue()
		case serviceNamespaceAltKey:
			nsAlt = a.GetValue().GetStringValue()
		}
		tags = append(tags, &keyValue{Key: a.GetKey(), Value: attrValue(a.GetValue())})
	}
	if ns == "" {
		ns = nsAlt
	}
	tags = appendCount(tags, "otel.resource.dropped_attributes_count", r.GetDroppedAttributesCount())
	tags = appendString(tags, "otel.resource.schema_url", schemaURL)
	if refs := r.GetEntityRefs(); len(refs) > 0 {
		type entityRef struct {
			SchemaURL       string   `json:"schema_url,omitempty"`
			Type            string   `json:"type"`
			IDKeys          []string `json:"id_keys"`
			DescriptionKeys []string `json:"description_keys,omitempty"`
		}
		out := make([]entityRef, len(refs))
		for i, ref := range refs {
			out[i] = entityRef{ref.GetSchemaUrl(), ref.GetType(), ref.GetIdKeys(), ref.GetDescriptionKeys()}
		}
		b, _ := json.Marshal(out)
		tags = append(tags, &keyValue{Key: "otel.resource.entity_refs", Value: string(b)})
	}
	return serviceName, ns, tags
}

func kvTags(kvs []*commonpb.KeyValue) []*keyValue {
	tags := make([]*keyValue, len(kvs))
	for i, a := range kvs {
		tags[i] = &keyValue{Key: a.GetKey(), Value: attrValue(a.GetValue())}
	}
	return tags
}

func appendCount(tags []*keyValue, key string, n uint32) []*keyValue {
	if n == 0 {
		return tags
	}
	return append(tags, &keyValue{Key: key, Value: int64(n)})
}

func appendString(tags []*keyValue, key, v string) []*keyValue {
	if v == "" {
		return tags
	}
	return append(tags, &keyValue{Key: key, Value: v})
}

// attrValue is Tempo's getAttributeVal, except that bytes render as base64
// (Tempo shows "").
func attrValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_DoubleValue:
		switch f := x.DoubleValue; {
		case math.IsNaN(f):
			return "NaN"
		case math.IsInf(f, 1):
			return "Inf"
		case math.IsInf(f, -1):
			return "-Inf"
		default:
			return f
		}
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		vals := make([]any, len(x.ArrayValue.GetValues()))
		for i, e := range x.ArrayValue.GetValues() {
			vals[i] = attrValue(e)
		}
		b, _ := json.Marshal(vals)
		return string(b)
	case *commonpb.AnyValue_KvlistValue:
		vals := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			vals[kv.GetKey()] = attrValue(kv.GetValue())
		}
		b, _ := json.Marshal(vals)
		return string(b)
	default:
		return ""
	}
}

func spanKind(k tracepb.Span_SpanKind) string {
	switch k {
	case tracepb.Span_SPAN_KIND_CLIENT:
		return "client"
	case tracepb.Span_SPAN_KIND_SERVER:
		return "server"
	case tracepb.Span_SPAN_KIND_PRODUCER:
		return "producer"
	case tracepb.Span_SPAN_KIND_CONSUMER:
		return "consumer"
	case tracepb.Span_SPAN_KIND_INTERNAL:
		return "internal"
	default:
		return ""
	}
}

// eventsToLogs is Tempo's spanEventsToLogs; nil (JSON null) for no events.
func eventsToLogs(events []*tracepb.Span_Event) []*traceLog {
	if len(events) == 0 {
		return nil
	}
	logs := make([]*traceLog, 0, len(events))
	for _, ev := range events {
		fields := appendCount(kvTags(ev.GetAttributes()), "otel.dropped_attributes_count", ev.GetDroppedAttributesCount())
		logs = append(logs, &traceLog{Timestamp: float64(ev.GetTimeUnixNano()) / 1e6, Fields: fields, Name: ev.GetName()})
	}
	return logs
}

// linksToReferences is Tempo's spanLinksToReferences; nil for no links.
func linksToReferences(links []*tracepb.Span_Link) []*traceReference {
	if len(links) == 0 {
		return nil
	}
	refs := make([]*traceReference, 0, len(links))
	for _, l := range links {
		tags := kvTags(l.GetAttributes())
		tags = appendString(tags, "otel.trace_state", l.GetTraceState())
		tags = appendCount(tags, "otel.flags", l.GetFlags())
		tags = appendCount(tags, "otel.dropped_attributes_count", l.GetDroppedAttributesCount())
		refs = append(refs, &traceReference{TraceID: hex.EncodeToString(l.GetTraceId()), SpanID: hex.EncodeToString(l.GetSpanId()), Tags: tags})
	}
	return refs
}
