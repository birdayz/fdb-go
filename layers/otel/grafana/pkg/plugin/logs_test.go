package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"fdb.dev/layers/otel"
)

func logRecords() []otel.LogRecord {
	return []otel.LogRecord{
		{
			EventID: "e1", EffectiveTime: 1_000, TraceID: tid(0), SpanID: "00000000000000aa", SeverityNumber: 17,
			Body:               &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "plain"}},
			ResourceAttributes: []*commonpb.KeyValue{str("service.name", "svc"), str("shadow", "resource")},
			ScopeAttributes:    []*commonpb.KeyValue{str("shadow", "scope")},
			Attributes:         []*commonpb.KeyValue{str("shadow", "log"), anyKV("n", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 5}})},
			EventName:          "gen_ai.choice",
		},
		{
			EventID: "e2", EffectiveTime: 3_000, SeverityText: "NOTICE", SeverityNumber: 9,
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
				anyKV("b", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1}}}),
			}}}},
		},
		{EventID: "e0", EffectiveTime: 3_000},
	}
}

func TestLogFrame(t *testing.T) {
	t.Parallel()
	f, err := logFrame(logRecords(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.Type != data.FrameTypeLogLines || f.Meta.PreferredVisualization != data.VisTypeLogs {
		t.Fatalf("meta %+v", f.Meta)
	}
	var names []string
	for _, fl := range f.Fields {
		names = append(names, fl.Name+":"+fl.Type().String())
	}
	if got := strings.Join(names, ","); got != "timestamp:[]time.Time,body:[]string,severity:[]string,id:[]string,labels:[]json.RawMessage,traceId:[]string,spanId:[]string" {
		t.Fatalf("fields %s", got)
	}
	col := func(name string) []any {
		fl, _ := f.FieldByName(name)
		out := make([]any, fl.Len())
		for i := range out {
			out[i] = fl.At(i)
		}
		return out
	}
	if ids := col("id"); ids[0] != "e0" || ids[1] != "e2" || ids[2] != "e1" {
		t.Fatalf("order %v: newest first, then event ID", ids)
	}
	if ts := col("timestamp"); !ts[2].(time.Time).Equal(time.Unix(0, 1_000)) {
		t.Fatalf("timestamp %v", ts[2])
	}
	if b := col("body"); b[1] != `{"b":"AQ=="}` || b[2] != "plain" || b[0] != "" {
		t.Fatalf("bodies %q", b)
	}
	if s := col("severity"); s[1] != "NOTICE" || s[2] != "error" || s[0] != "" {
		t.Fatalf("severity %q", s)
	}
	var labels map[string]string
	if err := json.Unmarshal(col("labels")[2].(json.RawMessage), &labels); err != nil {
		t.Fatal(err)
	}
	if labels["shadow"] != "log" || labels["service.name"] != "svc" || labels["n"] != "5" || labels["event_name"] != "gen_ai.choice" {
		t.Fatalf("labels %v", labels)
	}
	if tr := col("traceId"); tr[2] != tid(0) || col("spanId")[2] != "00000000000000aa" {
		t.Fatalf("correlation %v", tr)
	}
}

func TestSeverityRanges(t *testing.T) {
	t.Parallel()
	want := map[int32]string{0: "", 1: "trace", 4: "trace", 5: "debug", 8: "debug", 9: "info", 12: "info", 13: "warn", 16: "warn", 17: "error", 20: "error", 21: "fatal", 24: "fatal", 25: "", -1: ""}
	for n, s := range want {
		if got := severity("", n); got != s {
			t.Errorf("%d: %q, want %q", n, got, s)
		}
	}
	if severity("Custom", 17) != "Custom" {
		t.Error("text must win")
	}
}

func TestLogsQuery(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T1")
	r.logs = logRecords()
	f := onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeLogs, "traceId": strings.ToUpper(tid(0)), "spanId": "00000000000000AA", "limit": 2}, searchRange))
	if r.lastLogsArgs[0] != tid(0) || r.lastLogsArgs[1] != "00000000000000aa" {
		t.Fatalf("reader args %v", r.lastLogsArgs)
	}
	if n, _ := f.RowLen(); n != 2 || len(f.Meta.Notices) != 1 {
		t.Fatalf("limit 2 over 3 records: %d rows, notices %+v", n, f.Meta.Notices)
	}
	tf, _ := f.FieldByName("traceId")
	q := tf.Config.Links[0].Internal.Query.(map[string]any)
	if q["queryType"] != queryTypeTrace || q["traceId"] != valueRaw || q["tenant"] != "T1" {
		t.Fatalf("log → trace link %v", q)
	}

	f = onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeLogs, "attribute": map[string]any{"scope": "event_name", "key": "event_name", "type": "string", "value": "x"}}, searchRange))
	if a := r.lastLogsArgs[0].(otel.Attribute); a.Scope != otel.LogScopeIntrinsic || r.lastLogsArgs[1] != 1000 {
		t.Fatalf("attribute args %v", r.lastLogsArgs)
	}
	if n, _ := f.RowLen(); n != 3 {
		t.Fatalf("%d rows", n)
	}

	// A 64-bit trace ID is left-padded, as the query editor does.
	h.query(t, map[string]any{"queryType": queryTypeLogs, "traceId": "00000000000000ff"}, searchRange)
	if r.lastLogsArgs[0] != strings.Repeat("0", 30)+"ff" || r.lastLogsArgs[1] != "" {
		t.Fatalf("padded args %v", r.lastLogsArgs)
	}
}

func TestLogsQueryRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	attr := map[string]any{"scope": "log", "key": "k", "type": "string", "value": "v"}
	cases := map[string]map[string]any{
		"neither":         {},
		"both":            {"traceId": tid(0), "attribute": attr},
		"bad trace id":    {"traceId": "xyz"},
		"zero trace id":   {"traceId": strings.Repeat("0", 32)},
		"short trace id":  {"traceId": "abc"},
		"bad span id":     {"traceId": tid(0), "spanId": "123"},
		"zero span id":    {"traceId": tid(0), "spanId": strings.Repeat("0", 16)},
		"span scope":      {"attribute": map[string]any{"scope": "span", "key": "k", "type": "string"}},
		"limit over max":  {"traceId": tid(0), "limit": 1001},
		"limit zero":      {"traceId": tid(0), "limit": 0},
		"template var id": {"traceId": "$traceId"},
	}
	for name, m := range cases {
		m["queryType"] = queryTypeLogs
		if r := h.query(t, m, searchRange); r.Error == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(h.readers) != 0 {
		t.Fatalf("a refused query opened a reader")
	}
}
