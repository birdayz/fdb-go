package otel

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// The log tests of relLogStore on OTEL_V1 tenant stores (RFC §2.4): export,
// queries, replacement, fidelity, admission and corruption. One FDB
// testcontainer serves every subtest; each opens a fresh store.

// newLRHarness is newQRHarness that also returns the raw database, for
// transactor injection.
func newLRHarness(t *testing.T) (*qrHarness, fdb.Database) {
	t.Helper()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := fdb.APIVersion(730); err != nil {
		t.Fatal(err)
	}
	sqldriver.RegisterDomainIfNotExists(relDomain)
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", cluster))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setup.Close() })
	if err := otelsql.CreateTemplate(ctx, setup); err != nil {
		t.Fatal(err)
	}
	raw, err := fdb.OpenDatabase(cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	return &qrHarness{db: recordlayer.NewFDBDatabase(raw), setup: setup, keys: map[string]*memoryContinuationKeys{}}, raw
}

func TestRelLogsRealFDB(t *testing.T) {
	t.Parallel()
	h, raw := newLRHarness(t)
	for name, test := range map[string]func(*testing.T, *qrHarness){
		"export_and_query":         testRelLogsExportQuery,
		"replace_and_retract":      testRelLogsReplace,
		"large_split":              testRelLogsLarge,
		"admission_and_isolation":  testRelLogsAdmissionIsolation,
		"budget":                   testRelLogsBudget,
		"batching_and_otlp":        testRelLogsBatching,
		"otlp_fidelity":            testRelLogsFidelity,
		"typed_and_ties":           testRelLogsTyped,
		"existential_log_clauses":  testRelLogExistential,
		"index_key_model":          testRelLogIndexKeyModel,
		"boundary_admission":       testRelLogBoundaryAdmission,
		"persisted_disagreement":   testRelLogPersistedDisagreement,
		"native_cost_whole_call":   testRelLogNativeCostWholeCall,
		"replacement_prior_cost":   testRelLogReplacementPriorCost,
		"expanded_write_admission": testRelLogExpandedWrite,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, h)
		})
	}
	t.Run("prior_change_after_admission", func(t *testing.T) {
		t.Parallel()
		testRelLogPriorChangeAfterAdmission(t, h, raw)
	})
}

// testRelLogsExportQuery exports logs and queries them by trace, span and
// attribute, with the observed- and receiver-time fallbacks. The double score
// is typed (D1); LOGS_TRACE holds an entry for every log, the uncorrelated
// ones under NULL (D11).
func testRelLogsExportQuery(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	T, T2 := qaID(0x100, 16), qaID(0x200, 16)
	S1, S2 := qaID(0x11, 8), qaID(0x12, 8)
	eval := exKV("gen_ai.evaluation.name", exStr("relevance"))
	in := []LogRecord{
		{EventID: "e1", Timestamp: 300, TraceID: T, SpanID: S1, SeverityText: "INFO", SeverityNumber: 9, EventName: "gen_ai.evaluation.result", Body: exStr("good answer"), Attributes: []*commonpb.KeyValue{eval, exKV("gen_ai.evaluation.score.value", exDouble(0.8))}, ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("agent"))}, ScopeName: "genai", ScopeVersion: "1"},
		{EventID: "e2", Timestamp: 100, TraceID: T, SpanID: S2, Attributes: []*commonpb.KeyValue{eval}},
		{Timestamp: 200, TraceID: T, SpanID: S1, EventName: "gen_ai.client.inference.operation.details", Attributes: []*commonpb.KeyValue{exKV("gen_ai.input.messages", exStr(`[{"role":"user","content":"hi"}]`))}},
		{EventID: "e4", ObservedTimestamp: 250, TraceID: T, Attributes: []*commonpb.KeyValue{exKV("kind", exStr("trace-only"))}},
		{EventID: "e5", Timestamp: 400, Attributes: []*commonpb.KeyValue{eval}, ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("evaluator"))}},
		{Attributes: []*commonpb.KeyValue{exKV("kind", exStr("receiver-time"))}},
		{EventID: "e7", Timestamp: 500, TraceID: T2, Attributes: []*commonpb.KeyValue{exKV("gen_ai.evaluation.name", exStr(collision("relevance"))), exKV(collision("gen_ai.evaluation.name"), exStr("relevance"))}},
	}
	before := time.Now().UnixNano()
	stats, err := s.ExportLogs(lrCtx(ctx), in)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReceivedLogs != 7 || stats.Logs != 7 || stats.GeneratedIDs != 2 || stats.Correlated != 5 || stats.Replaced != 0 || stats.DuplicateLogs != 0 || stats.Batches < 1 || stats.LogicalBytes <= 0 || stats.StoredBytes <= 0 {
		t.Fatalf("stats: %+v", stats)
	}
	ids := stats.EventIDs
	if len(ids) != 7 || ids[0] != "e1" || len(ids[2]) != 32 || len(ids[5]) != 32 || ids[2] == ids[5] {
		t.Fatalf("event IDs: %v", ids)
	}
	want := make([]LogRecord, len(in))
	for i, l := range in {
		l.EventID = ids[i]
		switch {
		case l.Timestamp != 0:
			l.EffectiveTime, l.TimeSource = l.Timestamp, LogTimeEvent
		case l.ObservedTimestamp != 0:
			l.EffectiveTime, l.TimeSource = l.ObservedTimestamp, LogTimeObserved
		default:
			l.TimeSource = LogTimeReceiver
		}
		want[i] = l
	}

	byTrace, err := s.QueryLogsByTrace(ctx, T, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := lgIDs(byTrace); !reflect.DeepEqual(got, []string{"e2", ids[2], "e4", "e1"}) {
		t.Fatalf("trace logs in time order: %v", got)
	}
	lgRequireLog(t, "e1 round trip", byTrace[3], want[0])
	lgRequireLog(t, "generated-ID round trip", byTrace[1], want[2])
	if byTrace[2].TimeSource != LogTimeObserved || byTrace[2].Timestamp != 0 || byTrace[2].EffectiveTime != 250 {
		t.Fatalf("observed-time fallback: %+v", byTrace[2])
	}
	for _, c := range []struct {
		trace, span string
		want        []string
	}{
		{T, S1, []string{ids[2], "e1"}},
		{T, S2, []string{"e2"}},
		{strings.ToUpper(T), "", []string{"e2", ids[2], "e4", "e1"}},
		{T, strings.ToUpper(S1), []string{ids[2], "e1"}},
		{T2, "", []string{"e7"}},
		{qaID(0x999, 16), "", []string{}},
		{T, qaID(0x77, 8), []string{}},
	} {
		got, err := s.QueryLogsByTrace(ctx, c.trace, c.span)
		if err != nil || !reflect.DeepEqual(lgIDs(got), c.want) || got == nil {
			t.Fatalf("logs for %s/%s: %v %v want %v", c.trace, c.span, lgIDs(got), err, c.want)
		}
	}

	relevance := Attribute{LogScopeRecord, "gen_ai.evaluation.name", "string", "relevance"}
	for _, c := range []struct {
		a     Attribute
		limit int
		want  []string
	}{
		{relevance, 0, []string{"e5", "e1", "e2"}},
		{relevance, 2, []string{"e5", "e1"}},
		{Attribute{LogScopeRecord, "gen_ai.evaluation.name", "string", collision("relevance")}, 0, []string{"e7"}},
		// D1: the double is typed; v1 matched its canonical string "0.8".
		{Attribute{LogScopeRecord, "gen_ai.evaluation.score.value", "double", "0.8"}, 0, []string{"e1"}},
		{Attribute{LogScopeRecord, "gen_ai.evaluation.score.value", "string", "0.8"}, 0, []string{}},
		{Attribute{LogScopeIntrinsic, "event_name", "string", "gen_ai.client.inference.operation.details"}, 0, []string{ids[2]}},
		{Attribute{"resource", "service.name", "string", "evaluator"}, 0, []string{"e5"}},
		{Attribute{LogScopeRecord, "kind", "string", "receiver-time"}, 0, []string{ids[5]}},
		{Attribute{"resource", "gen_ai.evaluation.name", "string", "relevance"}, 0, []string{}},
		{Attribute{LogScopeRecord, "gen_ai.evaluation.name", "string", "toxicity"}, 0, []string{}},
		{Attribute{LogScopeIntrinsic, "severity_text", "string", "INFO"}, 0, []string{}},
	} {
		got, err := s.QueryLogsByAttribute(ctx, c.a, c.limit)
		if err != nil || got == nil || !reflect.DeepEqual(lgIDs(got), c.want) {
			t.Fatalf("logs by %+v limit %d: %v %v want %v", c.a, c.limit, lgIDs(got), err, c.want)
		}
	}
	got, err := s.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "kind", "string", "receiver-time"}, 0)
	if err != nil || got[0].TimeSource != LogTimeReceiver || got[0].EffectiveTime < before || got[0].EffectiveTime > time.Now().UnixNano() || got[0].Timestamp != 0 || got[0].ObservedTimestamp != 0 {
		t.Fatalf("receiver-time fallback: %+v %v", got, err)
	}
	uncorrelated, err := s.QueryLogsByAttribute(ctx, relevance, 1)
	if err != nil || uncorrelated[0].TraceID != "" {
		t.Fatalf("uncorrelated log by attribute: %+v %v", uncorrelated, err)
	}
	lgRequireLog(t, "uncorrelated round trip", uncorrelated[0], want[4])

	for name, err := range map[string]error{
		"bad trace": func() error { _, err := s.QueryLogsByTrace(ctx, "abc", ""); return err }(),
		"bad span":  func() error { _, err := s.QueryLogsByTrace(ctx, T, "zz"); return err }(),
		"zero span": func() error { _, err := s.QueryLogsByTrace(ctx, T, strings.Repeat("0", 16)); return err }(),
		"span scope": func() error {
			_, err := s.QueryLogsByAttribute(ctx, Attribute{"span", "k", "string", "v"}, 0)
			return err
		}(),
		"negative limit": func() error { _, err := s.QueryLogsByAttribute(ctx, relevance, -1); return err }(),
		"noncanonical int": func() error {
			_, err := s.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "k", "int", "+1"}, 0)
			return err
		}(),
	} {
		requireTyped[*ConfigError](t, name, err)
	}
	if n := lrCensus(t, ctx, s, logsTraceIndex); n != 7 {
		t.Fatalf("LOGS_TRACE census %d, want an entry for each of 7 logs (D11)", n)
	}
}

// testRelLogsReplace: a re-exported log replaces its row and retracts the old
// version's LOGS index entries exactly, the later duplicate in a call wins,
// and replaying an identical log is idempotent.
func testRelLogsReplace(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	T, T2, S := qaID(0x300, 16), qaID(0x301, 16), qaID(0x31, 8)
	oldAttr := Attribute{LogScopeRecord, "phase", "string", "draft"}
	newAttr := Attribute{LogScopeRecord, "phase", "string", "final"}
	if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "r1", Timestamp: 10, TraceID: T, SpanID: S, EventName: "old", Attributes: []*commonpb.KeyValue{exKV("phase", exStr("draft")), exKV("a", exInt(1))}}}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "r1", Timestamp: 20, TraceID: T2, Attributes: []*commonpb.KeyValue{exKV("phase", exStr("final"))}}})
	if err != nil || stats.Replaced != 1 || stats.Logs != 1 {
		t.Fatalf("replace: %+v %v", stats, err)
	}
	for _, c := range []struct {
		trace string
		want  []string
	}{{T, []string{}}, {T2, []string{"r1"}}} {
		got, err := s.QueryLogsByTrace(ctx, c.trace, "")
		if err != nil || !reflect.DeepEqual(lgIDs(got), c.want) {
			t.Fatalf("trace %s after replace: %v %v", c.trace, lgIDs(got), err)
		}
	}
	for _, c := range []struct {
		a    Attribute
		want []string
	}{
		{oldAttr, []string{}},
		{newAttr, []string{"r1"}},
		{Attribute{LogScopeIntrinsic, "event_name", "string", "old"}, []string{}},
		{Attribute{LogScopeRecord, "a", "int", "1"}, []string{}},
	} {
		got, err := s.QueryLogsByAttribute(ctx, c.a, 0)
		if err != nil || !reflect.DeepEqual(lgIDs(got), c.want) {
			t.Fatalf("attribute %+v after replace: %v %v", c.a, lgIDs(got), err)
		}
	}
	// The old version's entries were retracted exactly: one of each single
	// index (D11: NULL event name) and the new string attribute.
	wantCensus := map[string]int64{logsRecordType: 1, logsTraceIndex: 1, logsTimeIndex: 1, logsEventIndex: 1, logsStrAttrIndex: 1, logsLongStrIndex: 0, logsIntAttrIndex: 0, logsDoubleAttrIndex: 0, logsBoolAttrIndex: 0}
	if got := lrCensusAll(t, ctx, s); !reflect.DeepEqual(got, wantCensus) {
		t.Fatalf("census after replace: %v", got)
	}
	// Within one call the later duplicate wins.
	stats, err = s.ExportLogs(lrCtx(ctx), []LogRecord{
		{EventID: "d1", Timestamp: 1, Attributes: []*commonpb.KeyValue{exKV("v", exStr("one"))}},
		{EventID: "d1", Timestamp: 2, Attributes: []*commonpb.KeyValue{exKV("v", exStr("two"))}},
	})
	if err != nil || stats.DuplicateLogs != 1 || stats.Logs != 1 || !reflect.DeepEqual(stats.EventIDs, []string{"d1", "d1"}) {
		t.Fatalf("duplicate stats: %+v %v", stats, err)
	}
	for v, want := range map[string][]string{"one": {}, "two": {"d1"}} {
		got, err := s.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "v", "string", v}, 0)
		if err != nil || !reflect.DeepEqual(lgIDs(got), want) {
			t.Fatalf("duplicate %s: %v %v", v, lgIDs(got), err)
		}
	}
	// Replaying an identical log is idempotent.
	replay := LogRecord{EventID: "d1", Timestamp: 2, Attributes: []*commonpb.KeyValue{exKV("v", exStr("two"))}}
	if stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{replay}); err != nil || stats.Replaced != 1 {
		t.Fatalf("replay: %+v %v", stats, err)
	}
	if n := lrCensus(t, ctx, s, logsStrAttrIndex); n != 2 {
		t.Fatalf("string entries after replay: %d", n)
	}
}

// testRelLogsLarge: a large log is a Record Layer split record, StoredBytes
// is the serialized (zlib-1) record bytes (§2.4), and shrinking a split log
// clears its stale chunks.
func testRelLogsLarge(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	T := qaID(0x400, 16)
	body := lgRandom(t, 300000)
	big := LogRecord{EventID: "big", Timestamp: 7, TraceID: T, Body: exBytes(body), Attributes: []*commonpb.KeyValue{exKV("size", exStr("large"))}}
	stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{big, {EventID: "zip", Timestamp: 8, TraceID: T, Body: exStr(strings.Repeat("compressible ", 50000))}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.StoredBytes >= stats.LogicalBytes || stats.StoredBytes < int64(len(body)) {
		t.Fatalf("zlib-1 must shrink the compressible log only: %+v", stats)
	}
	if n := lrCensus(t, ctx, s, logsRecordType); n != 4+1 {
		t.Fatalf("record keys %d, want 4 split chunks and one unsplit row", n)
	}
	if rec := lrRow(t, ctx, s, "big"); rec == nil || !rec.Split || rec.KeyCount != 4 {
		t.Fatalf("the incompressible log is split: %+v", rec)
	}
	got, err := s.QueryLogsByTrace(ctx, T, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("large logs: %v %v", lgIDs(got), err)
	}
	if !bytes.Equal(got[0].Body.GetBytesValue(), body) || got[1].Body.GetStringValue() != strings.Repeat("compressible ", 50000) {
		t.Fatal("large body round trip")
	}
	// Shrinking a split log clears its stale chunks.
	if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "big", Timestamp: 7, TraceID: T, Body: exStr("small")}}); err != nil {
		t.Fatal(err)
	}
	if n := lrCensus(t, ctx, s, logsRecordType); n != 2 {
		t.Fatalf("record keys after shrink %d want 2", n)
	}
	got, err = s.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "size", "string", "large"}, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("stale large entry: %v %v", lgIDs(got), err)
	}
}

// testRelLogsAdmissionIsolation: a refused or canceled call writes nothing,
// and another tenant's store sees none of a tenant's logs.
func testRelLogsAdmissionIsolation(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	T := qaID(0x500, 16)
	_, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "ok", Timestamp: 1, TraceID: T}, {EventID: "bad", TraceID: "nothex"}})
	requireTyped[*AdmissionError](t, "mixed batch", err)
	for name, n := range lrCensusAll(t, ctx, s) {
		if n != 0 {
			t.Fatalf("rejected call wrote %d %s keys", n, name)
		}
	}
	a := Attribute{LogScopeRecord, "k", "string", "v"}
	if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "ok", Timestamp: 1, TraceID: T, Attributes: []*commonpb.KeyValue{exKV("k", exStr("v"))}}}); err != nil {
		t.Fatal(err)
	}
	h.create(t, ctx, s.cfg.Run, 12)
	other := h.logStore(t, ctx, s.cfg.Run, 12, nil)
	if got, err := other.QueryLogsByTrace(ctx, T, ""); err != nil || len(got) != 0 {
		t.Fatalf("tenant isolation by trace: %v %v", lgIDs(got), err)
	}
	if got, err := other.QueryLogsByAttribute(ctx, a, 0); err != nil || len(got) != 0 {
		t.Fatalf("tenant isolation by attribute: %v %v", lgIDs(got), err)
	}
	if got, err := s.QueryLogsByAttribute(ctx, a, 0); err != nil || !reflect.DeepEqual(lgIDs(got), []string{"ok"}) {
		t.Fatalf("owner tenant: %v %v", lgIDs(got), err)
	}
	// A canceled call writes nothing.
	canceled, stop := context.WithCancel(ctx)
	stop()
	_, err = s.ExportLogs(lrCtx(canceled), []LogRecord{{EventID: "late", Timestamp: 1}})
	requireOtelKind(t, err, "canceled")
	if n := lrCensus(t, ctx, s, logsRecordType); n != 1 {
		t.Fatalf("records after a canceled call: %d", n)
	}
}

// testRelLogsBudget: a candidate is one index entry or one record load; a
// budget failure returns an empty result, and an empty trace fits any budget.
func testRelLogsBudget(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, func(c *ExporterConfig) { c.MaxCandidates = 2 })
	T := qaID(0x600, 16)
	var logs []LogRecord
	for i := range 3 {
		logs = append(logs, LogRecord{EventID: fmt.Sprint("b", i), Timestamp: int64(10 + i), TraceID: T, Attributes: []*commonpb.KeyValue{exKV("k", exStr("v"))}})
	}
	if _, err := s.ExportLogs(lrCtx(ctx), logs); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryLogsByTrace(ctx, T, "")
	requireOtelBudget(t, err)
	if got == nil || len(got) != 0 {
		t.Fatalf("a budget failure returns an empty result: %v", got)
	}
	a := Attribute{LogScopeRecord, "k", "string", "v"}
	_, err = s.QueryLogsByAttribute(ctx, a, 0)
	requireOtelBudget(t, err)
	// One entry plus one record fits the two-candidate budget.
	got, err = s.QueryLogsByAttribute(ctx, a, 1)
	if err != nil || !reflect.DeepEqual(lgIDs(got), []string{"b2"}) {
		t.Fatalf("bounded attribute query: %v %v", lgIDs(got), err)
	}
	bytesBound := h.logStore(t, ctx, s.cfg.Run, s.cfg.Tenant, func(c *ExporterConfig) { c.MaxBytes = 64 })
	_, err = bytesBound.QueryLogsByAttribute(ctx, a, 1)
	requireOtelBudget(t, err)
	if got, err := bytesBound.QueryLogsByTrace(ctx, qaID(0x601, 16), ""); err != nil || len(got) != 0 {
		t.Fatalf("an empty trace fits any budget: %v %v", got, err)
	}
}

// testRelLogsBatching exports logs in batches and through ConsumeLogs, the
// exporter's adapter onto LogRecordsFromResourceLogs and ExportLogs.
func testRelLogsBatching(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, func(c *ExporterConfig) { c.BatchSize, c.Workers = 8, 4 })
	T := qaID(0x700, 16)
	var logs []LogRecord
	for i := range 200 {
		logs = append(logs, LogRecord{Timestamp: int64(1000 + i), TraceID: T, SpanID: qaID(i%5, 8), Attributes: []*commonpb.KeyValue{exKV("bulk", exStr("yes"))}})
	}
	stats, err := s.ExportLogs(lrCtx(ctx), logs)
	if err != nil || stats.Logs != 200 || stats.Batches != 25 || stats.GeneratedIDs != 200 {
		t.Fatalf("batched export: %+v %v", stats, err)
	}
	got, err := s.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "bulk", "string", "yes"}, 200)
	if err != nil || len(got) != 200 || got[0].EffectiveTime != 1199 || got[199].EffectiveTime != 1000 {
		t.Fatalf("bulk by attribute: %d %v", len(got), err)
	}
	perSpan, err := s.QueryLogsByTrace(ctx, T, qaID(3, 8))
	if err != nil || len(perSpan) != 40 {
		t.Fatalf("per-span logs: %d %v", len(perSpan), err)
	}
	for i := 1; i < len(perSpan); i++ {
		if perSpan[i-1].EffectiveTime >= perSpan[i].EffectiveTime {
			t.Fatal("per-span logs out of time order")
		}
	}

	otlp := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: 5, TraceId: qaBytes(t, qaID(0x701, 16)), Body: exStr("from collector")}}}}}}
	if _, err := s.ExportLogs(lrCtx(ctx), LogRecordsFromResourceLogs(otlp)); err != nil {
		t.Fatal(err)
	}
	fromCollector, err := s.QueryLogsByTrace(ctx, qaID(0x701, 16), "")
	if err != nil || len(fromCollector) != 1 || fromCollector[0].Body.GetStringValue() != "from collector" {
		t.Fatalf("ConsumeLogs: %+v %v", fromCollector, err)
	}
}

// testRelLogsFidelity: logs read back from persisted rows keep their OTLP,
// and bool and int attributes match as typed values (D1).
func testRelLogsFidelity(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	in := logFidelityFixture(t)
	// Exercise sibling extraction through the public receiver, too.
	in[4].ScopeLogs[0].LogRecords = append(in[4].ScopeLogs[0].LogRecords, proto.Clone(in[4].ScopeLogs[0].LogRecords[0]).(*logspb.LogRecord))
	in[4].ScopeLogs[0].LogRecords[1].Attributes[0].Value = exStr("sibling")
	in[4].ScopeLogs[0].LogRecords[1].TimeUnixNano = 999
	stats, err := s.ExportLogs(lrCtx(ctx), LogRecordsFromResourceLogs(in))
	if err != nil {
		t.Fatal(err)
	}
	// One oversize value per log is left out; the bool is indexed (D1).
	if stats.Logs != 7 || stats.UnindexedAttributes != 7 {
		t.Fatalf("stats: %+v", stats)
	}
	// A second handle proves the result comes from persisted rows, not ingest state.
	reader := h.logStore(t, ctx, s.cfg.Run, s.cfg.Tenant, nil)
	for _, rl := range in {
		for _, rec := range rl.ScopeLogs[0].LogRecords {
			name := rec.Attributes[0].Value.GetStringValue()
			got, err := reader.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "case", "string", name}, 10)
			if err != nil || len(got) != 1 {
				t.Fatalf("case %s: got %d logs, error %v", name, len(got), err)
			}
			want := proto.Clone(rl).(*logspb.ResourceLogs)
			want.ScopeLogs[0].LogRecords = []*logspb.LogRecord{proto.Clone(rec).(*logspb.LogRecord)}
			requireLogFidelity(t, got[0], want)
			if got[0].EventID == "" {
				t.Fatal("receiver did not assign event identity")
			}
		}
	}
	// Re-exporting a read record retains the original, not mutated projections.
	loaded, err := reader.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "case", "string", "populated"}, 10)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("re-export fixture: got %d logs, error %v", len(loaded), err)
	}
	loaded[0].Body.Value = &commonpb.AnyValue_StringValue{StringValue: "projection edit"}
	loaded[0].Attributes[0].Value = exStr("projection edit")
	stats, err = s.ExportLogs(lrCtx(ctx), loaded)
	if err != nil || stats.Replaced != 1 {
		t.Fatalf("re-export retained envelope: %+v, error %v", stats, err)
	}
	loaded, err = reader.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "case", "string", "populated"}, 10)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("re-export lost source projection: got %d logs, error %v", len(loaded), err)
	}
	want := proto.Clone(in[4]).(*logspb.ResourceLogs)
	want.ScopeLogs[0].LogRecords = want.ScopeLogs[0].LogRecords[:1]
	requireLogFidelity(t, loaded[0], want)
	// The returned envelope is a detached copy.
	envelope, err := loaded[0].OTLP()
	if err != nil {
		t.Fatal(err)
	}
	envelope.ScopeLogs[0].LogRecords[0].Body = exStr("mutated copy")
	requireLogFidelity(t, loaded[0], want)

	byTrace, err := reader.QueryLogsByTrace(ctx, qaID(0xf200, 16), qaID(0xf201, 8))
	if err != nil || len(byTrace) != 6 {
		t.Fatalf("trace projection: got %d logs, error %v", len(byTrace), err)
	}
	for _, l := range byTrace {
		if l.Attributes[0].Value.GetStringValue() == "zero_ids" {
			t.Fatal("all-zero source identity incorrectly indexed")
		}
	}
	for _, check := range []struct {
		attribute Attribute
		count     int
	}{
		{Attribute{LogScopeRecord, "duplicate", "string", "first"}, 0},
		{Attribute{LogScopeRecord, "duplicate", "string", "last"}, 7},
		{Attribute{LogScopeRecord, "bool", "bool", "true"}, 7},
		{Attribute{LogScopeRecord, "bool", "string", "true"}, 0},
		{Attribute{"resource", "resource-key", "bool", "true"}, 2},
		{Attribute{"scope", "scope-key", "int", "5"}, 2},
	} {
		got, err := reader.QueryLogsByAttribute(ctx, check.attribute, 10)
		if err != nil || len(got) != check.count {
			t.Fatalf("index projection %+v: got %d logs, want %d, error %v", check.attribute, len(got), check.count, err)
		}
	}
}

// D1 typed equality, long strings by digest verified on the row, and D3 tie
// order: equal effective times emit event IDs descending (index order; v1
// emitted them ascending).
func testRelLogsTyped(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	l1 := strings.Repeat("p", otelsql.MaxIndexedStringBytes) + "-one"
	l2 := strings.Repeat("p", otelsql.MaxIndexedStringBytes) + "-two"
	kvs := func(long string, ratio float64, ok bool) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{exKV("prompt", exStr(long)), exKV("ratio", exDouble(ratio)), exKV("ok", exBool(ok)), exKV("n", exInt(7)), exKV("raw", exBytes([]byte{1, 2}))}
	}
	stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{
		{EventID: "a", Timestamp: 100, Attributes: kvs(l1, 0.5, true)},
		{EventID: "b", Timestamp: 200, Attributes: kvs(l1, 0.25, false)},
		{EventID: "c", Timestamp: 300, Attributes: kvs(l2, 0.5, true)},
		{EventID: "t1", Timestamp: 600, Attributes: []*commonpb.KeyValue{exKV("tie", exStr("x"))}},
		{EventID: "t3", Timestamp: 600, Attributes: []*commonpb.KeyValue{exKV("tie", exStr("x"))}},
		{EventID: "t2", Timestamp: 600, Attributes: []*commonpb.KeyValue{exKV("tie", exStr("x"))}},
	})
	// D1: the bytes attribute is reported, not indexed.
	if err != nil || stats.UnindexedAttributes != 3 {
		t.Fatalf("export: %+v %v", stats, err)
	}
	// A row whose long string's digest names l1 but which holds l2: the
	// digest entry is a candidate, the row decides.
	p := lrPrepared(t, s, LogRecord{EventID: "d", Timestamp: 400, Attributes: kvs(l1, 0.5, true)})
	p.row.LONG_STR_ATTRS.Values[0].STR_VALUE = proto.String(l2)
	if _, err := s.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		return store.SaveRecord(p.row)
	}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		a    Attribute
		want []string
	}{
		"long":             {Attribute{LogScopeRecord, "prompt", "string", l1}, []string{"b", "a"}},
		"other long":       {Attribute{LogScopeRecord, "prompt", "string", l2}, []string{"c"}},
		"double":           {Attribute{LogScopeRecord, "ratio", "double", "0.5"}, []string{"d", "c", "a"}},
		"bool":             {Attribute{LogScopeRecord, "ok", "bool", "false"}, []string{"b"}},
		"int":              {Attribute{LogScopeRecord, "n", "int", "7"}, []string{"d", "c", "b", "a"}},
		"bool as string":   {Attribute{LogScopeRecord, "ok", "string", "true"}, []string{}},
		"double as string": {Attribute{LogScopeRecord, "ratio", "string", "0.5"}, []string{}},
		"int as double":    {Attribute{LogScopeRecord, "n", "double", "7"}, []string{}},
		"tie order":        {Attribute{LogScopeRecord, "tie", "string", "x"}, []string{"t3", "t2", "t1"}},
	} {
		got, err := s.QueryLogsByAttribute(ctx, c.a, 0)
		if err != nil || !reflect.DeepEqual(lgIDs(got), c.want) {
			t.Fatalf("%s: %v %v, want %v", name, lgIDs(got), err, c.want)
		}
	}
	if got, err := s.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "tie", "string", "x"}, 2); err != nil || !reflect.DeepEqual(lgIDs(got), []string{"t3", "t2"}) {
		t.Fatalf("tie limit: %v %v", lgIDs(got), err)
	}
	// A long event name is kept in the envelope and counted, not indexed.
	name := strings.Repeat("e", otelsql.MaxIndexedStringBytes+1)
	stats, err = s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "named", Timestamp: 1, TraceID: qaID(9, 16), EventName: name}})
	if err != nil || stats.UnindexedAttributes != 1 {
		t.Fatalf("long event name: %+v %v", stats, err)
	}
	if got, err := s.QueryLogsByTrace(ctx, qaID(9, 16), ""); err != nil || len(got) != 1 || got[0].EventName != name {
		t.Fatalf("long event name round trip: %v %v", lgIDs(got), err)
	}
	_, err = s.QueryLogsByAttribute(ctx, Attribute{LogScopeIntrinsic, "event_name", "string", name}, 0)
	requireTyped[*ConfigError](t, "unindexed event name", err)
}

// testRelLogExistential: log clauses of QueryTraceExistential beyond
// testRelTraceExistential's: a long-string clause verified on the rows, the
// window on effective_ns, and uncorrelated logs witnessing no trace.
func testRelLogExistential(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	r := h.open(t, ctx, nil)
	s := qrLogStore(t, r)
	long := strings.Repeat("q", otelsql.MaxIndexedStringBytes+5)
	T1, T2 := qaID(1, 16), qaID(2, 16)
	S := qaID(5, 8)
	if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{
		{EventID: "l1", Timestamp: 100, TraceID: T1, SpanID: S, Attributes: []*commonpb.KeyValue{exKV("prompt", exStr(long))}},
		{EventID: "l2", Timestamp: 300, TraceID: T1, Attributes: []*commonpb.KeyValue{exKV("prompt", exStr(long))}},
		{EventID: "l3", Timestamp: 200, TraceID: T2, Attributes: []*commonpb.KeyValue{exKV("prompt", exStr(long))}},
		{EventID: "l4", Timestamp: 900, Attributes: []*commonpb.KeyValue{exKV("prompt", exStr(long))}},
		{EventID: "l5", Timestamp: 250, TraceID: T2, Attributes: []*commonpb.KeyValue{exKV("prompt", exStr(long+"x"))}},
	}); err != nil {
		t.Fatal(err)
	}
	a := Attribute{LogScopeRecord, "prompt", "string", long}
	got, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{a}})
	if err != nil {
		t.Fatal(err)
	}
	want := []TraceExistentialHit{
		{T1, 300, [][]SpanMatch{}, [][]LogMatch{{{"l2", T1, "", 300}, {"l1", T1, S, 100}}}, nil},
		{T2, 200, [][]SpanMatch{}, [][]LogMatch{{{"l3", T2, "", 200}}}, nil},
	}
	if !reflect.DeepEqual(got.Hits, want) || got.Candidates == 0 {
		t.Fatalf("long-string log clause\n got %+v\nwant %+v", got, want)
	}
	got, err = r.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{a}, TimeWindow: &TimeRange{Start: 150, End: 300}})
	if err != nil || len(got.Hits) != 1 || got.Hits[0].Trace != T2 {
		t.Fatalf("windowed log clause: %+v %v", got.Hits, err)
	}
	got, err = r.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{a, {LogScopeRecord, "prompt", "string", long + "x"}}})
	if err != nil || len(got.Hits) != 1 || got.Hits[0].Trace != T2 || len(got.Hits[0].LogWitnesses) != 2 {
		t.Fatalf("two log clauses: %+v %v", got.Hits, err)
	}
	if got, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{{LogScopeIntrinsic, "severity_text", "string", "x"}}}); err != nil || len(got.Hits) != 0 {
		t.Fatalf("an intrinsic without an index: %+v %v", got, err)
	}
	_, err = r.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{{LogScopeIntrinsic, "event_name", "string", strings.Repeat("e", otelsql.MaxIndexedStringBytes+1)}}})
	requireTyped[*ConfigError](t, "unindexed event name clause", err)
	small := h.reader(t, ctx, r.cfg.Run, r.cfg.Tenant, func(c *Config) { c.MaxCandidates = 2 })
	_, err = small.QueryTraceExistential(ctx, TraceExistentialQuery{LogClauses: []Attribute{a}})
	requireOtelBudget(t, err)
}

// The write cost model's key arithmetic is the store's: for each fixture the
// LOGS index entries it adds are exactly the keys indexKeys visits, the
// deflated row bound is the stored row (within the prefix varint), and the
// admitted costs cover the transaction's approximate size, new and replaced.
func testRelLogIndexKeyModel(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	nul := strings.Repeat("\x00", 40)
	fixtures := []LogRecord{
		{
			EventID: "typed\x00id", Timestamp: 1 << 40, TraceID: qaID(1, 16), SpanID: qaID(2, 8), EventName: "ev" + nul, Body: exStr("body"),
			ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc"))}, ScopeAttributes: []*commonpb.KeyValue{exKV("lib", exInt(-300))},
			Attributes: []*commonpb.KeyValue{exKV("k"+nul, exStr(nul)), exKV("long", exStr(strings.Repeat("l", 3000))), exKV("d", exDouble(math.Copysign(0, -1))), exKV("b", exBool(false)), exKV("x", exBytes([]byte{0})), exKV("arr", exArray(exInt(1)))},
		},
		{EventID: "bare", ObservedTimestamp: 5},
		{EventID: "split", Timestamp: 9, TraceID: qaID(3, 16), Body: exBytes(lgRandom(t, 250_000)), Attributes: []*commonpb.KeyValue{exKV("n", exInt(1<<62))}},
	}
	for i := range 300 {
		fixtures[0].Attributes = append(fixtures[0].Attributes, exKV(fmt.Sprintf("i%03d", i), exInt(int64(i*i))))
	}
	keyBytes := func() (int64, int64) {
		var keys, n int64
		for _, name := range relLogIndexes {
			idx := s.store.ts.MetaData.GetIndex(name)
			sz, err := lrSize(ctx, s.store.db, s.store.ts.Subspace.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey()))
			if err != nil {
				t.Fatal(err)
			}
			keys, n = keys+sz.Keys, n+sz.KeyBytes
		}
		return keys, n
	}
	for _, l := range fixtures {
		p := lrPrepared(t, s, l)
		var wantKeys, wantBytes int64
		if _, err := s.indexKeys(p.row, p.pk, func(n int) { wantKeys, wantBytes = wantKeys+1, wantBytes+int64(n) }); err != nil {
			t.Fatal(err)
		}
		keys0, bytes0 := keyBytes()
		if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{l}); err != nil {
			t.Fatal(err)
		}
		keys1, bytes1 := keyBytes()
		if keys1-keys0 != wantKeys || bytes1-bytes0 != wantBytes {
			t.Fatalf("%q: index entries %d of %d bytes, the model %d of %d", l.EventID, keys1-keys0, bytes1-bytes0, wantKeys, wantBytes)
		}
		rec := lrRow(t, ctx, s, l.EventID)
		exact, err := s.rowBytes(p.row, true)
		if err != nil {
			t.Fatal(err)
		}
		if bound, _ := s.rowBytes(p.row, false); exact < rec.ValueSize || exact > rec.ValueSize+1 || bound < rec.ValueSize {
			t.Fatalf("%q: stored row %d bytes, deflated model %d, bound %d", l.EventID, rec.ValueSize, exact, bound)
		}
		prior, err := s.priorCost(rec.Record.(*otelv1pb.LOGS), p.pk, rec.KeyCount)
		if err != nil {
			t.Fatal(err)
		}
		// The save of a new row, and its replacement, within their admitted costs.
		for _, replace := range []bool{false, true} {
			row, admitted := p.row, p.writeCost
			if replace {
				admitted += prior
			} else {
				row = proto.Clone(p.row).(*otelv1pb.LOGS)
				row.EVENT_ID = proto.String(l.EventID + "-fresh")
				fresh := relPreparedLog{pk: s.primaryKey(row.GetEVENT_ID()), row: row}
				if err := s.admitCost(&fresh); err != nil {
					t.Fatal(err)
				}
				admitted = fresh.writeCost
			}
			var size int64
			errAbort := errors.New("abort")
			_, err := s.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := s.store.ts.Builder(rc).Open()
				if err != nil {
					return nil, err
				}
				if _, err := store.SaveRecordBatch([]proto.Message{row}); err != nil {
					return nil, err
				}
				if size, err = rc.Transaction().GetApproximateSize().Get(); err != nil {
					return nil, err
				}
				return nil, errAbort
			})
			if !errors.Is(err, errAbort) {
				t.Fatal(err)
			}
			if size == 0 || size > int64(admitted) {
				t.Fatalf("%q (replace %v): transaction of %d bytes, admitted at %d", l.EventID, replace, size, admitted)
			}
		}
	}
}

// testRelLogBoundaryAdmission: OTEL_V1 keys hold strings up to 1,024 bytes
// (2,048 escaped, far below FDB's key limit), longer ones by digest, and
// project values up to 64,000 bytes. So every size admits the whole call,
// and the boundaries are where a value stops being queryable.
func testRelLogBoundaryAdmission(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	for _, scope := range []string{"resource", "scope", LogScopeRecord, LogScopeIntrinsic} {
		sizes := []int{otelsql.MaxIndexedStringBytes, otelsql.MaxIndexedStringBytes + 1, otelsql.MaxAttributeValueBytes, otelsql.MaxAttributeValueBytes + 1}
		indexed := otelsql.MaxAttributeValueBytes
		key := "expanded"
		if scope == LogScopeIntrinsic {
			sizes, indexed, key = sizes[:2], otelsql.MaxIndexedStringBytes, "event_name"
		}
		for _, size := range sizes {
			s := h.openLogs(t, ctx, func(c *ExporterConfig) { c.BatchSize, c.Workers = 1, 1 })
			value := strings.Repeat("\x00", size)
			bad := LogRecord{EventID: "boundary", Timestamp: 7, TraceID: ftsTrace(2), SpanID: ftsSpan(2)}
			attrs := []*commonpb.KeyValue{exKV(key, exStr(value))}
			switch scope {
			case "resource":
				bad.ResourceAttributes = attrs
			case "scope":
				bad.ScopeAttributes = attrs
			case LogScopeRecord:
				bad.Attributes = attrs
			case LogScopeIntrinsic:
				bad.EventName = value
			}
			good := LogRecord{EventID: "good", Timestamp: 1, TraceID: ftsTrace(1)}
			stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{good, bad})
			unindexed := int64(0)
			if size > indexed {
				unindexed = 1
			}
			if err != nil || stats.Logs != 2 || stats.Batches != 2 || stats.UnindexedAttributes != unindexed {
				t.Fatalf("%s/%d: %+v / %v", scope, size, stats, err)
			}
			got, err := s.QueryLogsByTrace(ctx, bad.TraceID, bad.SpanID)
			if err != nil || len(got) != 1 {
				t.Fatalf("%s/%d boundary read: %+v / %v", scope, size, got, err)
			}
			bad.EffectiveTime, bad.TimeSource = 7, LogTimeEvent
			lgRequireLog(t, "complete boundary log", got[0], bad)
			byValue, err := s.QueryLogsByAttribute(ctx, Attribute{scope, key, "string", value}, 0)
			if size > indexed {
				requireTyped[*ConfigError](t, fmt.Sprintf("%s/%d unindexed", scope, size), err)
			} else if err != nil || !reflect.DeepEqual(lgIDs(byValue), []string{"boundary"}) {
				t.Fatalf("%s/%d by value: %v / %v", scope, size, lgIDs(byValue), err)
			}
		}
	}
}

// lrTamper replaces the stored row of row's event ID with row's bytes and no
// index maintenance: row is saved into a scratch store of the same metadata
// and serializer, and its record keys are copied over the original's, so
// every index entry of the original stays in place.
func lrTamper(t *testing.T, ctx context.Context, s *relLogStore, row *otelv1pb.LOGS) {
	t.Helper()
	scratch := subspace.Sub("lr_scratch", freshRun(t))
	pk := s.primaryKey(row.GetEVENT_ID())
	if _, err := s.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		builder := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(s.store.ts.MetaData).SetSubspace(scratch)
		if s.store.ts.Serializer != nil {
			builder = builder.SetSerializer(s.store.ts.Serializer)
		}
		store, err := builder.CreateOrOpen()
		if err != nil {
			return nil, err
		}
		if _, err := store.SaveRecord(row); err != nil {
			return nil, err
		}
		from, to := scratch.Sub(recordlayer.RecordKey), s.store.ts.Subspace.Sub(recordlayer.RecordKey)
		fr, err := fdb.PrefixRange(from.Pack(pk))
		if err != nil {
			return nil, err
		}
		rows, err := rc.Transaction().GetRange(fr, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil || len(rows) == 0 {
			return nil, fmt.Errorf("scratch row: %d keys, %v", len(rows), err)
		}
		tr, err := fdb.PrefixRange(to.Pack(pk))
		if err != nil {
			return nil, err
		}
		rc.Transaction().ClearRange(tr)
		for _, kv := range rows {
			rc.Transaction().Set(fdb.Key(append(append([]byte{}, to.Bytes()...), kv.Key[len(from.Bytes()):]...)), kv.Value)
		}
		rc.Transaction().ClearRange(scratch)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// testRelLogPersistedDisagreement: a valid, decodable LOGS row that its
// intact index entries no longer describe is corruption.
func testRelLogPersistedDisagreement(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for _, damage := range []string{"trace", "span", "time", "attribute", "otlp"} {
		s := h.openLogs(t, ctx, nil)
		original := LogRecord{EventID: "stale", Timestamp: 7, TraceID: ftsTrace(1), SpanID: ftsSpan(1), Attributes: []*commonpb.KeyValue{exKV("kind", exStr("old"))}}
		if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{original}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.QueryLogsByTrace(ctx, original.TraceID, original.SpanID); err != nil || len(got) != 1 {
			t.Fatalf("live trace positive control: %v / %v", got, err)
		}
		a := Attribute{LogScopeRecord, "kind", "string", "old"}
		if got, err := s.QueryLogsByAttribute(ctx, a, 0); err != nil || len(got) != 1 {
			t.Fatalf("live equality positive control: %v / %v", got, err)
		}
		changed := original
		switch damage {
		case "trace":
			changed.TraceID = ftsTrace(2)
		case "span":
			changed.SpanID = ftsSpan(2)
		case "time":
			changed.Timestamp++
		case "attribute", "otlp":
			changed.Attributes = []*commonpb.KeyValue{exKV("kind", exStr("new"))}
		}
		p := lrPrepared(t, s, changed)
		if damage == "otlp" {
			// The columns are the original's; only the envelope disagrees.
			p = lrPrepared(t, s, original)
			p.row.OTLP = lrPrepared(t, s, LogRecord{EventID: "stale", Timestamp: 7, TraceID: ftsTrace(2), SpanID: ftsSpan(1), Attributes: original.Attributes}).row.OTLP
		}
		lrTamper(t, ctx, s, p.row)
		got, err := s.QueryLogsByAttribute(ctx, a, 0)
		requireTyped[*CorruptionError](t, damage+": stale equality entry", err)
		if len(got) != 0 {
			t.Fatalf("corruption returned logs: %v", got)
		}
		got, err = s.QueryLogsByTrace(ctx, original.TraceID, original.SpanID)
		if damage == "attribute" {
			if err != nil || len(got) != 1 {
				t.Fatalf("trace-only witness remains valid: %v / %v", got, err)
			}
		} else {
			requireTyped[*CorruptionError](t, damage+": stale trace entry", err)
			if len(got) != 0 {
				t.Fatalf("corruption returned logs: %v", got)
			}
		}
	}
}

// lrManyInts is a log of n small integer attributes.
func lrManyInts(l LogRecord, n int) LogRecord {
	for i := range n {
		l.Attributes = append(l.Attributes, exKV(fmt.Sprintf("k%05d", i), exInt(int64(i))))
	}
	return l
}

// testRelLogNativeCostWholeCall: a log far under the raw cap whose index
// entries alone exceed one transaction refuses the whole call. The positive
// control is the store's own approximate size for saving it.
func testRelLogNativeCostWholeCall(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	large := lrManyInts(LogRecord{EventID: "0123456789abcdef0123456789abcdef", Timestamp: 7}, 40000)
	rl, err := large.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(rl)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= maxLogRawBytes/8 {
		t.Fatalf("not a key-cost fixture: raw=%d", len(raw))
	}
	_, err = s.prepare(large, 1)
	requireTyped[*AdmissionError](t, "small integer native costs", err)
	if !strings.Contains(err.Error(), "expanded transaction writes") {
		t.Fatalf("wrong admission boundary: %v", err)
	}
	// Admission is needed: saving the row alone exceeds FDB's 10 MB.
	sl := rl.ScopeLogs[0]
	unchecked, err := otelsql.BuildLogCompiled(otelsql.Log{Resource: rl.Resource, Scope: sl.Scope, Log: sl.LogRecords[0], EventID: large.EventID, EffectiveNs: 7, TimeSource: LogTimeEvent, OTLP: raw})
	if err != nil {
		t.Fatal(err)
	}
	errAbort := errors.New("abort")
	var size int64
	_, err = s.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := s.store.ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		if _, err := store.SaveRecordBatch([]proto.Message{unchecked}); err != nil {
			return nil, err
		}
		if size, err = rc.Transaction().GetApproximateSize().Get(); err != nil {
			return nil, err
		}
		return nil, errAbort
	})
	if !errors.Is(err, errAbort) || size <= 10_000_000 {
		t.Fatalf("positive control: transaction of %d bytes, %v", size, err)
	}
	stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, large})
	requireTyped[*AdmissionError](t, "whole call", err)
	if stats.Logs != 0 || stats.Batches != 0 {
		t.Fatalf("partially admitted request: %+v", stats)
	}
	for name, n := range lrCensusAll(t, ctx, s) {
		if n != 0 {
			t.Fatalf("rejected request wrote %d %s keys", n, name)
		}
	}
	if stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}}); err != nil || stats.Logs != 1 {
		t.Fatalf("neighbor positive control: %+v / %v", stats, err)
	}
}

// lrReplacementInts is the attribute count whose log fits one transaction
// alone but not with the clears of its own prior image.
const lrReplacementInts = 16000

// testRelLogReplacementPriorCost: the preflight adds the stored image's
// clears, read from the store, to the new image's cost.
func testRelLogReplacementPriorCost(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, nil)
	large := lrManyInts(LogRecord{EventID: "replacement", Timestamp: 7, TraceID: ftsTrace(1)}, lrReplacementInts)
	p, err := s.prepare(large, 1)
	if err != nil {
		t.Fatalf("initial image must fit: %v", err)
	}
	if stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{large}); err != nil || stats.Logs != 1 {
		t.Fatalf("initial image: %+v / %v", stats, err)
	}
	stored := lrRow(t, ctx, s, large.EventID)
	prior, err := s.priorCost(stored.Record.(*otelv1pb.LOGS), p.pk, stored.KeyCount)
	if err != nil {
		t.Fatal(err)
	}
	if p.writeCost+prior <= maxLogTxnWriteBytes {
		t.Fatalf("fixture does not exercise prior clears: new=%d prior=%d", p.writeCost, prior)
	}
	before := lrCensusAll(t, ctx, s)
	stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, large})
	requireTyped[*AdmissionError](t, "prior image clears", err)
	if stats.Logs != 0 || stats.Batches != 0 {
		t.Fatalf("replacement admission partially wrote: %+v", stats)
	}
	if after := lrCensusAll(t, ctx, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected replacement changed the store: %v, was %v", after, before)
	}
	// A small replacement still counts every old entry's clear, not clears
	// estimated from its own new attributes.
	small := lrPrepared(t, s, LogRecord{EventID: large.EventID, Timestamp: 8})
	items := []relPreparedLog{small}
	if err := s.admitPriors(lrCtx(ctx), items); err != nil {
		t.Fatal(err)
	}
	if items[0].cost != small.writeCost+prior {
		t.Fatalf("small replacement cost %d, want %d + %d", items[0].cost, small.writeCost, prior)
	}
	if stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: large.EventID, Timestamp: 8}}); err != nil || stats.Replaced != 1 {
		t.Fatalf("small replacement: %+v / %v", stats, err)
	}
	if n := lrCensus(t, ctx, s, logsIntAttrIndex); n != 0 {
		t.Fatalf("small replacement left %d old entries", n)
	}
}

// lrNeighborBarrier is logNeighborBarrier: it observes the neighbor's real
// commit, then grows the prior image before the next transaction.
type lrNeighborBarrier = logNeighborBarrier

// testRelLogPriorChangeAfterAdmission: a prior image that grows after
// admission, between two batches of one export, fails that batch as an
// operation error, not an admission error; the committed neighbor batch
// stays, the stats report it exactly, and the concurrent prior is untouched.
func testRelLogPriorChangeAfterAdmission(t *testing.T, h *qrHarness, raw fdb.Database) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s := h.openLogs(t, ctx, func(c *ExporterConfig) { c.BatchSize, c.Workers = 1, 1 })
	initial := LogRecord{EventID: "changing-prior", Timestamp: 1, TraceID: ftsTrace(1), Body: exStr("initial")}
	if _, err := s.ExportLogs(lrCtx(ctx), []LogRecord{initial}); err != nil {
		t.Fatal(err)
	}
	incoming := lrManyInts(LogRecord{EventID: initial.EventID, Timestamp: 8, TraceID: initial.TraceID, Body: exStr("must not persist")}, lrReplacementInts)
	neighbor := lrPrepared(t, s, LogRecord{EventID: "committed-neighbor", Timestamp: 2, TraceID: ftsTrace(2)})
	next := lrPrepared(t, s, incoming)
	if next.writeCost >= maxLogTxnWriteBytes {
		t.Fatalf("new image must fit without prior growth: %d", next.writeCost)
	}
	// A second handle performs the independent write at the observed commit
	// boundary inside one public export.
	other := h.logStore(t, ctx, s.cfg.Run, s.cfg.Tenant, nil)
	grown := incoming
	grown.Timestamp, grown.Body = 7, exStr("concurrent prior")
	neighborKey := s.store.ts.Subspace.Sub(recordlayer.RecordKey).Pack(append(s.primaryKey(neighbor.eventID), int64(0)))
	barrier := &lrNeighborBarrier{Database: raw, neighbor: neighborKey}
	barrier.grow = func() error {
		got, err := other.QueryLogsByTrace(ctx, ftsTrace(2), "")
		if err != nil {
			return err
		}
		if len(got) != 1 || got[0].EventID != neighbor.eventID {
			return fmt.Errorf("barrier lacks independently committed neighbor: %+v", got)
		}
		_, err = other.ExportLogs(lrCtx(ctx), []LogRecord{grown})
		return err
	}
	ts := *s.store
	ts.db = recordlayer.NewFDBDatabaseWithTransactor(barrier, raw)
	barred := *s
	barred.store = &ts
	stats, err := barred.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: neighbor.eventID, Timestamp: 2, TraceID: ftsTrace(2)}, incoming})
	if !barrier.committed || !barrier.crossed {
		t.Fatalf("public export missed neighbor/growth barrier: %+v / %v", barrier, err)
	}
	stored := lrRow(t, ctx, s, neighbor.eventID)
	if stored == nil || stats.ReceivedLogs != 2 || stats.Logs != 1 || stats.Batches != 1 || stats.Replaced != 0 || stats.Correlated != 1 || stats.LogicalBytes != neighbor.logical || stats.StoredBytes != int64(stored.ValueSize) || stats.DuplicateLogs != 0 || stats.GeneratedIDs != 0 || stats.UnindexedAttributes != 0 || len(stats.EventIDs) != 2 || stats.EventIDs[0] != neighbor.eventID || stats.EventIDs[1] != incoming.EventID {
		t.Fatalf("public late failure lost exact partial outcome: %+v", stats)
	}
	requireTyped[*OperationError](t, "live prior changed after admission", err)
	var admission *AdmissionError
	if errors.As(err, &admission) {
		t.Fatalf("runtime prior change mislabeled whole-call admission: %v", err)
	}
	got, err := s.QueryLogsByTrace(ctx, ftsTrace(2), "")
	if err != nil || len(got) != 1 || got[0].EventID != neighbor.eventID {
		t.Fatalf("committed neighbor rolled back: %+v / %v", got, err)
	}
	got, err = s.QueryLogsByTrace(ctx, initial.TraceID, "")
	if err != nil || len(got) != 1 || got[0].Timestamp != 7 || got[0].Body.GetStringValue() != "concurrent prior" || len(got[0].Attributes) != lrReplacementInts {
		t.Fatalf("failed live batch mutated concurrent prior: logs=%d err=%v", len(got), err)
	}
}

// testRelLogExpandedWrite: under the raw cap, a row whose projected copies of
// the attributes push the serialized record over the transaction budget
// refuses the whole call. Incompressible bytes stay over it deflated; the
// same shape in octal text deflates under it and is admitted.
func testRelLogExpandedWrite(t *testing.T, h *qrHarness) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	// The admitted octal row is read back whole, past the default byte budget.
	s := h.openLogs(t, ctx, func(c *ExporterConfig) { c.MaxBytes = 16 << 20 })
	large := LogRecord{EventID: "expanded", Timestamp: 7, TraceID: ftsTrace(1)}
	octal := LogRecord{EventID: "octal", Timestamp: 8, TraceID: ftsTrace(1)}
	state := uint64(0x91ad4708b3)
	for i := range 120 {
		large.Attributes = append(large.Attributes, exKV(fmt.Sprintf("k%03d", i), exBytes(lgRandom(t, otelsql.MaxAttributeValueBytes))))
		value := make([]byte, otelsql.MaxAttributeValueBytes)
		for j := range value {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			value[j] = '0' + byte(state&7)
		}
		octal.Attributes = append(octal.Attributes, exKV(fmt.Sprintf("k%03d", i), exStr(string(value))))
	}
	for _, l := range []LogRecord{large, octal} {
		rl, err := l.OTLP()
		if err != nil {
			t.Fatal(err)
		}
		if n := proto.Size(rl); n > maxLogRawBytes {
			t.Fatalf("raw fixture exceeds the raw cap: %d", n)
		}
	}
	_, err := s.prepare(large, 1)
	requireTyped[*AdmissionError](t, "expanded singleton", err)
	if err == nil || !strings.Contains(err.Error(), "expanded transaction writes") {
		t.Fatalf("wrong admission boundary: %v", err)
	}
	stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, large})
	requireTyped[*AdmissionError](t, "whole-call admission", err)
	if stats.Logs != 0 || stats.Batches != 0 {
		t.Fatalf("rejected call committed: %+v", stats)
	}
	for name, n := range lrCensusAll(t, ctx, s) {
		if n != 0 {
			t.Fatalf("rejected call wrote %d %s keys", n, name)
		}
	}
	p := lrPrepared(t, s, octal)
	if bound, _ := s.rowBytes(p.row, false); bound <= maxLogTxnWriteBytes {
		t.Fatalf("the octal fixture fits uncompressed: %d", bound)
	}
	if stats, err := s.ExportLogs(lrCtx(ctx), []LogRecord{{EventID: "valid-neighbor", Timestamp: 1}, octal}); err != nil || stats.Logs != 2 {
		t.Fatalf("deflated admission: %+v / %v", stats, err)
	}
	// Under the exporter's 10s transaction timeout, as the exporter reads: the
	// 1.5s default is short for decoding a 6 MB row under the race detector.
	if got, err := s.QueryLogsByTrace(lrCtx(ctx), ftsTrace(1), ""); err != nil || len(got) != 1 || !proto.Equal(got[0].Attributes[119], octal.Attributes[119]) {
		t.Fatalf("deflated log round trip: %d %v", len(got), err)
	}
}
