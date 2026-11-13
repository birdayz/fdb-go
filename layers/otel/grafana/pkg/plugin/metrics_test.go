package plugin

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/relational/api"
)

func counter(c *prometheus.CounterVec, l prometheus.Labels) float64 {
	return testutil.ToFloat64(c.With(l))
}

func TestTraceMetrics(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.reader("T1").trace = func(string) ([]*tracepb.ResourceSpans, otel.TraceReadStats, error) {
		return fullTrace(), otel.TraceReadStats{Spans: 3, Bytes: 9000, DecodedBytes: 4000, Truncated: true, Reason: "max_decoded_bytes,messages"}, nil
	}
	for range 2 {
		onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange))
	}
	l := h.ds.labels(queryTypeTrace, "T1")
	if counter(h.m.readBytes, l) != 18000 || counter(h.m.decodedBytes, l) != 8000 || counter(h.m.candidates, l) != 0 {
		t.Fatalf("bytes %v decoded %v", counter(h.m.readBytes, l), counter(h.m.decodedBytes, l))
	}
	for _, reason := range []string{otel.TraceReasonMaxDecodedBytes, otel.TraceReasonMessages} {
		if n := counter(h.m.truncations, withLabel(l, "reason", reason)); n != 2 {
			t.Fatalf("%s truncations %v", reason, n)
		}
	}
	if n := counter(h.m.truncations, withLabel(l, "reason", otel.TraceReasonMaxSpans)); n != 0 {
		t.Fatalf("max_spans counted %v", n)
	}
	if l["datasource"] != testUID {
		t.Fatalf("labels %v", l)
	}
}

func TestSearchMetrics(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T2")
	r.sameSpan = otel.SpanQueryPage{Spans: []otel.SpanMatch{{Trace: tid(0), ID: "a"}}, Candidates: 7, Bytes: 70}
	r.existential = otel.TraceExistentialResult{Hits: []otel.TraceExistentialHit{{Trace: tid(1)}}, Candidates: 11, Bytes: 110}
	r.queryResult = otel.QueryResult{Hits: []otel.Hit{{Trace: tid(2)}}, Candidates: 13, Bytes: 130}
	r.timeResult = otel.QueryResult{Hits: []otel.Hit{{Trace: tid(3)}}, Candidates: 17, Bytes: 170}
	r.trace = func(id string) ([]*tracepb.ResourceSpans, otel.TraceReadStats, error) {
		return traceOf(id, 1), otel.TraceReadStats{Spans: 2, Bytes: 5, DecodedBytes: 6}, nil
	}
	pred := []any{spanPred("k", "v")}
	for _, m := range []map[string]any{
		searchModel(modeSameSpan, map[string]any{"spanPredicates": pred}),
		searchModel(modeExistential, map[string]any{"spanPredicates": pred}),
		searchModel(modeAttribute, map[string]any{"spanPredicates": pred}),
		searchModel(modeTime, nil),
	} {
		m["tenant"] = "T2"
		onlyFrame(t, h.query(t, m, searchRange))
	}
	l := h.ds.labels(queryTypeTraceSearch, "T2")
	// Four searches, each with one hit summarized by one trace read.
	if c, b, d := counter(h.m.candidates, l), counter(h.m.readBytes, l), counter(h.m.decodedBytes, l); c != 48 || b != 480+4*5 || d != 4*6 {
		t.Fatalf("candidates %v bytes %v decoded %v", c, b, d)
	}
}

func TestReaderReopenMetric(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.reader("T1").stale = 1
	onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange))
	if n := counter(h.m.readerReopens, h.ds.labels(queryTypeTrace, "T1")); n != 1 {
		t.Fatalf("re-opens %v", n)
	}
}

func TestSQLState(t *testing.T) {
	t.Parallel()
	cases := map[error]string{
		api.NewError(api.ErrorCode("0A000"), "refused"):                          "0A000",
		fmt.Errorf("wrapped: %w", api.NewError(api.ErrorCode("54F01"), "limit")): "54F01",
		context.DeadlineExceeded:                                                 "deadline",
		fmt.Errorf("x: %w", context.Canceled):                                    "canceled",
		errors.New("other"):                                                      "unknown",
	}
	for err, want := range cases {
		if got := sqlstate(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}

// Every counter registers on the given registry under its documented name.
func TestMetricsRegister(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	l := prometheus.Labels{"datasource": "d", "query_type": "q", "tenant": "t"}
	m.otelRead(l, 1, 1, 1)
	m.traceRead(l, otel.TraceReadStats{Reason: "max_spans"})
	for _, c := range []*prometheus.CounterVec{m.sqlScannedRows, m.sqlScannedBytes, m.sqlPages, m.readerReopens} {
		c.With(l).Inc()
	}
	m.sqlFailures.With(withLabel(l, "sqlstate", "0A000")).Inc()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"fdb_datasource_sql_scanned_rows_total": true, "fdb_datasource_sql_scanned_bytes_total": true,
		"fdb_datasource_sql_pages_total": true, "fdb_datasource_sql_failures_total": true,
		"fdb_datasource_otel_candidates_total": true, "fdb_datasource_otel_read_bytes_total": true,
		"fdb_datasource_otel_decoded_bytes_total": true, "fdb_datasource_trace_truncations_total": true,
		"fdb_datasource_reader_reopens_total": true,
	}
	for _, f := range families {
		delete(want, f.GetName())
	}
	if len(want) != 0 {
		t.Fatalf("not registered: %v", want)
	}
}

// Instances share the process counters: a second instance (a settings
// change) must not register them again.
func TestProcessMetricsRegisterOnce(t *testing.T) {
	t.Parallel()
	for range 2 {
		inst, err := NewDatasource(context.Background(), instanceSettings(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		inst.(*Datasource).Dispose()
	}
	if processMetrics() != processMetrics() {
		t.Fatal("process metrics built twice")
	}
}
