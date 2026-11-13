package otel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestMessageQueueClearsConsumedAndSkippedSlots(t *testing.T) {
	t.Parallel()
	failed := &messageSubmission{left: 1, err: errors.New("failed"), done: make(chan struct{})}
	live := &messageSubmission{left: 2, done: make(chan struct{})}
	first, tail := &messagePlan{raw: messagePartRawBytes}, &messagePlan{raw: 1}
	backing := []queuedMessage{{plan: &messagePlan{}, sub: failed}, {plan: first, sub: live}, {plan: tail, sub: live}}
	q := messageQueue{pending: backing, loops: 1}
	got := q.take()
	if len(got) != 1 || got[0].plan != first || len(q.pending) != 1 || q.pending[0].plan != tail || failed.left != 0 {
		t.Fatalf("partial/failure drain: %+v / %+v", got, &q)
	}
	for i := range 2 {
		if backing[i].plan != nil || backing[i].sub != nil {
			t.Fatalf("consumed slot%d retains plan/submission", i)
		}
	}
	if backing[2].plan != tail {
		t.Fatal("partial drain discarded live tail")
	}
	got = q.take()
	if len(got) != 1 || got[0].plan != tail || q.pending != nil || backing[2].plan != nil || backing[2].sub != nil {
		t.Fatal("exhausted queue retains tail backing")
	}
	if got := q.take(); len(got) != 0 || q.loops != 0 {
		t.Fatalf("loop ownership: %+v / %d", got, q.loops)
	}
}

func cacheFixtureMarker(i int) traceMarker {
	return traceMarker{id: messageID{writer: 1, seq: int64(i + 1)}, trace: "memory-only-cache-fixture"}
}

func TestMarkerCacheCapacityEvictionAndRepeatedWrap(t *testing.T) {
	t.Parallel()
	var cache markerCache
	if cache.has(cacheFixtureMarker(0)) {
		t.Fatal("empty cache hit")
	}
	inserted := 0
	check := func() {
		t.Helper()
		want := min(inserted, markerCacheSize)
		if len(cache.set) != want || len(cache.ring) != want || cap(cache.ring) != markerCacheSize {
			t.Fatalf("unbounded cache: set=%d ring=%d/%d want%d", len(cache.set), len(cache.ring), cap(cache.ring), want)
		}
		start := max(inserted-markerCacheSize, 0)
		for i := start; i < inserted; i++ {
			key := cacheFixtureMarker(i)
			if !cache.has(key) || cache.ring[i%markerCacheSize] != key {
				t.Fatalf("retained marker/ring mismatch at %d", i)
			}
		}
		for key := range cache.set {
			i := int(key.id.seq) - 1
			if i < start || i >= inserted || key != cacheFixtureMarker(i) {
				t.Fatalf("unexpected retained marker: %+v", key)
			}
		}
		if start > 0 && cache.has(cacheFixtureMarker(start-1)) {
			t.Fatalf("oldest evicted marker retained: %d", start-1)
		}
		if cache.next != start%markerCacheSize {
			t.Fatalf("eviction cursor=%d want%d", cache.next, start%markerCacheSize)
		}
	}
	for _, target := range []int{1, markerCacheSize - 1, markerCacheSize, markerCacheSize + 1, 2*markerCacheSize + 17, 3*markerCacheSize + 31} {
		for inserted < target {
			cache.add([]traceMarker{cacheFixtureMarker(inserted)})
			inserted++
		}
		check()
		beforeNext, beforeLen := cache.next, len(cache.set)
		cache.add([]traceMarker{cacheFixtureMarker(inserted - 1), cacheFixtureMarker(inserted - 1), cacheFixtureMarker(max(inserted-markerCacheSize, 0))})
		if cache.next != beforeNext || len(cache.set) != beforeLen {
			t.Fatal("duplicates consumed eviction slots")
		}
		check()
	}
}

func TestMessageDecomposeConversation(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"array", "json"} {
		input := deltaConversation(deltaTurns, form)
		before := cloneDeltaSpans(input)
		d := decomposeInMemory(t, input)
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
	d := decomposeInMemory(t, input)
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
			d := decomposeInMemory(t, in)
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
	_, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", huge))})
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
	_, err = decomposeMessages(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(many...)))})
	requireTyped[*AdmissionError](t, "oversized manifest", err)
	if _, err := decomposeMessages(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(many[:3000]...)))}); err != nil {
		t.Fatalf("3000-message attribute must fit the manifest cap: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := decomposeMessages(canceledCtx, deltaConversation(1, "array")); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	d, err := decomposeMessages(ctx, deltaConversation(1, "array"))
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
	d := decomposeInMemory(t, deltaConversation(2, "json"))
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
		d, err := decomposeMessages(t.Context(), deltaConversation(turns, form))
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

// Query terms are the default tokenizer's query tokens, bounded like
// indexed tokens; otelsql's TestMessageTokenizer covers index tokens.
func TestTokenizeQueryTerms(t *testing.T) {
	t.Parallel()
	terms, err := tokenizeQueryTerms("Ünïcödé  FAÇADE! query_traces")
	if err != nil || !reflect.DeepEqual(terms, []messagePhraseTerm{{"unicode", 0}, {"facade", 1}, {"query_traces", 2}}) {
		t.Fatalf("query terms %v %v", terms, err)
	}
	_, err = tokenizeQueryTerms(strings.Repeat("b", maxIndexedTokenBytes+1))
	requireTyped[*ConfigError](t, "oversized query term", err)
	_, err = tokenizeQueryTerms(strings.Repeat("c ", maxMessageQueryBytes))
	requireTyped[*ConfigError](t, "oversized query", err)
}

// TestMessageSegments checks segment bounds, token identity and containment
// of every maxMessageQueryTerms-token window.
func TestMessageSegments(t *testing.T) {
	t.Parallel()
	small := deltaText("user", 1, 800)
	segs, unindexed, err := segmentMessageText(small, messageSegmentTokens)
	if err != nil || len(segs) != 1 || segs[0].text != small || segs[0].offset != 0 || unindexed != 0 {
		t.Fatalf("small text: %d segments, %v", len(segs), err)
	}
	tok := otelsql.MessageTokenizer{}
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
	d, err := decomposeMessages(t.Context(), spans)
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
	d, err = decomposeMessages(t.Context(), conv)
	if err != nil {
		t.Fatal(err)
	}
	alone, err := decomposeMessages(t.Context(), conv[2:3])
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

	// The span's ref_chunks pack them with the element IDs, or the
	// placeholder.
	ex.id = messageID{3, 7}
	chunks, err := (&relMessageStore{}).refChunks(want)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("%d chunks: %v", len(chunks), err)
	}
	refs, err := otelsql.UnpackRefChunk(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	wantRefs := []otelsql.MessageRef{
		{Writer: 3, Seq: 7, Attribute: in, Newest: true, Ordinals: []int{0, 2}},
		{Writer: messagePlaceholderID.writer, Seq: messagePlaceholderID.seq, Attribute: in, Ordinals: []int{1}},
		{Writer: messagePlaceholderID.writer, Seq: messagePlaceholderID.seq, Attribute: out, Newest: true, Ordinals: []int{0}},
	}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("references %+v, want %+v", refs, wantRefs)
	}
	if chunks, err := (&relMessageStore{}).refChunks(nil); err != nil || len(chunks) != 0 {
		t.Fatalf("a span without references packs %d chunks: %v", len(chunks), err)
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
	// Predicates are newRelMessageQueryPlan's (TestRelMessageQueryPlan).
	for name, q := range map[string]MessageFullTextQuery{
		"phrase":     {Query: "alpha beta", Mode: MessageMatchPhrase},
		"any":        {Query: "alpha beta", Mode: MessageMatchAny},
		"terms":      {Query: "alpha gamma"},
		"attribute":  {Query: "alpha beta", AttributeKey: "gen_ai.input.messages"},
		"role":       {Query: "alpha beta", Role: "user"},
		"first turn": {Query: "alpha beta", FirstTurnOnly: true},
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
		"inverted window":       {Query: "a", TimeWindow: &TimeRange{Start: 3, End: 2}},
		"long role":             {Query: "a", Role: strings.Repeat("r", maxMessageRoleBytes+1)},
		"unindexable long term": {Query: strings.Repeat("q", maxIndexedTokenBytes+1)},
	} {
		_, err := newMessageQueryPlan(op, q, 7)
		requireTyped[*ConfigError](t, name, err)
	}
}
