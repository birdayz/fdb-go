package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const deltaTurns = 25

func deltaStr(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func deltaKV(key string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: v}
}

func deltaKvlist(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
}

func deltaArray(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
}

// deltaText is distinct per (role, turn) and about n bytes of English-like text.
func deltaText(role string, turn, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s turn %d: ", role, turn)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "step %d inspects shard %d of trace %x and reports latency %dms; ", i, (turn*31+i*7)%97, turn*1009+i*13, (turn*17+i*3)%500)
	}
	return b.String()
}

type deltaMsg struct {
	role, text string
}

type deltaJSONPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type deltaJSONMsg struct {
	Role  string          `json:"role"`
	Parts []deltaJSONPart `json:"parts"`
}

type deltaJSONTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

var deltaTools = []deltaJSONTool{
	{"function", "query_traces", deltaText("tool-schema query_traces", 0, 700)},
	{"function", "fetch_logs", deltaText("tool-schema fetch_logs", 0, 700)},
}

var deltaSystem = deltaText("system", 0, 900)

// deltaMessages encodes one attribute's messages in either RFC-283 form.
func deltaMessages(form string, msgs []deltaMsg) *commonpb.AnyValue {
	if form == "json" {
		out := make([]deltaJSONMsg, len(msgs))
		for i, m := range msgs {
			out[i] = deltaJSONMsg{m.role, []deltaJSONPart{{"text", m.text}}}
		}
		wire, err := json.Marshal(out)
		if err != nil {
			panic(err)
		}
		return deltaStr(string(wire))
	}
	vs := make([]*commonpb.AnyValue, len(msgs))
	for i, m := range msgs {
		vs[i] = deltaKvlist(deltaKV("role", deltaStr(m.role)), deltaKV("parts", deltaArray(deltaKvlist(deltaKV("type", deltaStr("text")), deltaKV("content", deltaStr(m.text))))))
	}
	return deltaArray(vs...)
}

func deltaToolDefinitions(form string) *commonpb.AnyValue {
	if form == "json" {
		wire, err := json.Marshal(deltaTools)
		if err != nil {
			panic(err)
		}
		return deltaStr(string(wire))
	}
	vs := make([]*commonpb.AnyValue, len(deltaTools))
	for i, tool := range deltaTools {
		vs[i] = deltaKvlist(deltaKV("type", deltaStr(tool.Type)), deltaKV("name", deltaStr(tool.Name)), deltaKV("description", deltaStr(tool.Description)))
	}
	return deltaArray(vs...)
}

func deltaSpan(i int, attrs ...*commonpb.KeyValue) *tracepb.ResourceSpans {
	trace := bytes.Repeat([]byte{byte(i + 1)}, 16)
	id := bytes.Repeat([]byte{byte(i + 101)}, 8)
	return &tracepb.ResourceSpans{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{deltaKV("service.name", deltaStr("agent"))}},
		SchemaUrl: "https://opentelemetry.io/schemas/1.37.0",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "genai-sdk", Version: "1"},
			Spans: []*tracepb.Span{{TraceId: trace, SpanId: id, Name: "chat", Kind: tracepb.Span_SPAN_KIND_CLIENT, StartTimeUnixNano: uint64(1700000000000000000 + i), EndTimeUnixNano: uint64(1700000000000001000 + i), Attributes: attrs}},
		}},
	}
}

// deltaConversation repeats turn history plus a new user turn and one reply;
// each turn is its own trace, as in deltaSpan.
func deltaConversation(turns int, form string) []*tracepb.ResourceSpans {
	var history []deltaMsg
	out := make([]*tracepb.ResourceSpans, 0, turns)
	for k := 1; k <= turns; k++ {
		history = append(history, deltaMsg{"user", deltaText("user", k, 800)})
		reply := deltaMsg{"assistant", deltaText("assistant", k, 800)}
		sys := deltaArray(deltaKvlist(deltaKV("type", deltaStr("text")), deltaKV("content", deltaStr(deltaSystem))))
		if form == "json" {
			wire, _ := json.Marshal([]deltaJSONPart{{"text", deltaSystem}})
			sys = deltaStr(string(wire))
		}
		out = append(out, deltaSpan(
			k,
			deltaKV("gen_ai.operation.name", deltaStr("chat")),
			deltaKV("gen_ai.conversation.id", deltaStr("conv-283")),
			deltaKV("gen_ai.system_instructions", sys),
			deltaKV("gen_ai.tool.definitions", deltaToolDefinitions(form)),
			deltaKV("gen_ai.input.messages", deltaMessages(form, history)),
			deltaKV("gen_ai.output.messages", deltaMessages(form, []deltaMsg{reply})),
		))
		history = append(history, reply)
	}
	return out
}

// sameTrace puts every span of spans into the trace of the first.
func sameTrace(spans []*tracepb.ResourceSpans) []*tracepb.ResourceSpans {
	for _, rs := range spans {
		rs.ScopeSpans[0].Spans[0].TraceId = spans[0].ScopeSpans[0].Spans[0].TraceId
	}
	return spans
}

func cloneDeltaSpans(in []*tracepb.ResourceSpans) []*tracepb.ResourceSpans {
	out := make([]*tracepb.ResourceSpans, len(in))
	for i, rs := range in {
		if rs != nil {
			out[i] = proto.Clone(rs).(*tracepb.ResourceSpans)
		}
	}
	return out
}

func requireDeltaSpansEqual(t *testing.T, got, want []*tracepb.ResourceSpans) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d resource spans, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Fatalf("resource spans %d differ after reconstruction", i)
		}
	}
}

func deltaAttr(t *testing.T, rs *tracepb.ResourceSpans, key string) *commonpb.AnyValue {
	t.Helper()
	for _, kv := range rs.ScopeSpans[0].Spans[0].Attributes {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("attribute %s missing", key)
	return nil
}

func deltaManifest(t *testing.T, rs *tracepb.ResourceSpans, key string) *messageManifest {
	t.Helper()
	m, err := parseMessageManifest(deltaAttr(t, rs, key))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// decomposeInMemory decomposes spans and gives the elements IDs 1.1, 1.2, ...
// in first-reference order, as a writer storing them would.
func decomposeInMemory(t *testing.T, tenant int, spans []*tracepb.ResourceSpans) *messageDecomposition {
	t.Helper()
	d, err := decomposeMessages(t.Context(), spans, tenant)
	if err != nil {
		t.Fatal(err)
	}
	for i, el := range d.elements {
		el.id = messageID{1, int64(i + 1)}
	}
	if err := d.encodeManifests(false); err != nil {
		t.Fatal(err)
	}
	return d
}

// reconstructInMemory drives the reconstruct plan over the elements' raw bytes.
func reconstructInMemory(elements []*messageElement, spans []*tracepb.ResourceSpans) ([]*tracepb.ResourceSpans, error) {
	p, err := planReconstruct(context.Background(), spans)
	if err != nil {
		return nil, err
	}
	raws := map[messageID][]byte{}
	for _, el := range elements {
		raws[el.id] = el.raw
	}
	for _, id := range p.refs {
		if _, ok := raws[id]; !ok {
			return nil, fail("corruption", "test", "missing message")
		}
	}
	return p.apply(raws)
}

func deltaRandom(n int, seed byte) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{seed}).Read(b)
	return b
}

// deltaScribble overwrites every string and byte payload under v in place.
func deltaScribble(v *commonpb.AnyValue) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		x.StringValue = "scribbled"
	case *commonpb.AnyValue_BytesValue:
		for i := range x.BytesValue {
			x.BytesValue[i] = 0xee
		}
	case *commonpb.AnyValue_ArrayValue:
		for _, e := range x.ArrayValue.GetValues() {
			deltaScribble(e)
		}
	case *commonpb.AnyValue_KvlistValue:
		for _, kv := range x.KvlistValue.GetValues() {
			deltaScribble(kv.GetValue())
		}
	}
}

func deltaScribbleSpans(spans []*tracepb.ResourceSpans) {
	for _, rs := range spans {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				sp.Name = "scribbled"
				for _, kv := range sp.GetAttributes() {
					deltaScribble(kv.GetValue())
				}
				for _, ev := range sp.GetEvents() {
					for _, kv := range ev.GetAttributes() {
						deltaScribble(kv.GetValue())
					}
				}
			}
		}
	}
}

func deltaUnknown(m proto.Message) {
	m.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7))
}

type ftsRoleText struct{ role, text string }

func ftsAnyWire(t *testing.T, v *commonpb.AnyValue) []byte {
	t.Helper()
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// largeText is about n bytes of text with a vocabulary large enough that it
// needs several segments.
func largeText(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "w%d alpha%d beta, gamma.delta %d\n", i%5000, i%977, i)
	}
	return b.String()
}

// ftsBase is deltaSpan's start time origin: turn k starts at ftsBase+k.
const ftsBase = int64(1700000000000000000)

func ftsTrace(k int) string { return hex.EncodeToString(bytes.Repeat([]byte{byte(k + 1)}, 16)) }
func ftsSpan(k int) string  { return hex.EncodeToString(bytes.Repeat([]byte{byte(k + 101)}, 8)) }

func ftsMessageID(t *testing.T, s string) messageID {
	t.Helper()
	var id messageID
	if _, err := fmt.Sscanf(s, "%d.%d", &id.writer, &id.seq); err != nil || !id.valid() {
		t.Fatalf("message id %q: %v", s, err)
	}
	return id
}

// ftsTurns returns the deltaSpan turn of every hit, in hit order.
func ftsTurns(hits []MessageSpanHit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = int(h.Start - ftsBase)
	}
	return out
}
