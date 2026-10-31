package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func lgBackend() *FDBBackend {
	return &FDBBackend{cfg: Config{Run: "lg_run_0001", Tenant: 3, HashBytes: 8, K: 100}, root: subspace.Sub("lg_root")}
}

func lgRandom(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// lgRequireLog compares every stored field through the canonical OTLP form.
func lgRequireLog(t *testing.T, label string, got, want LogRecord) {
	t.Helper()
	g, err := toResourceLogs(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := toResourceLogs(want)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(g, w) || got.EventID != want.EventID || got.EffectiveTime != want.EffectiveTime || got.TimeSource != want.TimeSource || got.TraceID != want.TraceID || got.SpanID != want.SpanID {
		t.Fatalf("%s\n got %+v\nwant %+v", label, got, want)
	}
}

func lgIDs(logs []LogRecord) []string {
	out := []string{}
	for _, l := range logs {
		out = append(out, l.EventID)
	}
	return out
}

// lgRows is the KV layout saveLogBatch writes for one prepared log.
func lgRows(b *FDBBackend, p preparedLog) (subspace.Subspace, []fdb.KeyValue) {
	space := logRecordSpace(b.root, b.cfg.Tenant, p.eventID)
	rows := []fdb.KeyValue{{Key: space.Pack(tuple.Tuple{int64(0)}), Value: p.header}}
	for i, c := range p.chunks {
		rows = append(rows, fdb.KeyValue{Key: space.Pack(tuple.Tuple{int64(i + 1)}), Value: c})
	}
	return space, rows
}

func lgPrepared(t *testing.T, b *FDBBackend, l LogRecord) (preparedLog, LogRecord) {
	t.Helper()
	p, err := b.prepareLog(l, 777)
	if err != nil {
		t.Fatal(err)
	}
	return p, l
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

func TestLogValueEncodingRoundTrip(t *testing.T) {
	t.Parallel()
	b := lgBackend()
	attrs := []*commonpb.KeyValue{exKV("gen_ai.evaluation.name", exStr("relevance")), exKV("score", exDouble(0.75))}
	for _, c := range []struct {
		name   string
		body   *commonpb.AnyValue
		codec  int64
		chunks int
	}{
		// codec -1: Brotli is used only when it shrinks the payload, which is input-dependent.
		{"small", exStr("ok"), -1, 0},
		{"compressible_inline", exStr(strings.Repeat("prompt text ", 20000)), logCodecBrotli, 0},
		{"incompressible_chunked", exBytes(lgRandom(t, 300000)), -1, 4},
		{"no_body", nil, -1, 0},
	} {
		in := LogRecord{
			EventID: "ev-" + c.name, Timestamp: 123, ObservedTimestamp: 456, TraceID: qaID(1, 16), SpanID: qaID(2, 8), Flags: 1, SeverityText: "INFO", SeverityNumber: 9, EventName: "gen_ai.evaluation.result", Body: c.body, DroppedAttributesCount: 2,
			ResourceSchemaURL: "https://r", ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc"))}, ScopeName: "scope", ScopeVersion: "1.0", ScopeSchemaURL: "https://s", ScopeAttributes: []*commonpb.KeyValue{exKV("lib", exInt(3))}, Attributes: attrs,
		}
		p, _ := lgPrepared(t, b, in)
		h, err := tuple.Unpack(p.header)
		if err != nil {
			t.Fatal(err)
		}
		if (c.codec >= 0 && h[1].(int64) != c.codec) || len(p.chunks) != c.chunks || h[4].(int64) != int64(c.chunks) {
			t.Fatalf("%s: codec=%v chunks=%d header=%v", c.name, h[1], len(p.chunks), h[:7])
		}
		for _, ch := range p.chunks {
			if len(ch) > logChunkBytes {
				t.Fatalf("%s: chunk of %d bytes", c.name, len(ch))
			}
		}
		if len(p.header) > 100000 {
			t.Fatalf("%s: header row of %d bytes exceeds FDB value cap", c.name, len(p.header))
		}
		space, rows := lgRows(b, p)
		got, err := decodeLogRows(space, p.eventID, rows)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := in
		want.EffectiveTime, want.TimeSource = 123, LogTimeEvent
		lgRequireLog(t, c.name, got, want)
	}
}

func TestLogValueDecodingRejectsCorruption(t *testing.T) {
	t.Parallel()
	b := lgBackend()
	p, _ := lgPrepared(t, b, LogRecord{EventID: "big", Timestamp: 5, Body: exBytes(lgRandom(t, 200000))})
	space, rows := lgRows(b, p)
	if len(rows) != 4 {
		t.Fatalf("fixture must be chunked, got %d rows", len(rows))
	}
	small, _ := lgPrepared(t, b, LogRecord{EventID: "small", Timestamp: 5, Body: exStr(strings.Repeat("abc", 1000))})
	smallSpace, smallRows := lgRows(b, small)
	if h, err := tuple.Unpack(smallRows[0].Value); err != nil || h[1] != logCodecBrotli || len(small.chunks) != 0 {
		t.Fatalf("fixture must be inline Brotli: %v", err)
	}
	header := func(mut func(tuple.Tuple)) []fdb.KeyValue {
		h, err := tuple.Unpack(smallRows[0].Value)
		if err != nil {
			t.Fatal(err)
		}
		mut(h)
		return []fdb.KeyValue{{Key: smallRows[0].Key, Value: h.Pack()}}
	}
	clone := func(rs []fdb.KeyValue) []fdb.KeyValue { return append([]fdb.KeyValue(nil), rs...) }
	for name, c := range map[string]struct {
		space subspace.Subspace
		rows  []fdb.KeyValue
	}{
		"missing":          {space, nil},
		"dropped chunk":    {space, append(clone(rows[:2]), rows[3])},
		"truncated chunks": {space, rows[:3]},
		"extra row":        {space, append(clone(rows), fdb.KeyValue{Key: space.Pack(tuple.Tuple{int64(4)}), Value: []byte{1}})},
		"reordered":        {space, []fdb.KeyValue{rows[0], rows[2], rows[1], rows[3]}},
		"short chunk":      {space, append(clone(rows[:3]), fdb.KeyValue{Key: rows[3].Key, Value: rows[3].Value[1:]})},
		"foreign key":      {space, append(clone(rows[:3]), fdb.KeyValue{Key: space.Pack(tuple.Tuple{"x"}), Value: rows[3].Value})},
		"header garbage":   {smallSpace, []fdb.KeyValue{{Key: smallRows[0].Key, Value: []byte{0xff}}}},
		"bad version":      {smallSpace, header(func(h tuple.Tuple) { h[0] = int64(9) })},
		"bad codec":        {smallSpace, header(func(h tuple.Tuple) { h[1] = int64(7) })},
		"raw length":       {smallSpace, header(func(h tuple.Tuple) { h[2] = h[2].(int64) + 1 })},
		"stored length":    {smallSpace, header(func(h tuple.Tuple) { h[3] = h[3].(int64) - 1 })},
		"chunk count":      {smallSpace, header(func(h tuple.Tuple) { h[4] = int64(1) })},
		"negative time":    {smallSpace, header(func(h tuple.Tuple) { h[5] = int64(-1) })},
		"time source":      {smallSpace, header(func(h tuple.Tuple) { h[6] = "guess" })},
		"field type":       {smallSpace, header(func(h tuple.Tuple) { h[6] = int64(1) })},
		"truncated brotli": {smallSpace, header(func(h tuple.Tuple) {
			payload := h[7].([]byte)
			h[3], h[7] = int64(len(payload)-4), payload[:len(payload)-4]
		})},
		"not a log": {smallSpace, header(func(h tuple.Tuple) {
			wire, _ := proto.Marshal(&logspb.ResourceLogs{SchemaUrl: "x"})
			h[1], h[2], h[3], h[7] = logCodecRaw, int64(len(wire)), int64(len(wire)), wire
		})},
	} {
		if _, err := decodeLogRows(c.space, "big", c.rows); err == nil {
			t.Fatalf("%s: accepted", name)
		} else {
			requireTyped[*CorruptionError](t, name, err)
		}
	}
}

func TestPrepareLogAdmission(t *testing.T) {
	t.Parallel()
	b := lgBackend()
	for name, l := range map[string]LogRecord{
		"negative time":     {EventID: "a", Timestamp: -1},
		"negative observed": {EventID: "a", ObservedTimestamp: -1},
		"short trace":       {EventID: "a", TraceID: qaID(1, 16)[1:]},
		"zero trace":        {EventID: "a", TraceID: strings.Repeat("0", 32)},
		"non-hex trace":     {EventID: "a", TraceID: strings.Repeat("g", 32)},
		"span w/o trace":    {EventID: "a", SpanID: qaID(1, 8)},
		"zero span":         {EventID: "a", TraceID: qaID(1, 16), SpanID: strings.Repeat("0", 16)},
		"long event ID":     {EventID: strings.Repeat("e", maxLogEventID+1)},
		"invalid UTF-8 ID":  {EventID: "\xff"},
		"oversized":         {EventID: "a", Body: exBytes(lgRandom(t, maxLogStoredBytes+1024))},
	} {
		_, err := b.prepareLog(l, 1)
		requireTyped[*AdmissionError](t, name, err)
	}
	upper := strings.ToUpper(qaID(0xabc, 16))
	p, err := b.prepareLog(LogRecord{EventID: "u", Timestamp: 9, TraceID: upper, SpanID: strings.ToUpper(qaID(0xdef, 8))}, 1)
	if err != nil || !p.correlated {
		t.Fatalf("uppercase identities: %+v %v", p, err)
	}
	space, rows := lgRows(b, p)
	got, err := decodeLogRows(space, "u", rows)
	if err != nil || got.TraceID != qaID(0xabc, 16) || got.SpanID != qaID(0xdef, 8) {
		t.Fatalf("normalized identities: %+v %v", got, err)
	}
	for _, c := range []struct {
		l      LogRecord
		effect int64
		source string
	}{
		{LogRecord{EventID: "t", Timestamp: 10, ObservedTimestamp: 20}, 10, LogTimeEvent},
		{LogRecord{EventID: "t", ObservedTimestamp: 20}, 20, LogTimeObserved},
		{LogRecord{EventID: "t"}, 777, LogTimeReceiver},
	} {
		p, _ := lgPrepared(t, b, c.l)
		space, rows := lgRows(b, p)
		got, err := decodeLogRows(space, "t", rows)
		if err != nil || got.EffectiveTime != c.effect || got.TimeSource != c.source || got.Timestamp != c.l.Timestamp || got.ObservedTimestamp != c.l.ObservedTimestamp {
			t.Fatalf("time fallback %+v: %+v %v", c.l, got, err)
		}
	}
}

func TestLogIndexKeys(t *testing.T) {
	t.Parallel()
	b := lgBackend()
	l := LogRecord{
		EventID: "ev", EffectiveTime: 50, TraceID: qaID(1, 16), SpanID: qaID(2, 8), EventName: "gen_ai.evaluation.result",
		ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc"))},
		ScopeAttributes:    []*commonpb.KeyValue{exKV("big", exStr(strings.Repeat("x", maxIndexedValueBytes+1)))},
		Attributes: []*commonpb.KeyValue{
			exKV("k", exStr("first")), nil, exKV("flag", exBool(true)), exKV("k", exStr("last")),
			exKV("score", exDouble(0.5)), exKV("n", exInt(-4)), exKV("list", exArray(exStr("a"), exInt(1))),
		},
	}
	attrs, unindexed := logIndexed(l)
	want := []Attribute{
		{"resource", "service.name", "string", "svc"},
		{LogScopeRecord, "k", "string", "last"},
		{LogScopeRecord, "flag", "string", "true"},
		{LogScopeRecord, "score", "string", "0.5"},
		{LogScopeRecord, "n", "int", "-4"},
		{LogScopeRecord, "list", "string", `["a",1]`},
		{LogScopeIntrinsic, "event_name", "string", "gen_ai.evaluation.result"},
	}
	if !reflect.DeepEqual(attrs, want) || unindexed != 1 {
		t.Fatalf("indexed attributes:\n got %+v (unindexed %d)\nwant %+v", attrs, unindexed, want)
	}
	traceKey, postings, _ := logIndexKeys(b.root, b.cfg.Tenant, b.cfg.HashBytes, l)
	if !bytes.Equal(traceKey, logTraceSpace(b.root, 3).Pack(tuple.Tuple{l.TraceID, l.SpanID, int64(50), "ev"})) {
		t.Fatal("LOG_TRACE key layout")
	}
	if len(postings) != len(want) {
		t.Fatalf("postings: %d", len(postings))
	}
	for ordinal, p := range postings {
		a := want[ordinal]
		if !bytes.Equal(p.Key, logTermPrefix(b.root, 3, a, 8).Pack(tuple.Tuple{int64(-50), "ev", int64(ordinal)})) {
			t.Fatalf("posting %d key layout", ordinal)
		}
		got, eff, tr, sp, err := decodeLogCovering(p.Value)
		if err != nil || got != a || eff != 50 || tr != l.TraceID || sp != l.SpanID {
			t.Fatalf("posting %d covering: %+v %d %s %s %v", ordinal, got, eff, tr, sp, err)
		}
		if !logHasAttribute(l, a) {
			t.Fatalf("record verification rejects its own attribute %+v", a)
		}
	}
	if logHasAttribute(l, Attribute{LogScopeRecord, "k", "string", "first"}) {
		t.Fatal("overwritten duplicate key value must not verify")
	}
	l.TraceID, l.SpanID = "", ""
	if traceKey, _, _ := logIndexKeys(b.root, 3, 8, l); traceKey != nil {
		t.Fatal("uncorrelated log must not get a LOG_TRACE entry")
	}
	for name, v := range map[string][]byte{
		"garbage":     {0xff},
		"few fields":  tuple.Tuple{"log", "k", "string", "v", int64(1), ""}.Pack(),
		"time type":   tuple.Tuple{"log", "k", "string", "v", "1", "", ""}.Pack(),
		"string type": tuple.Tuple{"log", int64(1), "string", "v", int64(1), "", ""}.Pack(),
		"bad UTF-8":   tuple.Tuple{"log", "\xff", "string", "v", int64(1), "", ""}.Pack(),
		"big key":     tuple.Tuple{"log", strings.Repeat("k", maxIndexedKeyBytes+1), "string", "v", int64(1), "", ""}.Pack(),
	} {
		_, _, _, _, err := decodeLogCovering(v)
		requireTyped[*CorruptionError](t, name, err)
	}
}

func TestNewEventID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 1000 {
		id, err := newEventID()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != id || raw[6]>>4 != 4 || raw[8]>>6 != 2 {
			t.Fatalf("not a lowercase hex v4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate event ID %s", id)
		}
		seen[id] = true
	}
}

func TestLogRecordsFromResourceLogs(t *testing.T) {
	t.Parallel()
	tr, sp := qaBytes(t, qaID(1, 16)), qaBytes(t, qaID(2, 8))
	in := []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc"))}},
		SchemaUrl: "https://r",
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:     &commonpb.InstrumentationScope{Name: "s", Version: "v", Attributes: []*commonpb.KeyValue{exKV("a", exInt(1))}},
			SchemaUrl: "https://s",
			LogRecords: []*logspb.LogRecord{
				{TimeUnixNano: 1, ObservedTimeUnixNano: 2, TraceId: tr, SpanId: sp, Flags: 1, SeverityText: "WARN", SeverityNumber: 13, EventName: "e", Body: exStr("b"), DroppedAttributesCount: 3, Attributes: []*commonpb.KeyValue{exKV("x", exStr("y"))}},
				nil,
				{TraceId: make([]byte, 16), SpanId: make([]byte, 8), TimeUnixNano: math.MaxUint64},
			},
		}},
	}}
	got := LogRecordsFromResourceLogs(in)
	if len(got) != 2 {
		t.Fatalf("records: %d", len(got))
	}
	want := LogRecord{Timestamp: 1, ObservedTimestamp: 2, TraceID: qaID(1, 16), SpanID: qaID(2, 8), Flags: 1, SeverityText: "WARN", SeverityNumber: 13, EventName: "e", Body: exStr("b"), DroppedAttributesCount: 3, ResourceSchemaURL: "https://r", ResourceAttributes: in[0].Resource.Attributes, ScopeName: "s", ScopeVersion: "v", ScopeSchemaURL: "https://s", ScopeAttributes: in[0].ScopeLogs[0].Scope.Attributes, Attributes: in[0].ScopeLogs[0].LogRecords[0].Attributes}
	lgRequireLog(t, "converted", got[0], want)
	if got[1].TraceID != "" || got[1].SpanID != "" || got[1].Timestamp != -1 {
		t.Fatalf("zero IDs must be uncorrelated and overflow must not clamp: %+v", got[1])
	}
	_, err := lgBackend().prepareLog(got[1], 1)
	requireTyped[*AdmissionError](t, "overflowed timestamp", err)
}

func TestPartitionLogs(t *testing.T) {
	t.Parallel()
	mk := func(costs ...int) []preparedLog {
		out := make([]preparedLog, len(costs))
		for i, c := range costs {
			out[i] = preparedLog{eventID: fmt.Sprint(i), cost: c}
		}
		return out
	}
	shape := func(bs []logBatch) []int {
		out := []int{}
		for _, b := range bs {
			out = append(out, len(b.logs))
		}
		return out
	}
	if got := shape(partitionLogs(mk(1, 1, 1, 1, 1), 2)); !reflect.DeepEqual(got, []int{2, 2, 1}) {
		t.Fatalf("size partition: %v", got)
	}
	if got := shape(partitionLogs(mk(logBatchBudget-10, 20, logBatchBudget*2, 5), 64)); !reflect.DeepEqual(got, []int{1, 1, 1, 1}) {
		t.Fatalf("budget partition: %v", got)
	}
	if got := partitionLogs(nil, 4); len(got) != 0 {
		t.Fatalf("empty partition: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Real FDB integration
// ---------------------------------------------------------------------------

func TestLogsRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	t.Run("export_and_query/default", func(t *testing.T) { testLogsExportQuery(t, cluster, nil) })
	t.Run("export_and_query/h1_collisions", func(t *testing.T) { testLogsExportQuery(t, cluster, func(c *ExporterConfig) { c.HashBytes = 1 }) })
	t.Run("replace_and_retract", func(t *testing.T) { testLogsReplace(t, cluster) })
	t.Run("large_chunked", func(t *testing.T) { testLogsLarge(t, cluster) })
	t.Run("admission_atomic_and_isolation", func(t *testing.T) { testLogsAdmissionIsolation(t, cluster) })
	t.Run("budget", func(t *testing.T) { testLogsBudget(t, cluster) })
	t.Run("batching_and_otlp", func(t *testing.T) { testLogsBatching(t, cluster) })
}

func lgCensus(t *testing.T, ctx context.Context, e *Exporter, name string) int64 {
	t.Helper()
	sz, err := e.backend.size(ctx, e.backend.root.Sub(name, int64(e.Config().Tenant)))
	if err != nil {
		t.Fatal(err)
	}
	return sz.Keys
}

func testLogsExportQuery(t *testing.T, cluster string, mutate func(*ExporterConfig)) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, mutate)
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
	stats, err := e.ExportLogs(ctx, in)
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

	byTrace, err := e.QueryLogsByTrace(ctx, T, "")
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
		{T2, "", []string{"e7"}},
		{qaID(0x999, 16), "", []string{}},
		{T, qaID(0x77, 8), []string{}},
	} {
		got, err := e.QueryLogsByTrace(ctx, c.trace, c.span)
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
		{Attribute{LogScopeRecord, "gen_ai.evaluation.score.value", "string", "0.8"}, 0, []string{"e1"}},
		{Attribute{LogScopeIntrinsic, "event_name", "string", "gen_ai.client.inference.operation.details"}, 0, []string{ids[2]}},
		{Attribute{"resource", "service.name", "string", "evaluator"}, 0, []string{"e5"}},
		{Attribute{LogScopeRecord, "kind", "string", "receiver-time"}, 0, []string{ids[5]}},
		{Attribute{"resource", "gen_ai.evaluation.name", "string", "relevance"}, 0, []string{}},
		{Attribute{LogScopeRecord, "gen_ai.evaluation.name", "string", "toxicity"}, 0, []string{}},
	} {
		got, err := e.QueryLogsByAttribute(ctx, c.a, c.limit)
		if err != nil || got == nil || !reflect.DeepEqual(lgIDs(got), c.want) {
			t.Fatalf("logs by %+v limit %d: %v %v want %v", c.a, c.limit, lgIDs(got), err, c.want)
		}
	}
	got, err := e.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "kind", "string", "receiver-time"}, 0)
	if err != nil || got[0].TimeSource != LogTimeReceiver || got[0].EffectiveTime < before || got[0].EffectiveTime > time.Now().UnixNano() || got[0].Timestamp != 0 || got[0].ObservedTimestamp != 0 {
		t.Fatalf("receiver-time fallback: %+v %v", got, err)
	}
	uncorrelated, err := e.QueryLogsByAttribute(ctx, relevance, 1)
	if err != nil || uncorrelated[0].TraceID != "" {
		t.Fatalf("uncorrelated log by attribute: %+v %v", uncorrelated, err)
	}
	lgRequireLog(t, "uncorrelated round trip", uncorrelated[0], want[4])

	for name, err := range map[string]error{
		"bad trace": func() error { _, err := e.QueryLogsByTrace(ctx, "abc", ""); return err }(),
		"bad span":  func() error { _, err := e.QueryLogsByTrace(ctx, T, "zz"); return err }(),
		"span scope": func() error {
			_, err := e.QueryLogsByAttribute(ctx, Attribute{"span", "k", "string", "v"}, 0)
			return err
		}(),
		"negative limit": func() error { _, err := e.QueryLogsByAttribute(ctx, relevance, -1); return err }(),
		"noncanonical int": func() error {
			_, err := e.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "k", "int", "+1"}, 0)
			return err
		}(),
	} {
		requireTyped[*ConfigError](t, name, err)
	}
	if n := lgCensus(t, ctx, e, logTraceSubspace); n != 5 {
		t.Fatalf("LOG_TRACE census %d want 5", n)
	}
}

func testLogsReplace(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	T, T2, S := qaID(0x300, 16), qaID(0x301, 16), qaID(0x31, 8)
	oldAttr := Attribute{LogScopeRecord, "phase", "string", "draft"}
	newAttr := Attribute{LogScopeRecord, "phase", "string", "final"}
	if _, err := e.ExportLogs(ctx, []LogRecord{{EventID: "r1", Timestamp: 10, TraceID: T, SpanID: S, EventName: "old", Attributes: []*commonpb.KeyValue{exKV("phase", exStr("draft")), exKV("a", exInt(1))}}}); err != nil {
		t.Fatal(err)
	}
	stats, err := e.ExportLogs(ctx, []LogRecord{{EventID: "r1", Timestamp: 20, TraceID: T2, Attributes: []*commonpb.KeyValue{exKV("phase", exStr("final"))}}})
	if err != nil || stats.Replaced != 1 || stats.Logs != 1 {
		t.Fatalf("replace: %+v %v", stats, err)
	}
	for _, c := range []struct {
		trace string
		want  []string
	}{{T, []string{}}, {T2, []string{"r1"}}} {
		got, err := e.QueryLogsByTrace(ctx, c.trace, "")
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
		got, err := e.QueryLogsByAttribute(ctx, c.a, 0)
		if err != nil || !reflect.DeepEqual(lgIDs(got), c.want) {
			t.Fatalf("attribute %+v after replace: %v %v", c.a, lgIDs(got), err)
		}
	}
	// The old version's postings and trace membership were retracted exactly.
	if eq, tr := lgCensus(t, ctx, e, logEqSubspace), lgCensus(t, ctx, e, logTraceSubspace); eq != 1 || tr != 1 {
		t.Fatalf("census after replace: eq=%d trace=%d", eq, tr)
	}
	// Within one call the later duplicate wins.
	stats, err = e.ExportLogs(ctx, []LogRecord{
		{EventID: "d1", Timestamp: 1, Attributes: []*commonpb.KeyValue{exKV("v", exStr("one"))}},
		{EventID: "d1", Timestamp: 2, Attributes: []*commonpb.KeyValue{exKV("v", exStr("two"))}},
	})
	if err != nil || stats.DuplicateLogs != 1 || stats.Logs != 1 || !reflect.DeepEqual(stats.EventIDs, []string{"d1", "d1"}) {
		t.Fatalf("duplicate stats: %+v %v", stats, err)
	}
	for v, want := range map[string][]string{"one": {}, "two": {"d1"}} {
		got, err := e.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "v", "string", v}, 0)
		if err != nil || !reflect.DeepEqual(lgIDs(got), want) {
			t.Fatalf("duplicate %s: %v %v", v, lgIDs(got), err)
		}
	}
	// Replaying an identical log is idempotent.
	replay := LogRecord{EventID: "d1", Timestamp: 2, Attributes: []*commonpb.KeyValue{exKV("v", exStr("two"))}}
	if stats, err := e.ExportLogs(ctx, []LogRecord{replay}); err != nil || stats.Replaced != 1 {
		t.Fatalf("replay: %+v %v", stats, err)
	}
	if eq := lgCensus(t, ctx, e, logEqSubspace); eq != 2 {
		t.Fatalf("census after replay: %d", eq)
	}
}

func testLogsLarge(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	T := qaID(0x400, 16)
	body := lgRandom(t, 300000)
	big := LogRecord{EventID: "big", Timestamp: 7, TraceID: T, Body: exBytes(body), Attributes: []*commonpb.KeyValue{exKV("size", exStr("large"))}}
	stats, err := e.ExportLogs(ctx, []LogRecord{big, {EventID: "zip", Timestamp: 8, TraceID: T, Body: exStr(strings.Repeat("compressible ", 50000))}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.StoredBytes >= stats.LogicalBytes {
		t.Fatalf("Brotli must shrink the compressible log: %+v", stats)
	}
	if n := lgCensus(t, ctx, e, logRecordSubspace); n != 1+4+1 {
		t.Fatalf("record rows %d want header+4 chunks+inline", n)
	}
	got, err := e.QueryLogsByTrace(ctx, T, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("large logs: %v %v", lgIDs(got), err)
	}
	if !bytes.Equal(got[0].Body.GetBytesValue(), body) || got[1].Body.GetStringValue() != strings.Repeat("compressible ", 50000) {
		t.Fatal("large body round trip")
	}
	// Shrinking a chunked log clears its stale chunks.
	if _, err := e.ExportLogs(ctx, []LogRecord{{EventID: "big", Timestamp: 7, TraceID: T, Body: exStr("small")}}); err != nil {
		t.Fatal(err)
	}
	if n := lgCensus(t, ctx, e, logRecordSubspace); n != 2 {
		t.Fatalf("record rows after shrink %d want 2", n)
	}
	got, err = e.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "size", "string", "large"}, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("stale large posting: %v %v", lgIDs(got), err)
	}
}

func testLogsAdmissionIsolation(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, nil)
	T := qaID(0x500, 16)
	_, err := e.ExportLogs(ctx, []LogRecord{{EventID: "ok", Timestamp: 1, TraceID: T}, {EventID: "bad", TraceID: "nothex"}})
	requireTyped[*AdmissionError](t, "mixed batch", err)
	for _, name := range []string{logRecordSubspace, logTraceSubspace, logEqSubspace} {
		if n := lgCensus(t, ctx, e, name); n != 0 {
			t.Fatalf("rejected call wrote %d %s keys", n, name)
		}
	}
	a := Attribute{LogScopeRecord, "k", "string", "v"}
	if _, err := e.ExportLogs(ctx, []LogRecord{{EventID: "ok", Timestamp: 1, TraceID: T, Attributes: []*commonpb.KeyValue{exKV("k", exStr("v"))}}}); err != nil {
		t.Fatal(err)
	}
	other := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Run, c.Tenant = e.Config().Run, 12 })
	if got, err := other.QueryLogsByTrace(ctx, T, ""); err != nil || len(got) != 0 {
		t.Fatalf("tenant isolation by trace: %v %v", lgIDs(got), err)
	}
	if got, err := other.QueryLogsByAttribute(ctx, a, 0); err != nil || len(got) != 0 {
		t.Fatalf("tenant isolation by attribute: %v %v", lgIDs(got), err)
	}
	if got, err := e.QueryLogsByAttribute(ctx, a, 0); err != nil || !reflect.DeepEqual(lgIDs(got), []string{"ok"}) {
		t.Fatalf("owner tenant: %v %v", lgIDs(got), err)
	}

	shutdown, cancelShutdown := context.WithTimeout(ctx, 30*time.Second)
	defer cancelShutdown()
	if err := other.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	_, err = other.ExportLogs(ctx, []LogRecord{{EventID: "late"}})
	requireOtelKind(t, err, "operation")
	_, err = other.QueryLogsByTrace(ctx, T, "")
	requireOtelKind(t, err, "operation")
	_, err = other.QueryLogsByAttribute(ctx, a, 0)
	requireOtelKind(t, err, "operation")
}

func testLogsBudget(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.MaxCandidates = 2 })
	T := qaID(0x600, 16)
	var logs []LogRecord
	for i := range 3 {
		logs = append(logs, LogRecord{EventID: fmt.Sprint("b", i), Timestamp: int64(10 + i), TraceID: T, Attributes: []*commonpb.KeyValue{exKV("k", exStr("v"))}})
	}
	if _, err := e.ExportLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	_, err := e.QueryLogsByTrace(ctx, T, "")
	requireOtelBudget(t, err)
	a := Attribute{LogScopeRecord, "k", "string", "v"}
	_, err = e.QueryLogsByAttribute(ctx, a, 0)
	requireOtelBudget(t, err)
	// One posting plus one record row fits the two-candidate budget.
	got, err := e.QueryLogsByAttribute(ctx, a, 1)
	if err != nil || !reflect.DeepEqual(lgIDs(got), []string{"b2"}) {
		t.Fatalf("bounded attribute query: %v %v", lgIDs(got), err)
	}
}

func testLogsBatching(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 8, 4 })
	T := qaID(0x700, 16)
	var logs []LogRecord
	for i := range 200 {
		logs = append(logs, LogRecord{Timestamp: int64(1000 + i), TraceID: T, SpanID: qaID(i%5, 8), Attributes: []*commonpb.KeyValue{exKV("bulk", exStr("yes"))}})
	}
	stats, err := e.ExportLogs(ctx, logs)
	if err != nil || stats.Logs != 200 || stats.Batches != 25 || stats.GeneratedIDs != 200 {
		t.Fatalf("batched export: %+v %v", stats, err)
	}
	got, err := e.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "bulk", "string", "yes"}, 200)
	if err != nil || len(got) != 200 || got[0].EffectiveTime != 1199 || got[199].EffectiveTime != 1000 {
		t.Fatalf("bulk by attribute: %d %v", len(got), err)
	}
	perSpan, err := e.QueryLogsByTrace(ctx, T, qaID(3, 8))
	if err != nil || len(perSpan) != 40 {
		t.Fatalf("per-span logs: %d %v", len(perSpan), err)
	}
	for i := 1; i < len(perSpan); i++ {
		if perSpan[i-1].EffectiveTime >= perSpan[i].EffectiveTime {
			t.Fatal("per-span logs out of time order")
		}
	}

	otlp := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: 5, TraceId: qaBytes(t, qaID(0x701, 16)), Body: exStr("from collector")}}}}}}
	if err := e.ConsumeLogs(ctx, otlp); err != nil {
		t.Fatal(err)
	}
	fromCollector, err := e.QueryLogsByTrace(ctx, qaID(0x701, 16), "")
	if err != nil || len(fromCollector) != 1 || fromCollector[0].Body.GetStringValue() != "from collector" {
		t.Fatalf("ConsumeLogs: %+v %v", fromCollector, err)
	}
}
