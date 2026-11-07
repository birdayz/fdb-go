package otel

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

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
	chunkedHeader, err := tuple.Unpack(rows[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	chunkedHeader[4] = int64(len(rows)) // Declare four chunks while preserving all three payload rows.
	wrongChunkCount := clone(rows)
	wrongChunkCount[0].Value = chunkedHeader.Pack()
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
		"chunk count":      {space, wrongChunkCount},
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
	traceKey, postings, _ := logIndexKeys(b.keys, b.cfg.Tenant, b.cfg.HashBytes, l)
	if !bytes.Equal(traceKey, logTraceSpace(b.keys, 3).Pack(tuple.Tuple{l.TraceID, l.SpanID, int64(50), "ev"})) {
		t.Fatal("LOG_TRACE key layout")
	}
	if len(postings) != len(want) {
		t.Fatalf("postings: %d", len(postings))
	}
	for ordinal, p := range postings {
		a := want[ordinal]
		if !bytes.Equal(p.Key, logTermPrefix(b.keys, 3, a, 8).Pack(tuple.Tuple{int64(-50), "ev", int64(ordinal)})) {
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
	if traceKey, _, _ := logIndexKeys(b.keys, 3, 8, l); traceKey != nil {
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
