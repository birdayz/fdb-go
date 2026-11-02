package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
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

// deltaConversation is an N-turn agent session: turn k's input repeats all
// prior user/assistant turns plus the new user turn; its output is one reply.
// Each turn is its own trace, as in deltaSpan.
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

func TestMessageDecomposeConversation(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"array", "json"} {
		input := deltaConversation(deltaTurns, form)
		before := cloneDeltaSpans(input)
		d := decomposeInMemory(t, 7, input)
		n := deltaTurns
		// Turn k references the system prompt, two tools, k inputs and 1 output.
		if s := d.stats; s.TotalMessages != int64(n*(n+1)+3*n) || s.UniqueMessages != int64(2*n+3) || s.ReusedMessages != s.TotalMessages-s.UniqueMessages || s.SavedBytes <= 0 {
			t.Fatalf("%s stats %+v", form, s)
		}
		requireDeltaSpansEqual(t, input, before)
		// Turn k's input lists turns 1..k in order: user, assistant, ..., user.
		m := deltaManifest(t, d.spans[n-1], "gen_ai.input.messages")
		if len(m.ids) != 2*n-1 {
			t.Fatalf("%s: last input lists %d messages", form, len(m.ids))
		}
		prev := deltaManifest(t, d.spans[n-2], "gen_ai.input.messages")
		if !reflect.DeepEqual(m.ids[:len(prev.ids)], prev.ids) {
			t.Fatalf("%s: turn %d input does not extend turn %d's", form, n, n-1)
		}
		uses := d.uses[spanKey(d.spans[n-1].ScopeSpans[0].Spans[0])]
		if len(uses) != 3+(2*n-1)+1 {
			t.Fatalf("%s: last span has %d uses", form, len(uses))
		}
		got, err := reconstructInMemory(d.elements, d.spans)
		if err != nil {
			t.Fatal(err)
		}
		requireDeltaSpansEqual(t, got, before)
	}
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

// TestMessageDecomposeInputImmutable proves decomposition and reconstruction
// neither modify their inputs nor share mutable memory with their outputs.
func TestMessageDecomposeInputImmutable(t *testing.T) {
	t.Parallel()
	input := append(deltaConversation(3, "array"), deltaConversation(3, "json")...)
	input = append(input, deltaSpan(
		9,
		deltaKV("gen_ai.output.messages", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(1000, 7)}}),
		deltaKV("gen_ai.input.messages", deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(300_000, 8)}})),
	), nil)
	input[0].ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.input.messages", deltaMessages("array", []deltaMsg{{"user", "event copy"}}))}}}
	before := cloneDeltaSpans(input)
	d := decomposeInMemory(t, 4, input)
	requireDeltaSpansEqual(t, input, before)
	for i := range input {
		if input[i] != nil && d.spans[i] == input[i] {
			t.Fatalf("output %d aliases its input", i)
		}
	}
	if d.spans[len(d.spans)-1] != nil {
		t.Fatal("nil input entry was not preserved")
	}
	outBefore := cloneDeltaSpans(d.spans)
	deltaScribbleSpans(input)
	if proto.Equal(input[len(input)-2], before[len(before)-2]) {
		t.Fatal("scribble fixture did not modify the input")
	}
	got, err := reconstructInMemory(d.elements, d.spans)
	if err != nil {
		t.Fatal(err)
	}
	requireDeltaSpansEqual(t, got, before)
	requireDeltaSpansEqual(t, d.spans, outBefore)
	deltaScribbleSpans(got)
	requireDeltaSpansEqual(t, d.spans, outBefore)
	again, err := reconstructInMemory(d.elements, d.spans)
	if err != nil {
		t.Fatal(err)
	}
	requireDeltaSpansEqual(t, again, before)
}

func deltaUnknown(m proto.Message) {
	m.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7))
}

func TestMessageManifestLayouts(t *testing.T) {
	t.Parallel()
	unknownArray := deltaArray(deltaStr("a"), deltaStr("b"))
	deltaUnknown(unknownArray.GetArrayValue())
	unknownShell := deltaArray(deltaStr("a"))
	deltaUnknown(unknownShell)
	unknownJSON := deltaStr(`[{"role":"user"}]`)
	deltaUnknown(unknownJSON)
	unknownElement := deltaStr("x")
	deltaUnknown(unknownElement)
	for _, c := range []struct {
		name   string
		value  *commonpb.AnyValue
		layout byte
		count  int
	}{
		{"nil value", nil, messageLayoutNone, 0},
		{"empty any", &commonpb.AnyValue{}, messageLayoutValue, 1},
		{"plain string", deltaStr("not json at all"), messageLayoutValue, 1},
		{"json object", deltaStr(`{"role":"user"}`), messageLayoutValue, 1},
		{"json compact", deltaStr(`[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]`), messageLayoutJSON, 2},
		{"json pretty", deltaStr("  [\n  {\"role\": \"user\"} ,\n\t{\"role\": \"tool\"}\n]\n "), messageLayoutJSON, 2},
		{"json scalars", deltaStr(`[1,"two",null,true,[3]]`), messageLayoutJSON, 5},
		{"json empty array", deltaStr(`[]`), messageLayoutValue, 1},
		{"json truncated", deltaStr(`[{"role":"user"},`), messageLayoutValue, 1},
		{"json trailing value", deltaStr(`[1] [2]`), messageLayoutValue, 1},
		{"json invalid utf8 escape", deltaStr(`["\ud800"]`), messageLayoutJSON, 1},
		{"int", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}, messageLayoutValue, 1},
		{"bytes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("[1,2]")}}, messageLayoutValue, 1},
		{"kvlist", deltaKvlist(deltaKV("role", deltaStr("user"))), messageLayoutValue, 1},
		{"empty array", deltaArray(), messageLayoutArray, 0},
		{"nil array", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{}}, messageLayoutArray, 0},
		{"array mixed", deltaArray(deltaStr("s"), &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0, 1}}}, deltaKvlist(), deltaArray(deltaStr("nested")), unknownElement), messageLayoutArray, 5},
		{"array unknown fields", unknownArray, messageLayoutValue, 1},
		{"shell unknown fields", unknownShell, messageLayoutValue, 1},
		{"json unknown fields", unknownJSON, messageLayoutValue, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rs := deltaSpan(1, deltaKV("gen_ai.input.messages", c.value), deltaKV("other", deltaStr(`[{"role":"user"}]`)))
			rs.ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "gen_ai.client.inference.operation.details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.output.messages", proto.Clone(rs.ScopeSpans[0].Spans[0].Attributes[0]).(*commonpb.KeyValue).Value)}}}
			in := []*tracepb.ResourceSpans{rs, nil}
			before := cloneDeltaSpans(in)
			d := decomposeInMemory(t, 3, in)
			if d.spans[1] != nil || d.spans[0] == rs {
				t.Fatal("decompose must clone non-nil inputs and keep nil entries")
			}
			if !proto.Equal(deltaAttr(t, d.spans[0], "other"), deltaStr(`[{"role":"user"}]`)) {
				t.Fatal("non-content attribute changed")
			}
			m := deltaManifest(t, d.spans[0], "gen_ai.input.messages")
			ev, err := parseMessageManifest(d.spans[0].ScopeSpans[0].Spans[0].Events[0].Attributes[0].Value)
			if err != nil {
				t.Fatal(err)
			}
			if m.layout != c.layout || len(m.ids) != c.count || ev.layout != c.layout {
				t.Fatalf("layout %d/%d (event %d) want %d/%d", m.layout, len(m.ids), ev.layout, c.layout, c.count)
			}
			if d.stats.TotalMessages != int64(2*c.count) || int(d.stats.UniqueMessages) != len(d.elements) {
				t.Fatalf("stats %+v for %d elements per attribute", d.stats, c.count)
			}
			got, err := reconstructInMemory(d.elements, d.spans)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, got, in)
			requireDeltaSpansEqual(t, in, before)
		})
	}
}

func TestMessageDecomposeAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	huge := deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: make([]byte, maxMessageBytes)}})
	_, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", huge))}, 0)
	requireTyped[*AdmissionError](t, "oversized element", err)

	// Placeholder IDs are 18 bytes, the longest an ID encodes to, so a
	// manifest admitted with them stays admitted with real IDs.
	if n := len(binaryUvarints(messagePlaceholderID.writer, messagePlaceholderID.seq)); n != 18 {
		t.Fatalf("placeholder ID encodes to %d bytes", n)
	}
	many := make([]*commonpb.AnyValue, maxMessageManifest/18+1)
	for i := range many {
		many[i] = deltaStr(fmt.Sprint(i))
	}
	_, err = decomposeMessages(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(many...)))}, 0)
	requireTyped[*AdmissionError](t, "oversized manifest", err)
	if _, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(many[:3000]...)))}, 0); err != nil {
		t.Fatalf("3000-message attribute must fit the manifest cap: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := decomposeMessages(canceledCtx, deltaConversation(1, "array"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	d, err := decomposeMessages(ctx, deltaConversation(1, "array"), 0)
	if err != nil {
		t.Fatal(err)
	}
	err = d.encodeManifests(false)
	requireTyped[*OperationError](t, "manifest of unstored elements", err)
}

func binaryUvarints(vs ...int64) []byte {
	var out []byte
	for _, v := range vs {
		out = protowire.AppendVarint(out, uint64(v))
	}
	return out
}

func TestMessageManifestCorruption(t *testing.T) {
	t.Parallel()
	d := decomposeInMemory(t, 0, deltaConversation(2, "json"))
	good := deltaAttr(t, d.spans[1], "gen_ai.input.messages").GetBytesValue()
	if _, err := parseMessageManifest(deltaAttr(t, d.spans[1], "gen_ai.input.messages")); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	mutate := func(f func([]byte) []byte) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: f(append([]byte(nil), good...))}}
	}
	bytesOf := func(m *messageManifest) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: m.encode()}}
	}
	for _, c := range []struct {
		name  string
		value *commonpb.AnyValue
	}{
		{"original string", deltaStr(`[{"role":"user"}]`)},
		{"nil value", nil},
		{"bad magic", mutate(func(b []byte) []byte { b[0] = 'X'; return b })},
		{"bad version", mutate(func(b []byte) []byte { b[4] = 1; return b })},
		{"bad layout", mutate(func(b []byte) []byte { b[5] = 9; return b })},
		{"truncated", mutate(func(b []byte) []byte { return b[:len(b)-1] })},
		{"trailing", mutate(func(b []byte) []byte { return append(b, 0) })},
		{"bad separator flag", mutate(func(b []byte) []byte { b[len(b)-1] = 7; return b })},
		{"zero writer", bytesOf(&messageManifest{layout: messageLayoutValue, ids: []messageID{{0, 1}}})},
		{"zero seq", bytesOf(&messageManifest{layout: messageLayoutValue, ids: []messageID{{1, 0}}})},
		{"value layout with two elements", bytesOf(&messageManifest{layout: messageLayoutValue, ids: []messageID{{1, 1}, {1, 2}}})},
		{"json layout without elements", bytesOf(&messageManifest{layout: messageLayoutJSON})},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseMessageManifest(c.value)
			requireTyped[*CorruptionError](t, c.name, err)
			bad := cloneDeltaSpans(d.spans)
			for _, kv := range bad[1].ScopeSpans[0].Spans[0].Attributes {
				if kv.Key == "gen_ai.input.messages" {
					kv.Value = c.value
				}
			}
			_, err = reconstructInMemory(d.elements, bad)
			requireTyped[*CorruptionError](t, c.name, err)
		})
	}
	// One message cannot be referenced under two element encodings.
	mixed := cloneDeltaSpans(d.spans)
	id := deltaManifest(t, d.spans[0], "gen_ai.output.messages").ids[0]
	mixed[0].ScopeSpans[0].Spans[0].Attributes = append(mixed[0].ScopeSpans[0].Spans[0].Attributes, deltaKV("gen_ai.output.messages", bytesOf(&messageManifest{layout: messageLayoutArray, ids: []messageID{id}})))
	_, err := reconstructInMemory(d.elements, mixed)
	requireTyped[*CorruptionError](t, "mixed encodings", err)
}

// ---------------------------------------------------------------------------
// Text extraction, tokenizer and segments
// ---------------------------------------------------------------------------

type ftsRoleText struct{ role, text string }

func ftsAnyWire(t *testing.T, v *commonpb.AnyValue) []byte {
	t.Helper()
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestMessageTextExtractionConversation(t *testing.T) {
	t.Parallel()
	const turns = 3
	want := map[ftsRoleText]bool{{"", deltaSystem}: true}
	for _, tool := range deltaTools {
		want[ftsRoleText{"", tool.Name + "\n" + tool.Description}] = true
	}
	for k := 1; k <= turns; k++ {
		want[ftsRoleText{"user", deltaText("user", k, 800)}] = true
		want[ftsRoleText{"assistant", deltaText("assistant", k, 800)}] = true
	}
	for _, form := range []string{"array", "json"} {
		d, err := decomposeMessages(t.Context(), deltaConversation(turns, form), 0)
		if err != nil {
			t.Fatal(err)
		}
		got := map[ftsRoleText]bool{}
		for _, el := range d.elements {
			role, text, err := extractMessageText(el.encoding, el.raw)
			if err != nil {
				t.Fatal(err)
			}
			got[ftsRoleText{role, text}] = true
		}
		if len(d.elements) != 2*turns+3 || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: extracted %d elements\n got %v\nwant %v", form, len(d.elements), got, want)
		}
	}
}

func TestMessageTextExtractionEncodingsAgree(t *testing.T) {
	t.Parallel()
	nested := deltaKvlist(
		deltaKV("z", deltaStr("first")),
		deltaKV("role", deltaStr("  User ")),
		deltaKV("parts", deltaArray(deltaStr("second"), deltaKvlist(deltaKV("type", deltaStr("tool_call")), deltaKV("name", deltaStr("third")), deltaKV("role", deltaStr("inner"))))),
		deltaKV("n", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 5}}),
		deltaKV("a", deltaStr("fourth")),
	)
	for _, c := range []struct {
		name       string
		any        *commonpb.AnyValue
		json       string
		role, text string
	}{
		{"plain string", deltaStr("hello"), `"hello"`, "", "hello"},
		{"document order", nested, `{"z":"first","role":"  User ","parts":["second",{"type":"tool_call","name":"third","role":"inner"}],"n":5,"a":"fourth"}`, "user", "first\nsecond\nthird\nfourth"},
		{"nested role is not the role", deltaKvlist(deltaKV("parts", deltaArray(deltaKvlist(deltaKV("role", deltaStr("tool")), deltaKV("content", deltaStr("c")))))), `{"parts":[{"role":"tool","content":"c"}]}`, "", "c"},
		{"non-string role", deltaKvlist(deltaKV("role", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}), deltaKV("content", deltaStr("x"))), `{"role":7,"content":"x"}`, "", "x"},
		{"no text", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}, `true`, "", ""},
	} {
		role, text, err := extractMessageText(messageEncodingAny, ftsAnyWire(t, c.any))
		if err != nil || role != c.role || text != c.text {
			t.Fatalf("%s otlp_any: (%q, %q, %v) want (%q, %q)", c.name, role, text, err, c.role, c.text)
		}
		for range 20 {
			role, text, err := extractMessageText(messageEncodingJSON, []byte(c.json))
			if err != nil || role != c.role || text != c.text {
				t.Fatalf("%s json: (%q, %q, %v) want (%q, %q)", c.name, role, text, err, c.role, c.text)
			}
		}
	}
	for name, c := range map[string]struct {
		encoding string
		raw      []byte
	}{
		"truncated json":   {messageEncodingJSON, []byte(`{"a":`)},
		"trailing json":    {messageEncodingJSON, []byte(`"a" "b"`)},
		"garbage proto":    {messageEncodingAny, []byte{0xff, 0xff, 0xff}},
		"unknown encoding": {"xml", []byte("<a/>")},
	} {
		_, _, err := extractMessageText(c.encoding, c.raw)
		requireTyped[*CorruptionError](t, name, err)
	}
}

// TestMessageTokenizer pins the registered tokenizer to the default one,
// except that a token over maxIndexedTokenBytes is "" and keeps its position.
func TestMessageTokenizer(t *testing.T) {
	t.Parallel()
	tok, err := recordlayer.GetTextTokenizer(messageTokenizerName)
	if err != nil {
		t.Fatal(err)
	}
	text := "Crème BRÛLÉE, crème! Ångström " + strings.Repeat("a", maxIndexedTokenBytes+1) + " ok " + strings.Repeat("b", maxIndexedTokenBytes)
	got, err := tok.TokenizeToMap(text, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int{"creme": {0, 2}, "brulee": {1}, "angstrom": {3}, "ok": {5}, strings.Repeat("b", maxIndexedTokenBytes): {6}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens %v", got)
	}
	def, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tok.TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil || len(list) != len(def) || list[4] != "" || def[4] == "" {
		t.Fatalf("list %d vs default %d: %v", len(list), len(def), err)
	}
	terms, err := tokenizeQueryTerms("Ünïcödé  FAÇADE! query_traces")
	if err != nil || !reflect.DeepEqual(terms, []messagePhraseTerm{{"unicode", 0}, {"facade", 1}, {"query_traces", 2}}) {
		t.Fatalf("query terms %v %v", terms, err)
	}
	_, err = tokenizeQueryTerms(strings.Repeat("b", maxIndexedTokenBytes+1))
	requireTyped[*ConfigError](t, "oversized query term", err)
	_, err = tokenizeQueryTerms(strings.Repeat("c ", maxMessageQueryBytes))
	requireTyped[*ConfigError](t, "oversized query", err)
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

// TestMessageSegments checks the bounds of every segment, that each
// tokenizes to its tokens of the whole text, and that every window of
// maxMessageQueryTerms consecutive tokens lies inside one segment.
func TestMessageSegments(t *testing.T) {
	t.Parallel()
	small := deltaText("user", 1, 800)
	segs, unindexed, err := segmentMessageText(small, messageSegmentTokens)
	if err != nil || len(segs) != 1 || segs[0].text != small || segs[0].offset != 0 || unindexed != 0 {
		t.Fatalf("small text: %d segments, %v", len(segs), err)
	}
	tok := messageTokenizer{}
	// A segment's postings are the TEXT index entries it adds.
	if m, err := tok.TokenizeToMap(small, 0, recordlayer.TokenizerModeIndex); err != nil || segs[0].postings != len(m) {
		t.Fatalf("small text: %d postings, want %d (%v)", segs[0].postings, len(m), err)
	}
	for _, c := range []struct {
		name     string
		text     string
		distinct int
	}{
		{"1MB", largeText(1 << 20), messageSegmentTokens},
		{"bytes bound", strings.Repeat("same words again ", 10000), messageSegmentTokens},
		{"minimum distinct", largeText(64 << 10), messageSegmentMinTokens},
		{"long tokens", strings.Repeat("x"+strings.Repeat("y", maxIndexedTokenBytes)+" z ", 300), messageSegmentTokens},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			whole, err := tok.TokenizeToList(c.text, 0, recordlayer.TokenizerModeIndex)
			if err != nil {
				t.Fatal(err)
			}
			segs, unindexed, err := segmentMessageText(c.text, c.distinct)
			if err != nil {
				t.Fatal(err)
			}
			if len(segs) < 2 {
				t.Fatalf("%d segments", len(segs))
			}
			blank := 0
			for _, w := range whole {
				if w == "" {
					blank++
				}
			}
			if unindexed != blank {
				t.Fatalf("unindexed %d, want %d", unindexed, blank)
			}
			end := 0
			for i, s := range segs {
				got, err := tok.TokenizeToList(s.text, 0, recordlayer.TokenizerModeIndex)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, whole[s.offset:s.offset+len(got)]) {
					t.Fatalf("segment %d does not tokenize to its tokens", i)
				}
				distinct := map[string]bool{}
				for _, w := range got {
					if w != "" {
						distinct[w] = true
					}
				}
				if len(distinct) > c.distinct || len(s.text) > messageSegmentBytes {
					t.Fatalf("segment %d: %d distinct tokens, %d bytes", i, len(distinct), len(s.text))
				}
				if m, err := tok.TokenizeToMap(s.text, 0, recordlayer.TokenizerModeIndex); err != nil || s.postings != len(m) || s.postings != len(distinct) {
					t.Fatalf("segment %d: %d postings, want %d (%v)", i, s.postings, len(m), err)
				}
				if i > 0 && end-s.offset != messageSegmentOverlap {
					t.Fatalf("segments %d and %d overlap by %d tokens", i-1, i, end-s.offset)
				}
				end = s.offset + len(got)
			}
			if segs[0].offset != 0 || end != len(whole) {
				t.Fatalf("segments cover tokens [0, %d) of %d", end, len(whole))
			}
		})
	}
}

// TestPlanMessageParts checks the records of a message larger than one part.
func TestPlanMessageParts(t *testing.T) {
	t.Parallel()
	text := largeText(3 << 20)
	value := deltaKvlist(deltaKV("role", deltaStr("Tool")), deltaKV("content", deltaStr(text)))
	raw := ftsAnyWire(t, value)
	el := &messageElement{digest: messageHash(2, messageEncodingAny, raw), encoding: messageEncodingAny, raw: raw}
	p, err := planMessage(2, el, true, messageSegmentTokens)
	if err != nil {
		t.Fatal(err)
	}
	segs, _, err := segmentMessageText(text, messageSegmentTokens)
	if err != nil {
		t.Fatal(err)
	}
	if want := max(len(segs), (len(raw)+messagePartRawBytes-1)/messagePartRawBytes); len(p.records) != want || len(p.records) < 2 {
		t.Fatalf("%d records, want %d", len(p.records), want)
	}
	var rebuilt []byte
	for i, r := range p.records {
		m := r.ProtoReflect()
		f := m.Descriptor().Fields()
		if m.Get(f.ByName("part")).Int() != int64(i) || m.Get(f.ByName("role")).String() != "tool" || m.Get(f.ByName("text")).String() != segs[i].text || m.Get(f.ByName("token_offset")).Int() != int64(segs[i].offset) {
			t.Fatalf("part %d fields", i)
		}
		if m.Has(f.ByName("hash")) != (i == 0) || m.Has(f.ByName("parts")) != (i == 0) || (m.Get(f.ByName("large")).List().Len() == 1) != (i == 0) {
			t.Fatalf("part %d header fields", i)
		}
		rebuilt = append(rebuilt, m.Get(f.ByName("raw")).Bytes()...)
	}
	if !bytes.Equal(rebuilt, raw) {
		t.Fatal("parts do not hold the raw element")
	}
	small, err := planMessage(2, &messageElement{encoding: messageEncodingAny, raw: ftsAnyWire(t, deltaStr("hi"))}, true, messageSegmentTokens)
	if err != nil || len(small.records) != 1 || small.records[0].ProtoReflect().Get(small.records[0].ProtoReflect().Descriptor().Fields().ByName("large")).List().Len() != 0 {
		t.Fatalf("small message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Span references
// ---------------------------------------------------------------------------

func TestMessageUses(t *testing.T) {
	t.Parallel()
	x, y, z := deltaMsg{"user", "x"}, deltaMsg{"assistant", "y"}, deltaMsg{"user", "z"}
	spans := []*tracepb.ResourceSpans{deltaSpan(
		1,
		deltaKV("gen_ai.input.messages", deltaMessages("array", []deltaMsg{x, y, x})),
		deltaKV("gen_ai.output.messages", deltaMessages("array", []deltaMsg{y})),
		// A later value of the same key does not describe references.
		deltaKV("gen_ai.input.messages", deltaMessages("array", []deltaMsg{z})),
	)}
	d, err := decomposeMessages(t.Context(), spans, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.elements) != 3 {
		t.Fatalf("%d elements, want 3", len(d.elements))
	}
	ex, ey := d.elements[0], d.elements[1]
	in, out := messageAttributeCode("gen_ai.input.messages"), messageAttributeCode("gen_ai.output.messages")
	got := d.uses[spanKey(d.spans[0].ScopeSpans[0].Spans[0])]
	want := []messageUse{
		{element: ex, attribute: in, ordinals: []int{0, 2}, newest: true},
		{element: ey, attribute: in, ordinals: []int{1}},
		{element: ey, attribute: out, ordinals: []int{0}, newest: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uses %+v, want %+v", got, want)
	}

	// Turn k of a conversation references its whole history; only its own
	// prompt and reply are newest. The uses depend only on the span.
	conv := deltaConversation(4, "array")
	d, err = decomposeMessages(t.Context(), conv, 0)
	if err != nil {
		t.Fatal(err)
	}
	alone, err := decomposeMessages(t.Context(), conv[2:3], 0)
	if err != nil {
		t.Fatal(err)
	}
	key := spanKey(d.spans[2].ScopeSpans[0].Spans[0])
	var newest []int64
	for _, u := range d.uses[key] {
		if u.newest {
			newest = append(newest, u.attribute)
		}
	}
	if len(d.uses[key]) != 5+1+1+2 || len(alone.uses[key]) != len(d.uses[key]) || !slices.Equal(newest, []int64{3, 4, in, out}) {
		t.Fatalf("turn 3 uses %+v (alone %+v)", d.uses[key], alone.uses[key])
	}
	for i, u := range d.uses[key] {
		a := alone.uses[key][i]
		if u.element.digest != a.element.digest || u.attribute != a.attribute || u.newest != a.newest || !slices.Equal(u.ordinals, a.ordinals) {
			t.Fatalf("use %d differs alone: %+v vs %+v", i, u, a)
		}
	}

	// The Envelope lists them with the element IDs, or the placeholder.
	fd, err := recordFile(messagesFullText)
	if err != nil {
		t.Fatal(err)
	}
	m := dynamicpb.NewMessage(fd.Messages().ByName("Envelope"))
	ex.id = messageID{3, 7}
	setEnvelopeMessages(m, want)
	list := m.Get(m.Descriptor().Fields().ByName("messages")).List()
	if list.Len() != 3 {
		t.Fatalf("%d references", list.Len())
	}
	ref := func(i int) (int64, int64, bool, int64, []int64) {
		r := list.Get(i).Message()
		f := r.Descriptor().Fields()
		ords := r.Get(f.ByName("ordinals")).List()
		var o []int64
		for j := range ords.Len() {
			o = append(o, ords.Get(j).Int())
		}
		return r.Get(f.ByName("writer")).Int(), r.Get(f.ByName("seq")).Int(), r.Get(f.ByName("newest")).Bool(), r.Get(f.ByName("attribute")).Int(), o
	}
	if w, s, n, a, o := ref(0); w != 3 || s != 7 || !n || a != in || !slices.Equal(o, []int64{0, 2}) {
		t.Fatalf("reference 0: %d %d %v %d %v", w, s, n, a, o)
	}
	if w, s, n, a, _ := ref(1); w != messagePlaceholderID.writer || s != messagePlaceholderID.seq || n || a != in {
		t.Fatalf("reference 1: %d %d %v %d", w, s, n, a)
	}
	// Setting again replaces the list.
	setEnvelopeMessages(m, want[:1])
	if list := m.Get(m.Descriptor().Fields().ByName("messages")).List(); list.Len() != 1 {
		t.Fatalf("%d references after replacement", list.Len())
	}
}

// ---------------------------------------------------------------------------
// Query planning and continuations
// ---------------------------------------------------------------------------

func TestMessageQueryPlan(t *testing.T) {
	t.Parallel()
	const op = "test"
	plan := func(q MessageFullTextQuery) *messageQueryPlan {
		t.Helper()
		p, err := newMessageQueryPlan(op, q, 7)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := plan(MessageFullTextQuery{Query: "New York, new YORK!", Mode: MessageMatchPhrase})
	if !reflect.DeepEqual(p.terms, []string{"new", "york"}) || !reflect.DeepEqual(p.phrase, []messagePhraseTerm{{"new", 0}, {"york", 1}, {"new", 2}, {"york", 3}}) || p.limit != 7 {
		t.Fatalf("phrase plan %+v", p)
	}
	if p := plan(MessageFullTextQuery{Query: "x", AttributeKey: "gen_ai.output.messages"}); p.mode != MessageMatchAll || p.attribute != 2 {
		t.Fatalf("default mode %q, attribute %d", p.mode, p.attribute)
	}
	base := plan(MessageFullTextQuery{Query: "alpha beta"}).digest
	for _, q := range []MessageFullTextQuery{{Query: "BETA alpha"}, {Query: "alpha beta", Mode: MessageMatchAll, Limit: 3}} {
		if !bytes.Equal(plan(q).digest, base) {
			t.Fatalf("digest must ignore term order and limit: %+v", q)
		}
	}
	if !bytes.Equal(plan(MessageFullTextQuery{Query: "a", Role: " USER"}).digest, plan(MessageFullTextQuery{Query: "a", Role: "user"}).digest) {
		t.Fatal("digest must normalize role")
	}
	pred := Attribute{"span", "k", "string", "v"}
	for name, q := range map[string]MessageFullTextQuery{
		"phrase":     {Query: "alpha beta", Mode: MessageMatchPhrase},
		"any":        {Query: "alpha beta", Mode: MessageMatchAny},
		"terms":      {Query: "alpha gamma"},
		"attribute":  {Query: "alpha beta", AttributeKey: "gen_ai.input.messages"},
		"role":       {Query: "alpha beta", Role: "user"},
		"first turn": {Query: "alpha beta", FirstTurnOnly: true},
		"predicate":  {Query: "alpha beta", Predicates: []Attribute{pred}},
		"window":     {Query: "alpha beta", TimeWindow: &TimeRange{Start: 1, End: 2}},
	} {
		if bytes.Equal(plan(q).digest, base) {
			t.Fatalf("digest ignores %s", name)
		}
	}
	if bytes.Equal(plan(MessageFullTextQuery{Query: "beta alpha", Mode: MessageMatchPhrase}).digest, plan(MessageFullTextQuery{Query: "alpha beta", Mode: MessageMatchPhrase}).digest) {
		t.Fatal("phrase digest must bind term order")
	}
	for name, q := range map[string]MessageFullTextQuery{
		"empty":                 {},
		"punctuation":           {Query: " ?! "},
		"bad mode":              {Query: "a", Mode: "fuzzy"},
		"too many terms":        {Query: strings.Repeat("w ", maxMessageQueryTerms+1)},
		"bad attribute":         {Query: "a", AttributeKey: "gen_ai.request.model"},
		"negative limit":        {Query: "a", Limit: -1},
		"bad predicate":         {Query: "a", Predicates: []Attribute{{"event", "k", "string", "v"}}},
		"message predicate":     {Query: "a", Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "v"}}},
		"inverted window":       {Query: "a", TimeWindow: &TimeRange{Start: 3, End: 2}},
		"long role":             {Query: "a", Role: strings.Repeat("r", maxMessageRoleBytes+1)},
		"unindexable long term": {Query: strings.Repeat("q", maxIndexedTokenBytes+1)},
	} {
		_, err := newMessageQueryPlan(op, q, 7)
		requireTyped[*ConfigError](t, name, err)
	}
}

func TestMessageContinuation(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{3}, continuationKeyBytes)
	b := qaBackend("time")
	digest := bytes.Repeat([]byte{1}, 32)
	last := messageRefKey("time", qaID(1, 16), qaID(2, 8), 42, 1, messageID{3, 9})
	token := signContinuationKind(secret, messageContinuationKind, b.cfg.Run, b.cfg.Tenant, "time", digest, last)
	c, err := parseContinuationKind(token, messageContinuationKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.authenticate(secret, b, digest); err != nil {
		t.Fatal(err)
	}
	if r, err := parseMessageResume("time", c.last); err != nil || !bytes.Equal(r, last) {
		t.Fatalf("resume %x %v", r, err)
	}
	// Sort keys order by span, then attribute, then message.
	keys := [][]byte{
		messageRefKey("time", qaID(1, 16), qaID(2, 8), 43, 2, messageID{1, 1}),
		messageRefKey("time", qaID(1, 16), qaID(2, 8), 42, 1, messageID{3, 9}),
		messageRefKey("time", qaID(1, 16), qaID(2, 8), 42, 1, messageID{3, 10}),
		messageRefKey("time", qaID(1, 16), qaID(2, 8), 42, 2, messageID{1, 1}),
	}
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Fatalf("sort key %d does not follow %d", i, i-1)
		}
	}
	_, err = parseContinuationKind(token, continuationKind)
	requireTyped[*ConfigError](t, "message token as same-span", err)
	_, err = parseContinuationKind(signContinuation(secret, b.cfg.Run, b.cfg.Tenant, "time", digest, last), messageContinuationKind)
	requireTyped[*ConfigError](t, "same-span token as message", err)
	for i := range token {
		mutated := append([]byte(nil), token...)
		mutated[i] ^= 0x10
		c, err := parseContinuationKind(mutated, messageContinuationKind)
		if err == nil {
			err = c.authenticate(secret, b, digest)
		}
		if err == nil {
			t.Fatalf("flip at byte %d accepted", i)
		}
		requireConfigOrCorruption(t, fmt.Sprintf("flip %d", i), err)
	}
	for name, raw := range map[string][]byte{
		"trace order key": messageRefKey("trace", qaID(1, 16), qaID(2, 8), 42, 1, messageID{3, 9}),
		"zero seq":        messageRefKey("time", qaID(1, 16), qaID(2, 8), 42, 1, messageID{3, 0}),
		"garbage":         {0xff},
		"bad attribute":   append(postingSuffix(Span{Trace: qaID(1, 16), ID: qaID(2, 8), Start: 42}, "time"), "gen_ai.request.model", int64(1), int64(1)).Pack(),
		"short":           tuple.Tuple{int64(1)}.Pack(),
	} {
		_, err := parseMessageResume("time", raw)
		requireTyped[*CorruptionError](t, name, err)
	}
}
