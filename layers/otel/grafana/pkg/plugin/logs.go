package plugin

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"fdb.dev/layers/otel"
)

func (d *Datasource) queryLogs(ctx context.Context, pc backend.PluginContext, q backend.DataQuery) (backend.DataResponse, error) {
	m, err := parseQuery(q)
	if err != nil {
		return backend.DataResponse{}, err
	}
	tenant, err := d.tenant(m.Tenant)
	if err != nil {
		return backend.DataResponse{}, err
	}
	limit := d.cfg.LogsLimit
	if m.Limit != nil {
		limit = *m.Limit
	}
	if limit < 1 || limit > d.cfg.LogsLimit {
		return backend.DataResponse{}, queryErrorf("limit", "%d is outside 1..%d (logsLimit)", limit, d.cfg.LogsLimit)
	}

	var traceID, spanID string
	var attr otel.Attribute
	switch {
	case m.TraceID != "" && m.Attribute != nil:
		return backend.DataResponse{}, queryErrorf("logs", "set either traceId or attribute, not both")
	case m.TraceID != "":
		if traceID, err = normalizeTraceID("traceId", m.TraceID); err != nil {
			return backend.DataResponse{}, err
		}
		if m.SpanID != "" {
			if spanID, err = normalizeSpanID("spanId", m.SpanID); err != nil {
				return backend.DataResponse{}, err
			}
		}
	case m.Attribute != nil:
		if attr, err = m.Attribute.toAttribute("attribute", signalLog); err != nil {
			return backend.DataResponse{}, err
		}
	default:
		return backend.DataResponse{}, queryErrorf("logs", "set a traceId or an attribute")
	}

	r, release, err := d.readerFor(ctx, tenant)
	if err != nil {
		return backend.DataResponse{}, err
	}
	defer release()
	var recs []otel.LogRecord
	if traceID != "" {
		recs, err = r.QueryLogsByTrace(ctx, traceID, spanID)
	} else {
		recs, err = r.QueryLogsByAttribute(ctx, attr, limit)
	}
	if err != nil {
		return backend.DataResponse{}, err
	}
	var notices []data.Notice
	if len(recs) > limit {
		notices = append(notices, data.Notice{Severity: data.NoticeSeverityWarning, Text: fmt.Sprintf("showing the newest %d of %d log records (limit)", limit, len(recs))})
	}
	frame, err := logFrame(recs, limit)
	if err != nil {
		return backend.DataResponse{}, err
	}
	frame.RefID = q.RefID
	traceField, _ := frame.FieldByName("traceId")
	addLinks(traceField, traceLink(linkTarget(pc, d.inst), tenant))
	frame.AppendNotices(notices...)
	return backend.DataResponse{Frames: data.Frames{frame}}, nil
}

// logFrame is Grafana's data-plane log-lines frame, newest first: timestamp
// (effective time), body (string, else JSON), severity (text, else from the
// OTLP number), id (event_id), labels, plus traceId and spanId.
func logFrame(recs []otel.LogRecord, limit int) (*data.Frame, error) {
	recs = slices.Clone(recs)
	slices.SortStableFunc(recs, func(a, b otel.LogRecord) int {
		if c := cmp.Compare(b.EffectiveTime, a.EffectiveTime); c != 0 {
			return c
		}
		return cmp.Compare(a.EventID, b.EventID)
	})
	if len(recs) > limit {
		recs = recs[:limit]
	}
	n := len(recs)
	ts := make([]time.Time, n)
	bodies := make([]string, n)
	sevs := make([]string, n)
	ids := make([]string, n)
	labels := make([]json.RawMessage, n)
	traceIDs := make([]string, n)
	spanIDs := make([]string, n)
	for i, rec := range recs {
		ts[i] = time.Unix(0, rec.EffectiveTime).UTC()
		bodies[i] = logBody(rec.Body)
		sevs[i] = severity(rec.SeverityText, rec.SeverityNumber)
		ids[i] = rec.EventID
		l, err := json.Marshal(logLabels(rec))
		if err != nil {
			return nil, err
		}
		labels[i] = l
		traceIDs[i] = rec.TraceID
		spanIDs[i] = rec.SpanID
	}
	frame := data.NewFrame("logs",
		data.NewField("timestamp", nil, ts),
		data.NewField("body", nil, bodies),
		data.NewField("severity", nil, sevs),
		data.NewField("id", nil, ids),
		data.NewField("labels", nil, labels),
		data.NewField("traceId", nil, traceIDs),
		data.NewField("spanId", nil, spanIDs),
	)
	frame.Meta = &data.FrameMeta{
		Type:                   data.FrameTypeLogLines,
		TypeVersion:            data.FrameTypeVersion{0, 0},
		PreferredVisualization: data.VisTypeLogs,
	}
	return frame, nil
}

// logBody is a string body as is, anything else as its JSON rendering.
func logBody(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
		return s.StringValue
	}
	b, _ := json.Marshal(anyJSON(v))
	return string(b)
}

// anyJSON renders an AnyValue as plain JSON (bytes as base64).
func anyJSON(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, len(x.ArrayValue.GetValues()))
		for i, e := range x.ArrayValue.GetValues() {
			out[i] = anyJSON(e)
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = anyJSON(kv.GetValue())
		}
		return out
	case nil:
		return nil
	default:
		return attrValue(v)
	}
}

// severity is the record's text, else the OTLP severity-number range name.
func severity(text string, number int32) string {
	if text != "" {
		return text
	}
	switch {
	case number >= 1 && number <= 4:
		return "trace"
	case number >= 5 && number <= 8:
		return "debug"
	case number >= 9 && number <= 12:
		return "info"
	case number >= 13 && number <= 16:
		return "warn"
	case number >= 17 && number <= 20:
		return "error"
	case number >= 21 && number <= 24:
		return "fatal"
	default:
		return ""
	}
}

// logLabels are the resource, scope and record attributes as strings (a
// record attribute wins over a scope one, which wins over a resource one),
// plus event_name.
func logLabels(rec otel.LogRecord) map[string]string {
	out := map[string]string{}
	for _, kvs := range [][]*commonpb.KeyValue{rec.ResourceAttributes, rec.ScopeAttributes, rec.Attributes} {
		for _, kv := range kvs {
			out[kv.GetKey()] = labelValue(kv.GetValue())
		}
	}
	if rec.EventName != "" {
		out["event_name"] = rec.EventName
	}
	return out
}

func labelValue(v *commonpb.AnyValue) string {
	switch x := attrValue(v).(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
