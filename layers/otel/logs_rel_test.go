package otel

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// packedLen is the tuple encoder's length for every column type a LOGS key
// holds, each integer width and NUL escaping included.
func TestPackedLen(t *testing.T) {
	t.Parallel()
	for _, v := range []any{
		nil, "", "a", "a\x00b\x00", strings.Repeat("\x00", 9),
		[]byte{},
		[]byte{0, 1, 0},
		[]byte("xyz"),
		int64(0), int64(1), int64(255), int64(256), int64(-1), int64(-255), int64(-256), int64(1 << 40), int64(math.MaxInt64), int64(math.MinInt64),
		0.5, math.NaN(), math.Inf(-1), true, false, int64(-(1 << 56)),
	} {
		if got, want := packedLen(v), len(tuple.Tuple{v}.Pack()); got != want {
			t.Errorf("packedLen(%#v) = %d, want %d", v, got, want)
		}
	}
	if got := packedLen([]byte(nil)); got != len(tuple.Tuple{nil}.Pack()) {
		t.Errorf("a NULL bytes column packs as nil: %d", got)
	}
}

// TestRelLogPrepareAdmission: the row builder refuses invalid times,
// identities and UTF-8 and oversized logs, normalizes identities and picks
// the effective time. The raw cap is the only size cap per log (§3.1), so
// "oversized" is a raw image over 8 MiB.
func TestRelLogPrepareAdmission(t *testing.T) {
	t.Parallel()
	s := lrUnitStore(t)
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
		"oversized":         {EventID: "a", Body: exBytes(lgRandom(t, maxLogRawBytes+1024))},
		"invalid UTF-8":     {EventID: "a", Attributes: []*commonpb.KeyValue{exKV("k", exStr("\xff"))}},
	} {
		_, err := s.prepare(l, 1)
		requireTyped[*AdmissionError](t, name, err)
	}
	upper := strings.ToUpper(qaID(0xabc, 16))
	p, err := s.prepare(LogRecord{EventID: "u", Timestamp: 9, TraceID: upper, SpanID: strings.ToUpper(qaID(0xdef, 8))}, 1)
	if err != nil || !p.correlated {
		t.Fatalf("uppercase identities: %+v %v", p, err)
	}
	got, err := decodeLogRow("test", p.row)
	if err != nil || got.TraceID != qaID(0xabc, 16) || got.SpanID != qaID(0xdef, 8) || hexID(p.row.TRACE_ID) != got.TraceID {
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
		p := lrPrepared(t, s, c.l)
		got, err := decodeLogRow("test", p.row)
		if err != nil || got.EffectiveTime != c.effect || got.TimeSource != c.source || got.Timestamp != c.l.Timestamp || got.ObservedTimestamp != c.l.ObservedTimestamp ||
			p.row.GetEFFECTIVE_NS() != c.effect || p.row.GetTIME_SOURCE() != c.source {
			t.Fatalf("time fallback %+v: %+v %v", c.l, got, err)
		}
	}
	// An overflowed OTLP time is refused, never clamped.
	conv := LogRecordsFromResourceLogs([]*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: math.MaxUint64}}}}}})
	_, err = s.prepare(conv[0], 1)
	requireTyped[*AdmissionError](t, "overflowed timestamp", err)
}

// TestRelLogProjection pins the projection rules (§2.4): the LOGS row holds
// each scope's canonical set, typed (D1), with event_name intrinsic; the key
// layout is the template's, pinned in TestRelLogsRealFDB/index_key_model.
func TestRelLogProjection(t *testing.T) {
	t.Parallel()
	s := lrUnitStore(t)
	l := LogRecord{
		EventID: "ev", Timestamp: 50, TraceID: qaID(1, 16), SpanID: qaID(2, 8), EventName: "gen_ai.evaluation.result",
		ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc"))},
		ScopeAttributes:    []*commonpb.KeyValue{exKV("big", exStr(strings.Repeat("x", maxIndexedValueBytes+1)))},
		Attributes: []*commonpb.KeyValue{
			exKV("k", exStr("first")), nil, exKV("flag", exBool(true)), exKV("k", exStr("last")),
			exKV("score", exDouble(0.5)), exKV("n", exInt(-4)), exKV("list", exArray(exStr("a"), exInt(1))),
		},
	}
	p := lrPrepared(t, s, l)
	r := p.row
	var str []string
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		str = append(str, a.GetSCOPE()+"/"+a.GetATTR_KEY()+"="+a.GetSTR_VALUE())
	}
	if want := []string{"resource/service.name=svc", "log/k=last"}; !reflect.DeepEqual(str, want) {
		t.Fatalf("string attributes %v, want %v", str, want)
	}
	if b := r.GetBOOL_ATTRS().GetValues(); len(b) != 1 || b[0].GetATTR_KEY() != "flag" || !b[0].GetBOOL_VALUE() {
		t.Fatalf("bool attributes %v", b)
	}
	if d := r.GetDOUBLE_ATTRS().GetValues(); len(d) != 1 || d[0].GetDOUBLE_VALUE() != 0.5 {
		t.Fatalf("double attributes %v", d)
	}
	if n := r.GetINT_ATTRS().GetValues(); len(n) != 1 || n[0].GetINT_VALUE() != -4 {
		t.Fatalf("int attributes %v", n)
	}
	if a := r.GetANY_ATTRS().GetValues(); len(a) != 1 || a[0].GetJSON_VALUE() != `["a",1]` {
		t.Fatalf("any attributes %v", a)
	}
	// D1: the array is reported, not indexed, so two keys are unindexed.
	if r.GetEVENT_NAME() != l.EventName || len(r.GetLONG_STR_ATTRS().GetValues()) != 0 || p.unindexed != 2 {
		t.Fatalf("event name %q, long strings %v, unindexed %d", r.GetEVENT_NAME(), r.GetLONG_STR_ATTRS(), p.unindexed)
	}
	attr := func(a Attribute) relLogAttr {
		q, err := newRelLogAttr("test", a)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	for _, a := range []Attribute{
		{"resource", "service.name", "string", "svc"},
		{LogScopeRecord, "k", "string", "last"},
		{LogScopeRecord, "flag", "bool", "true"},
		{LogScopeRecord, "score", "double", "0.5"},
		{LogScopeRecord, "n", "int", "-4"},
		{LogScopeIntrinsic, "event_name", "string", "gen_ai.evaluation.result"},
	} {
		if q := attr(a); q.none || !q.holds(r) {
			t.Fatalf("the row does not hold its own attribute %+v (%+v)", a, q)
		}
	}
	for _, a := range []Attribute{
		{LogScopeRecord, "k", "string", "first"},
		{LogScopeRecord, "flag", "string", "true"},
		{LogScopeRecord, "score", "string", "0.5"},
		{"scope", "n", "int", "-4"},
		{LogScopeRecord, "n", "double", "-4"},
	} {
		if q := attr(a); !q.none && q.holds(r) {
			t.Fatalf("the row holds %+v", a)
		}
	}
	if q := attr(Attribute{LogScopeIntrinsic, "severity", "string", "x"}); !q.none {
		t.Fatalf("an intrinsic other than event_name names an index: %+v", q)
	}
	for name, a := range map[string]Attribute{
		"span scope":     {"span", "k", "string", "v"},
		"bytes":          {LogScopeRecord, "k", "bytes", "AQI="},
		"long event":     {LogScopeIntrinsic, "event_name", "string", strings.Repeat("e", 1025)},
		"over the cap":   {LogScopeRecord, "k", "string", strings.Repeat("v", maxIndexedValueBytes+1)},
		"noncanonical":   {LogScopeRecord, "k", "int", "+1"},
		"double as text": {LogScopeRecord, "k", "double", "0.50"},
	} {
		_, err := newRelLogAttr("test", a)
		requireTyped[*ConfigError](t, name, err)
	}
	l.TraceID, l.SpanID = "", ""
	if u := lrPrepared(t, s, l); u.row.TRACE_ID != nil || u.row.SPAN_ID != nil || u.correlated {
		t.Fatal("an uncorrelated log has NULL trace and span columns")
	}
}

// TestPartitionRelLogs: a batch ends at the batch size or before the log that
// would take it past logBatchBudget; an over-budget log is a batch alone.
func TestPartitionRelLogs(t *testing.T) {
	t.Parallel()
	mk := func(costs ...int) []relPreparedLog {
		out := make([]relPreparedLog, len(costs))
		for i, c := range costs {
			out[i] = relPreparedLog{eventID: fmt.Sprint(i), cost: c}
		}
		return out
	}
	shape := func(bs []relLogBatch) []int {
		out := []int{}
		for _, b := range bs {
			out = append(out, len(b.logs))
		}
		return out
	}
	if got := shape(partitionRelLogs(mk(1, 1, 1, 1, 1), 2)); !reflect.DeepEqual(got, []int{2, 2, 1}) {
		t.Fatalf("size partition: %v", got)
	}
	if got := shape(partitionRelLogs(mk(logBatchBudget-10, 20, logBatchBudget*2, 5), 64)); !reflect.DeepEqual(got, []int{1, 1, 1, 1}) {
		t.Fatalf("budget partition: %v", got)
	}
	if got := partitionRelLogs(nil, 4); len(got) != 0 {
		t.Fatalf("empty partition: %v", got)
	}
}

// Every arm of the row size: uncompressed (no serializer), bounded (an
// unknown codec), and deflated exactly only for a row over the budget.
func TestRelLogRowBytesArms(t *testing.T) {
	t.Parallel()
	zlib := lrUnitStore(t)
	// Highly compressible, and over the budget uncompressed: body_text holds
	// the string body again beside the otlp envelope.
	body := strings.Repeat("compressible text ", 250_000)
	l := LogRecord{EventID: "big", Timestamp: 1, Body: exStr(body)}
	p, err := zlib.prepare(l, 1)
	if err != nil {
		t.Fatalf("a compressible log under the raw cap is refused under zlib: %v", err)
	}
	bound, err := zlib.rowBytes(p.row, false)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := zlib.rowBytes(p.row, true)
	if err != nil {
		t.Fatal(err)
	}
	if bound <= maxLogTxnWriteBytes || exact >= bound/10 || p.writeCost >= maxLogTxnWriteBytes || p.logical > maxLogRawBytes {
		t.Fatalf("fixture: bound %d, deflated %d, cost %d, raw %d", bound, exact, p.writeCost, p.logical)
	}
	if n := proto.Size(p.row); bound != n+2+protowireLen(n) {
		t.Fatalf("bound %d of a %d-byte row", bound, n)
	}
	unknown := *zlib
	unknown.zlibLevel = noZlib
	_, err = unknown.prepare(l, 1)
	requireTyped[*AdmissionError](t, "unknown codec bounds by size", err)
	none := *zlib
	none.zlibLevel = 0
	_, err = none.prepare(l, 1)
	requireTyped[*AdmissionError](t, "uncompressed store", err)
	if n, _ := none.rowBytes(p.row, true); n != bound-1 {
		t.Fatalf("an uncompressed row is its union: %d, bound %d", n, bound)
	}
	// A small row is never deflated at admission.
	small := lrPrepared(t, zlib, LogRecord{EventID: "s", Timestamp: 1, Body: exStr(strings.Repeat("x", 1000))})
	if n, _ := zlib.rowBytes(small.row, false); small.writeCost < n {
		t.Fatalf("small cost %d under its row bound %d", small.writeCost, n)
	}
}

// TestRelLogOTLPFidelityEncoding: the LOGS row's otlp column
// is the owned envelope, unaffected by caller or projection edits, and the
// row's indexed sets keep the last duplicate without altering it.
func TestRelLogOTLPFidelityEncoding(t *testing.T) {
	t.Parallel()
	s := lrUnitStore(t)
	for _, rl := range logFidelityFixture(t) {
		want := proto.Clone(rl).(*logspb.ResourceLogs)
		logs := LogRecordsFromResourceLogs([]*logspb.ResourceLogs{rl})
		if len(logs) != 1 {
			t.Fatalf("converted %d logs, want one", len(logs))
		}
		l := logs[0]
		l.EventID = "owned-envelope"
		rl.SchemaUrl = "mutated input"
		rl.ProtoReflect().SetUnknown(nil)
		rl.ScopeLogs[0].LogRecords[0].Body = exStr("mutated input")
		l.Body.Value = &commonpb.AnyValue_StringValue{StringValue: "mutated projection"}
		l.Attributes[0].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "mutated projection"}
		l.Timestamp, l.TraceID, l.EventName = -1, "invalid projection", "mutated projection"
		requireLogFidelity(t, l, want)

		p := lrPrepared(t, s, l)
		got, err := decodeLogRow("test", p.row)
		if err != nil {
			t.Fatal(err)
		}
		requireLogFidelity(t, got, want)
		// The oversize attribute is the one unindexed key; the bool is
		// indexed as a bool (D1).
		if got.EventID != l.EventID || got.EventName != want.ScopeLogs[0].LogRecords[0].EventName || p.unindexed != 1 {
			t.Fatalf("source projections not used: %+v, unindexed=%d", got, p.unindexed)
		}
		last, err := newRelLogAttr("test", Attribute{LogScopeRecord, "duplicate", "string", "last"})
		if err != nil {
			t.Fatal(err)
		}
		first, err := newRelLogAttr("test", Attribute{LogScopeRecord, "duplicate", "string", "first"})
		if err != nil {
			t.Fatal(err)
		}
		if !last.holds(p.row) || first.holds(p.row) || len(got.Attributes) != 5 {
			t.Fatal("index must use last duplicate without altering stored attributes")
		}
		if got.Timestamp == 0 && (got.TraceID != "" || got.SpanID != "" || p.correlated || p.row.TRACE_ID != nil || got.EffectiveTime != 777 || got.TimeSource != LogTimeReceiver) {
			t.Fatalf("zero IDs and receiver time must remain independent projections: %+v", got)
		}
	}
}

// decodeLogRow refuses a row whose columns disagree with its envelope.
func TestRelLogRowDecodeRejectsDisagreement(t *testing.T) {
	t.Parallel()
	s := lrUnitStore(t)
	base := LogRecord{EventID: "r", Timestamp: 5, ObservedTimestamp: 6, TraceID: qaID(1, 16), SpanID: qaID(2, 8)}
	for name, damage := range map[string]func(p *relPreparedLog){
		"time source":    func(p *relPreparedLog) { p.row.TIME_SOURCE = proto.String("guess") },
		"observed time":  func(p *relPreparedLog) { p.row.TIME_SOURCE = proto.String(LogTimeObserved) },
		"effective time": func(p *relPreparedLog) { p.row.EFFECTIVE_NS = proto.Int64(6) },
		"negative time":  func(p *relPreparedLog) { p.row.EFFECTIVE_NS = proto.Int64(-1) },
		"trace column":   func(p *relPreparedLog) { p.row.TRACE_ID = qaBytes(t, qaID(3, 16)) },
		"span column":    func(p *relPreparedLog) { p.row.SPAN_ID = nil },
		"otlp garbage":   func(p *relPreparedLog) { p.row.OTLP = []byte{0xff} },
		"two records": func(p *relPreparedLog) {
			rl := &logspb.ResourceLogs{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: 5}, {TimeUnixNano: 5}}}}}
			p.row.OTLP, _ = proto.Marshal(rl)
		},
	} {
		p := lrPrepared(t, s, base)
		if _, err := decodeLogRow("test", p.row); err != nil {
			t.Fatalf("%s: positive control: %v", name, err)
		}
		damage(&p)
		_, err := decodeLogRow("test", p.row)
		requireTyped[*CorruptionError](t, name, err)
	}
}

// Every arm of logEntryAt: the identity columns of an attribute entry
// (scope, key, value, effective_ns, event_id, trace_id, span_id, pk...).
func TestRelLogEntryAt(t *testing.T) {
	t.Parallel()
	s := lrUnitStore(t)
	idx := s.index[relLogStr].idx
	tr, sp := qaBytes(t, qaID(1, 16)), qaBytes(t, qaID(2, 8))
	entry := func(cols ...tuple.TupleElement) *recordlayer.IndexEntry {
		key := append(tuple.Tuple{"log", "k", "v"}, cols...)
		return recordlayer.NewIndexEntry(idx, key, nil, tuple.Tuple{int64(3), "ev"})
	}
	for name, c := range map[string]struct {
		e    *recordlayer.IndexEntry
		want relLogEntry
	}{
		"correlated":   {entry(int64(5), "ev", tr, sp), relLogEntry{eventID: "ev", effective: 5, trace: tr, span: sp}},
		"trace only":   {entry(int64(5), "ev", tr, nil), relLogEntry{eventID: "ev", effective: 5, trace: tr}},
		"uncorrelated": {entry(int64(5), "ev", nil, nil), relLogEntry{eventID: "ev", effective: 5}},
	} {
		got, err := logEntryAt("test", c.e, 3, [4]int{0, 1, 2, 3})
		c.want.pk = tuple.Tuple{int64(3), "ev"}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	for name, e := range map[string]*recordlayer.IndexEntry{
		"short":          entry(int64(5), "ev", tr),
		"time type":      entry("5", "ev", tr, sp),
		"id type":        entry(int64(5), int64(1), tr, sp),
		"short trace":    entry(int64(5), "ev", tr[:15], sp),
		"zero trace":     entry(int64(5), "ev", make([]byte, 16), sp),
		"trace type":     entry(int64(5), "ev", "trace", sp),
		"short span":     entry(int64(5), "ev", tr, sp[:7]),
		"zero span":      entry(int64(5), "ev", tr, make([]byte, 8)),
		"span w/o trace": entry(int64(5), "ev", nil, sp),
	} {
		_, err := logEntryAt("test", e, 3, [4]int{0, 1, 2, 3})
		requireTyped[*CorruptionError](t, name, err)
	}
	// LOGS_TRACE's order: (trace_id, span_id, effective_ns, event_id).
	got, err := logEntryAt("test", recordlayer.NewIndexEntry(s.index[relLogTrace].idx, tuple.Tuple{tr, nil, int64(9), "x"}, nil, nil), 0, [4]int{2, 3, 0, 1})
	if err != nil || got.eventID != "x" || got.effective != 9 || !bytes.Equal(got.trace, tr) || got.span != nil {
		t.Fatalf("trace order: %+v %v", got, err)
	}
}

func protowireLen(n int) int {
	l := 1
	for v := uint64(n); v >= 0x80; v >>= 7 { //nolint:gosec // a size
		l++
	}
	return l
}
