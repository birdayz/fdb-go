package otel

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// TestRelPartition: batches end at BatchSize spans or at the
// conservative transaction estimate, whichever comes first.
func TestRelPartition(t *testing.T) {
	t.Parallel()
	w := &SpanWriter{opts: SpanWriterOptions{BatchSize: 2}}
	if got := w.partition(nil); len(got) != 0 {
		t.Fatalf("empty partition: %+v", got)
	}
	items := make([]relSpan, 5)
	for i := range items {
		items[i] = relSpan{logical: int64(10 * (i + 1)), cost: 100, references: i}
	}
	type shape struct {
		first, next, n int
		logical        int64
		references     int64
		estimate       int
	}
	shapes := func(bs []relBatch) []shape {
		var out []shape
		for _, b := range bs {
			out = append(out, shape{b.first, b.next, len(b.spans), b.logical, b.references, b.estimate})
		}
		return out
	}
	want := []shape{{0, 2, 2, 30, 1, 200}, {2, 4, 2, 70, 5, 200}, {4, 5, 1, 50, 4, 100}}
	if got := shapes(w.partition(items)); !reflect.DeepEqual(got, want) {
		t.Fatalf("size partition\n got %+v\nwant %+v", got, want)
	}
	// The conservative transaction estimate splits independently of size.
	w.opts.BatchSize = 64
	items = []relSpan{{cost: 200000}, {cost: 200000}, {cost: 50000}, {cost: 50000}, {cost: 1}}
	want = []shape{{0, 1, 1, 0, 0, 200000}, {1, 4, 3, 0, 0, exportSpanBudget}, {4, 5, 1, 0, 0, 1}}
	if got := shapes(w.partition(items)); !reflect.DeepEqual(got, want) {
		t.Fatalf("budget partition\n got %+v\nwant %+v", got, want)
	}
}

// rowAttributes are a SPANS row's indexed typed attributes (D1).
func rowAttributes(r *otelv1pb.SPANS) []Attribute {
	var out []Attribute
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		out = append(out, Attribute{a.GetSCOPE(), a.GetATTR_KEY(), "string", a.GetSTR_VALUE()})
	}
	for _, a := range r.GetLONG_STR_ATTRS().GetValues() {
		out = append(out, Attribute{a.GetSCOPE(), a.GetATTR_KEY(), "string", a.GetSTR_VALUE()})
	}
	for _, a := range r.GetINT_ATTRS().GetValues() {
		out = append(out, Attribute{a.GetSCOPE(), a.GetATTR_KEY(), "int", strconv.FormatInt(a.GetINT_VALUE(), 10)})
	}
	for _, a := range r.GetDOUBLE_ATTRS().GetValues() {
		out = append(out, Attribute{a.GetSCOPE(), a.GetATTR_KEY(), "double", otelsql.FormatDouble(a.GetDOUBLE_VALUE())})
	}
	for _, a := range r.GetBOOL_ATTRS().GetValues() {
		out = append(out, Attribute{a.GetSCOPE(), a.GetATTR_KEY(), "bool", strconv.FormatBool(a.GetBOOL_VALUE())})
	}
	sort.Slice(out, func(i, j int) bool { return attributeLess(out[i], out[j]) })
	return out
}

func sortedAttributes(as ...Attribute) []Attribute {
	sort.Slice(as, func(i, j int) bool { return attributeLess(as[i], as[j]) })
	return as
}

// TestRelPrepare pins the row builder: bools and doubles are typed values
// (D1), so they normalize nothing; the row's otlp is the original
// single-span ResourceSpans (§1.2), not a canonicalized one.
func TestRelPrepare(t *testing.T) {
	t.Parallel()
	w := &SpanWriter{opts: SpanWriterOptions{BatchSize: 64}}
	traceID, spanA, spanB := exRandomID(t, 16), exRandomID(t, 8), exRandomID(t, 8)
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 4242, protowire.VarintType), 7)
	rs := &tracepb.ResourceSpans{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc")), exKV("res.flag", exBool(true))}},
		SchemaUrl: "schema/r",
		ScopeSpans: []*tracepb.ScopeSpans{nil, {
			Scope:     &commonpb.InstrumentationScope{Name: "scope", Attributes: []*commonpb.KeyValue{exKV("scope.team", exStr("ml"))}},
			SchemaUrl: "schema/s",
			Spans: []*tracepb.Span{
				{TraceId: traceID, SpanId: spanA, Name: "first", StartTimeUnixNano: 100, Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("chat")), exKV("t", exDouble(0.5))}},
				nil,
				{TraceId: traceID, SpanId: spanB, Name: "plain", StartTimeUnixNano: 200, Attributes: []*commonpb.KeyValue{exKV("n", exInt(1))}},
				{TraceId: traceID, SpanId: spanA, Name: "replacement", StartTimeUnixNano: 150, Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("embeddings")), exKV("dup", exInt(1)), exKV("dup", exInt(2))}},
			},
		}},
	}
	rs.ProtoReflect().SetUnknown(unknown)
	plain := &tracepb.ResourceSpans{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("plain"))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: traceID, SpanId: exRandomID(t, 8), StartTimeUnixNano: 300}}}},
	}
	before := []*tracepb.ResourceSpans{proto.Clone(rs).(*tracepb.ResourceSpans), proto.Clone(plain).(*tracepb.ResourceSpans)}
	items, stats, err := w.prepare(t.Context(), []*tracepb.ResourceSpans{nil, rs, plain}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// D1: v1 normalized the "plain" span too, its resource bool becoming a string.
	if want := (ExportStats{ReceivedSpans: 4, DuplicateSpans: 1, NormalizedSpans: 1}); stats != want {
		t.Fatalf("stats %+v want %+v", stats, want)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items", len(items))
	}
	ss := rs.ScopeSpans[1]
	resource := []Attribute{{"resource", "service.name", "string", "svc"}, {"resource", "res.flag", "bool", "true"}, {"scope", "scope.team", "string", "ml"}}
	// The replacement keeps the first occurrence's position.
	wants := []struct {
		single     *tracepb.ResourceSpans
		start      int64
		indexed    []Attribute
		normalized bool
	}{
		{exSingle(rs, ss, ss.Spans[3]), 150, sortedAttributes(append(resource[:3:3], Attribute{"span", "gen_ai.operation.name", "string", "embeddings"}, Attribute{"span", "dup", "int", "2"})...), true},
		{exSingle(rs, ss, ss.Spans[2]), 200, sortedAttributes(append(resource[:3:3], Attribute{"span", "n", "int", "1"})...), false},
		{exSingle(plain, plain.ScopeSpans[0], plain.ScopeSpans[0].Spans[0]), 300, []Attribute{{"resource", "service.name", "string", "plain"}}, false},
	}
	for i, want := range wants {
		it := items[i]
		row := it.row.(*otelv1pb.SPANS)
		sp := want.single.ScopeSpans[0].Spans[0]
		if string(row.TRACE_ID) != string(sp.TraceId) || string(row.SPAN_ID) != string(sp.SpanId) || row.GetSTART_NS() != want.start {
			t.Fatalf("item %d row identity %x/%x@%d", i, row.TRACE_ID, row.SPAN_ID, row.GetSTART_NS())
		}
		if it.key != string(sp.TraceId)+string(sp.SpanId) || it.normalized != want.normalized || it.logical != int64(len(row.OTLP)) || it.cost != spanCost(row) || it.cost <= 2048 {
			t.Fatalf("item %d metadata %+v", i, it)
		}
		if got := rowAttributes(row); !reflect.DeepEqual(got, want.indexed) {
			t.Fatalf("item %d indexed\n got %+v\nwant %+v", i, got, want.indexed)
		}
		stored, err := rowOTLP("test", row.ProtoReflect(), sp.TraceId, sp.SpanId)
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("item %d otlp", i), stored, want.single)
	}
	for i, rs := range []*tracepb.ResourceSpans{rs, plain} {
		requireProtoEqual(t, fmt.Sprintf("input %d", i), rs, before[i])
	}

	t.Run("admission", func(t *testing.T) {
		good := func() *tracepb.Span {
			return &tracepb.Span{TraceId: exRandomID(t, 16), SpanId: exRandomID(t, 8), StartTimeUnixNano: 1}
		}
		cases := map[string]func(*tracepb.Span){
			"zero trace":       func(s *tracepb.Span) { s.TraceId = make([]byte, 16) },
			"short trace":      func(s *tracepb.Span) { s.TraceId = s.TraceId[:15] },
			"missing trace":    func(s *tracepb.Span) { s.TraceId = nil },
			"zero span":        func(s *tracepb.Span) { s.SpanId = make([]byte, 8) },
			"long span":        func(s *tracepb.Span) { s.SpanId = append(s.SpanId, 1) },
			"start past int64": func(s *tracepb.Span) { s.StartTimeUnixNano = math.MaxInt64 + 1 },
			"oversized span": func(s *tracepb.Span) {
				s.Attributes = []*commonpb.KeyValue{exKV("a", exStr(strings.Repeat("x", 60000))), exKV("b", exStr(strings.Repeat("y", 60000))), exKV("c", exStr(strings.Repeat("z", 60000)))}
			},
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				bad := good()
				mutate(bad)
				in := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{good(), bad}}}}}
				items, _, err := w.prepare(t.Context(), in, nil)
				requireOtelKind(t, err, "admission")
				if items != nil {
					t.Fatal("inadmissible export returned prepared items")
				}
			})
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := w.prepare(ctx, []*tracepb.ResourceSpans{plain}, nil)
		requireOtelKind(t, err, "canceled")
	})
}

// The estimate is 2048 and the row's serialized bytes, plus key + value text
// + 512 per projected attribute and encoded index key + 512 per ref chunk.
// Every column counts: a long name or status message grows it.
func TestSpanCost(t *testing.T) {
	t.Parallel()
	row := &otelv1pb.SPANS{
		OTLP:       make([]byte, 100),
		STR_ATTRS:  &otelv1pb.SPANS_STR_ATTRS{Values: []*otelv1pb.STR_ATTR{{ATTR_KEY: proto.String("k"), STR_VALUE: proto.String("vv")}}},
		INT_ATTRS:  &otelv1pb.SPANS_INT_ATTRS{Values: []*otelv1pb.INT_ATTR{{ATTR_KEY: proto.String("n"), INT_VALUE: proto.Int64(-12)}}},
		BOOL_ATTRS: &otelv1pb.SPANS_BOOL_ATTRS{Values: []*otelv1pb.BOOL_ATTR{{ATTR_KEY: proto.String("b"), BOOL_VALUE: proto.Bool(false)}}},
		REF_CHUNKS: &otelv1pb.SPANS_REF_CHUNKS{Values: []*otelv1pb.MESSAGE_REF_CHUNK{{CHUNK: proto.Int64(0), REFS: []byte{0, 1, 0}}}},
	}
	want := 2048 + proto.Size(row) + (1 + 2 + 512) + (1 + 3 + 512) + (1 + 5 + 512) + (len(tuple.Tuple{[]byte{0, 1, 0}}.Pack()) + 512)
	if got := spanCost(row); got != want {
		t.Fatalf("cost %d, want %d", got, want)
	}
	for name, grow := range map[string]func(*otelv1pb.SPANS){
		"name":           func(r *otelv1pb.SPANS) { r.NAME = proto.String(strings.Repeat("n", 1000)) },
		"status message": func(r *otelv1pb.SPANS) { r.STATUS_MESSAGE = proto.String(strings.Repeat("s", 1000)) },
		"trace state":    func(r *otelv1pb.SPANS) { r.TRACE_STATE = proto.String(strings.Repeat("t", 1000)) },
	} {
		grown := proto.Clone(row).(*otelv1pb.SPANS)
		grow(grown)
		if got := spanCost(grown); got < want+1000 {
			t.Fatalf("a 1000-byte %s adds %d to the estimate", name, got-want)
		}
	}
	if len(tuple.Tuple{[]byte{0, 1, 0}}.Pack()) != 7 {
		t.Fatal("ref chunk bytes are not counted escaped")
	}
}

// With messages on, the GenAI content attributes leave the indexes and the
// span's references become ref_chunks; a refChunks failure fails admission.
func TestRelPrepareMessages(t *testing.T) {
	t.Parallel()
	w := &SpanWriter{opts: SpanWriterOptions{BatchSize: 64}}
	rs := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("hi"))), deltaKV("gen_ai.operation.name", deltaStr("chat")))
	sp := rs.ScopeSpans[0].Spans[0]
	uses := map[string][]messageUse{spanKey(sp): {{element: &messageElement{}, attribute: 1, ordinals: []int{0}, newest: true}}}
	var packed [][]messageUse
	msg := &spanMessages{uses: uses, refChunks: func(u []messageUse) ([][]byte, error) {
		packed = append(packed, u)
		return [][]byte{[]byte("c0"), []byte("c1")}, nil
	}}
	items, stats, err := w.prepare(t.Context(), []*tracepb.ResourceSpans{rs}, msg)
	if err != nil {
		t.Fatal(err)
	}
	row := items[0].row.(*otelv1pb.SPANS)
	if got := rowAttributes(row); !reflect.DeepEqual(got, []Attribute{{"resource", "service.name", "string", "agent"}, {"span", "gen_ai.operation.name", "string", "chat"}}) {
		t.Fatalf("indexed with messages: %+v", got)
	}
	if stats.NormalizedSpans != 1 || stats.UnindexedAttributes != 1 || items[0].references != 1 || len(packed) != 1 {
		t.Fatalf("stats %+v, item %+v, refChunks calls %d", stats, items[0], len(packed))
	}
	chunks := row.GetREF_CHUNKS().GetValues()
	if len(chunks) != 2 || chunks[0].GetCHUNK() != 0 || string(chunks[1].GetREFS()) != "c1" || chunks[1].GetCHUNK() != 1 {
		t.Fatalf("ref chunks %+v", chunks)
	}
	if items[0].cost != spanCost(row) {
		t.Fatal("the estimate leaves out the ref chunks")
	}
	// Without references (deltas) no chunk is packed.
	packed = nil
	if items, _, err := w.prepare(t.Context(), []*tracepb.ResourceSpans{rs}, &spanMessages{refChunks: msg.refChunks}); err != nil || items[0].row.(*otelv1pb.SPANS).REF_CHUNKS != nil || packed != nil {
		t.Fatalf("deltas rows: %v %v", err, packed)
	}
	boom := fail("admission", "message references", "too many")
	_, _, err = w.prepare(t.Context(), []*tracepb.ResourceSpans{rs}, &spanMessages{uses: uses, refChunks: func([]messageUse) ([][]byte, error) { return nil, boom }})
	if err != boom {
		t.Fatalf("refChunks error %v", err)
	}
}

// rowOTLP refuses an otlp column that is not the row's single span.
func TestRowOTLPCorruption(t *testing.T) {
	t.Parallel()
	rs := deltaSpan(1)
	sp := rs.ScopeSpans[0].Spans[0]
	build := func(otlp []byte) *otelv1pb.SPANS {
		row, err := otelsql.BuildCompiled(otelsql.Span{Resource: rs.Resource, Scope: rs.ScopeSpans[0].Scope, Span: sp, OTLP: otlp})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	wire := func(m proto.Message) []byte {
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := rowOTLP("test", build(wire(rs)).ProtoReflect(), sp.TraceId, sp.SpanId); err != nil {
		t.Fatalf("valid row: %v", err)
	}
	two := proto.Clone(rs).(*tracepb.ResourceSpans)
	two.ScopeSpans[0].Spans = append(two.ScopeSpans[0].Spans, sp)
	other := deltaSpan(2)
	for name, otlp := range map[string][]byte{
		"not protobuf":   {0xff, 0xff},
		"two spans":      wire(two),
		"no spans":       wire(&tracepb.ResourceSpans{}),
		"other identity": wire(other),
	} {
		_, err := rowOTLP("test", build(otlp).ProtoReflect(), sp.TraceId, sp.SpanId)
		requireTyped[*CorruptionError](t, name, err)
	}
}
