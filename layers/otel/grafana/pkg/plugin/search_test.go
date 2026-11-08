package plugin

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
)

var searchRange = backend.TimeRange{From: time.Unix(1_700_000_000, 0), To: time.Unix(1_700_003_600, 0)}

func tid(i int) string { return fmt.Sprintf("%032x", i+1) }

// traceOf is a two-span trace: root (no parent) and a later, longer child.
func traceOf(id string, start int64) []*tracepb.ResourceSpans {
	rs := fullTrace()[:2]
	rs[0].ScopeSpans[0].Spans[0].TraceId = idBytes(id)
	rs[0].ScopeSpans[0].Spans[0].StartTimeUnixNano = uint64(start)
	rs[0].ScopeSpans[0].Spans[0].EndTimeUnixNano = uint64(start + 1_000_000)
	rs[1].ScopeSpans[0].Spans[0].TraceId = idBytes(id)
	rs[1].ScopeSpans[0].Spans[0].StartTimeUnixNano = uint64(start + 500_000)
	rs[1].ScopeSpans[0].Spans[0].EndTimeUnixNano = uint64(start + 3_000_000)
	return rs
}

func searchModel(mode string, extra map[string]any) map[string]any {
	m := map[string]any{"queryType": queryTypeTraceSearch, "mode": mode}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func spanPred(k, v string) map[string]any {
	return map[string]any{"scope": "span", "key": k, "type": "string", "value": v}
}

func onlyFrame(t *testing.T, r backend.DataResponse) *data.Frame {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("query failed: %v", r.Error)
	}
	if len(r.Frames) != 1 {
		t.Fatalf("%d frames", len(r.Frames))
	}
	return r.Frames[0]
}

func TestTraceSearchSameSpan(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T1")
	r.sameSpan = otel.SpanQueryPage{Spans: []otel.SpanMatch{
		{Trace: tid(0), ID: "a", Start: 30}, {Trace: tid(1), ID: "b", Start: 20}, {Trace: tid(0), ID: "c", Start: 10},
	}}
	r.trace = func(id string) ([]*tracepb.ResourceSpans, traceReadStats, error) {
		return traceOf(id, 1_000_000_000), traceReadStats{}, nil
	}
	f := onlyFrame(t, h.query(t, searchModel(modeSameSpan, map[string]any{
		"spanPredicates": []any{spanPred("http.method", "GET"), map[string]any{"scope": "resource", "key": "n", "type": "int", "value": "3"}},
		"limit":          7,
	}), searchRange))
	q := r.lastSameSpan
	if len(q.Predicates) != 2 || q.Predicates[1] != (otel.Attribute{Scope: "resource", Key: "n", Type: "int", Value: "3"}) || q.Limit != 7 ||
		q.TimeWindow == nil || *q.TimeWindow != (otel.TimeRange{Start: searchRange.From.UnixNano(), End: searchRange.To.UnixNano()}) {
		t.Fatalf("reader query %+v", q)
	}
	names := []string{}
	for _, fl := range f.Fields {
		names = append(names, fl.Name)
	}
	if strings.Join(names, ",") != "traceID,startTime,matchedSpans,traceService,traceName,traceDuration" {
		t.Fatalf("fields %v", names)
	}
	if n, _ := f.RowLen(); n != 2 {
		t.Fatalf("%d rows", n)
	}
	if f.Fields[0].At(0) != tid(0) || *f.Fields[2].At(0).(*int64) != 2 || *f.Fields[2].At(1).(*int64) != 1 {
		t.Fatalf("grouping: %v %v", f.Fields[0].At(0), *f.Fields[2].At(0).(*int64))
	}
	if svc := f.Fields[3].At(0).(*string); *svc != "checkout" {
		t.Fatalf("root service %v", *svc)
	}
	if name := f.Fields[4].At(0).(*string); *name != "checkout" {
		t.Fatalf("root name %v", *name)
	}
	if d := f.Fields[5].At(0).(*float64); *d != 3 {
		t.Fatalf("duration %v ms", *d)
	}
	if st := f.Fields[1].At(0).(time.Time); st.UnixNano() != 1_000_000_000 {
		t.Fatalf("summary start %v", st)
	}
	link := f.Fields[0].Config.Links[0]
	if link.Internal.DatasourceUID != testUID || link.Internal.Query.(map[string]any)["queryType"] != queryTypeTrace ||
		link.Internal.Query.(map[string]any)["tenant"] != "T1" || link.Internal.Query.(map[string]any)["traceId"] != valueRaw {
		t.Fatalf("trace link %+v", link.Internal)
	}
	if f.Meta.PreferredVisualization != data.VisTypeTable {
		t.Fatalf("meta %+v", f.Meta)
	}
}

// The root is the span without a parent even when a child starts earlier
// (clock skew); the duration spans every span.
func TestSummarizePrefersParentless(t *testing.T) {
	t.Parallel()
	rss := traceOf(tid(0), 10_000_000)
	rss[1].ScopeSpans[0].Spans[0].StartTimeUnixNano = 1_000_000
	rss[1].ScopeSpans[0].Spans[0].Name = "early child"
	rss[0], rss[1] = rss[1], rss[0] // the child comes first
	sum, ok := summarize(rss)
	if !ok || *sum.name != "checkout" || sum.start != 1_000_000 || sum.durationMs != 12 {
		t.Fatalf("summary %+v name %q", sum, *sum.name)
	}
	if _, ok := summarize(nil); ok {
		t.Fatal("empty trace summarized")
	}
}

func TestTraceSearchManyHitsSkipsSummaries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T1")
	for i := range summaryHitLimit + 1 {
		r.timeResult.Hits = append(r.timeResult.Hits, otel.Hit{Trace: tid(i), Latest: int64(100 - i)})
	}
	f := onlyFrame(t, h.query(t, searchModel(modeTime, map[string]any{"limit": 100}), searchRange))
	if len(f.Fields) != 3 {
		t.Fatalf("over %d hits the summary fields must be absent, got %d fields", summaryHitLimit, len(f.Fields))
	}
	for _, c := range r.callLog() {
		if strings.HasPrefix(c, "TraceOTLP") {
			t.Fatalf("read a trace for a summary: %s", c)
		}
	}
	if f.Fields[2].At(0).(*int64) != nil {
		t.Fatal("time mode reports no matched spans")
	}
	if len(f.Meta.Notices) != 2 {
		t.Fatalf("want the K and summary notices, got %+v", f.Meta.Notices)
	}
	if r.lastTime != timeRange(searchRange) {
		t.Fatalf("time window %+v", r.lastTime)
	}
}

func TestTraceSearchExistentialAndAttribute(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T1")
	r.existential = otel.TraceExistentialResult{Hits: []otel.TraceExistentialHit{{
		Trace: tid(0), Latest: 5,
		Witnesses: [][]otel.SpanMatch{{{Trace: tid(0), ID: "a"}, {Trace: tid(0), ID: "b"}}, {{Trace: tid(0), ID: "a"}}},
	}}}
	f := onlyFrame(t, h.query(t, searchModel(modeExistential, map[string]any{
		"spanPredicates": []any{spanPred("k", "v")},
		"logPredicates":  []any{map[string]any{"scope": "event_name", "key": "event_name", "type": "string", "value": "gen_ai.evaluation.result"}, map[string]any{"scope": "log", "key": "lk", "type": "bool", "value": "true"}},
	}), searchRange))
	if *f.Fields[2].At(0).(*int64) != 2 {
		t.Fatalf("matched spans %d, want 2 distinct", *f.Fields[2].At(0).(*int64))
	}
	lc := r.lastExist.LogClauses
	if len(lc) != 2 || lc[0] != (otel.Attribute{Scope: otel.LogScopeIntrinsic, Key: "event_name", Type: "string", Value: "gen_ai.evaluation.result"}) || lc[1].Scope != "log" || r.lastExist.Limit != 20 {
		t.Fatalf("existential query %+v", r.lastExist)
	}

	r.queryResult = otel.QueryResult{Hits: []otel.Hit{{Trace: tid(3), Latest: 9}, {Trace: tid(4), Latest: 8}}}
	f = onlyFrame(t, h.query(t, searchModel(modeAttribute, map[string]any{"spanPredicates": []any{spanPred("k", "v")}, "limit": 1}), searchRange))
	if n, _ := f.RowLen(); n != 1 || f.Fields[0].At(0) != tid(3) || r.lastAttr != (otel.Attribute{Scope: "span", Key: "k", Type: "string", Value: "v"}) {
		t.Fatalf("attribute mode: %d rows, attr %+v", n, r.lastAttr)
	}
}

func TestTraceSearchRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	sp := []any{spanPred("k", "v")}
	lp := []any{map[string]any{"scope": "log", "key": "k", "type": "string", "value": "v"}}
	cases := map[string]map[string]any{
		"unknown mode":              searchModel("regex", nil),
		"no mode":                   searchModel("", nil),
		"sameSpan without preds":    searchModel(modeSameSpan, nil),
		"sameSpan with log preds":   searchModel(modeSameSpan, map[string]any{"spanPredicates": sp, "logPredicates": lp}),
		"existential empty":         searchModel(modeExistential, nil),
		"attribute two preds":       searchModel(modeAttribute, map[string]any{"spanPredicates": append(sp, sp...)}),
		"attribute log pred":        searchModel(modeAttribute, map[string]any{"spanPredicates": sp, "logPredicates": lp}),
		"time with preds":           searchModel(modeTime, map[string]any{"spanPredicates": sp}),
		"limit zero":                searchModel(modeTime, map[string]any{"limit": 0}),
		"limit over max":            searchModel(modeTime, map[string]any{"limit": 1001}),
		"span scope log":            searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{map[string]any{"scope": "log", "key": "k", "type": "string"}}}),
		"bytes type":                searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{map[string]any{"scope": "span", "key": "k", "type": "bytes"}}}),
		"empty key":                 searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{map[string]any{"scope": "span", "type": "string"}}}),
		"event_name as span scope":  searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{map[string]any{"scope": "event_name", "key": "event_name", "type": "string"}}}),
		"event_name wrong type":     searchModel(modeExistential, map[string]any{"logPredicates": []any{map[string]any{"scope": "event_name", "key": "event_name", "type": "int"}}}),
		"disallowed tenant":         searchModel(modeTime, map[string]any{"tenant": "T2"}),
		"uninterpolated tenant var": searchModel(modeTime, map[string]any{"tenant": "$tenant"}),
	}
	h.ds.cfg.AllowedSchemas, _ = parseSchemaPolicy([]string{"T1"})
	for name, m := range cases {
		r := h.query(t, m, searchRange)
		if r.Error == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.readers["T2"]; ok {
		t.Fatalf("a refused tenant was opened: %+v", r)
	}
	if len(h.readers) != 0 {
		t.Fatalf("a refused query opened a reader: %v", h.readers)
	}
}
