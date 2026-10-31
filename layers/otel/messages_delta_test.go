package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
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

// deltaConversation is an N-turn agent session: turn k's input repeats all
// prior user/assistant turns plus the new user turn; its output is one reply.
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
		out = append(out, deltaSpan(k,
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

func requireDeltaError[T error](t *testing.T, err error) {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("want %T, got %T: %v", target, err, err)
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

func deltaManifest(t *testing.T, rs *tracepb.ResourceSpans, key string) *MessageDeltaManifest {
	t.Helper()
	m, err := ParseMessageDeltaManifest(deltaAttr(t, rs, key))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// deltaRows lays out a blob's chunks exactly as persisted under its subspace.
func deltaRows(root subspace.Subspace, tenant int, b *MessageBlob) []fdb.KeyValue {
	space := messageBlobSpace(root, tenant, b.hash)
	rows := make([]fdb.KeyValue, len(b.chunks))
	for j, c := range b.chunks {
		rows[j] = fdb.KeyValue{Key: space.Sub(int64(j)).FDBKey(), Value: append([]byte(nil), c...)}
	}
	return rows
}

// reconstructInMemory drives the reconstruct plan and blob decoder over
// chunk rows synthesized from blobs, without FDB.
func reconstructInMemory(tenant int, blobs []*MessageBlob, spans []*tracepb.ResourceSpans) ([]*tracepb.ResourceSpans, error) {
	root := subspace.Sub("memory")
	byHash := map[[32]byte]*MessageBlob{}
	for _, b := range blobs {
		byHash[b.hash] = b
	}
	p, err := planMessageReconstruct(context.Background(), spans)
	if err != nil {
		return nil, err
	}
	raws := map[[32]byte][]byte{}
	for _, r := range p.refs {
		var rows []fdb.KeyValue
		if b := byHash[r.hash]; b != nil {
			rows = deltaRows(root, b.tenant, b)
		}
		raw, err := decodeMessageBlob(messageBlobSpace(root, tenant, r.hash), tenant, r.hash, r.encoding, rows)
		if err != nil {
			return nil, err
		}
		raws[r.hash] = raw
	}
	return p.apply(raws)
}

func TestMessageDeltaConversationDedup(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"array", "json"} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			input := deltaConversation(deltaTurns, form)
			before := cloneDeltaSpans(input)
			out, blobs, stats, err := DecomposeResourceSpans(t.Context(), input, 0)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, input, before)
			// Turn k carries 2k-1 input, 1 output, 1 system and 2 tool elements.
			n := int64(deltaTurns)
			if want := n*(n+1) + 3*n; stats.TotalMessages != want {
				t.Fatalf("total messages %d want %d", stats.TotalMessages, want)
			}
			if want := 2*n + 3; stats.UniqueMessages != want || int64(len(blobs)) != want {
				t.Fatalf("unique messages %d (blobs %d) want %d", stats.UniqueMessages, len(blobs), want)
			}
			if stats.ReusedMessages != stats.TotalMessages-stats.UniqueMessages {
				t.Fatalf("reused %d inconsistent with %+v", stats.ReusedMessages, stats)
			}
			var raw, stored int64
			for i := range input {
				for _, key := range []string{"gen_ai.system_instructions", "gen_ai.tool.definitions", "gen_ai.input.messages", "gen_ai.output.messages"} {
					raw += int64(proto.Size(deltaAttr(t, input[i], key)))
					stored += int64(proto.Size(deltaAttr(t, out[i], key)))
				}
			}
			for _, b := range blobs {
				stored += int64(b.StoredBytes())
			}
			if stats.RawMessageBytes != raw || stats.StoredDeltaBytes != stored || stats.SavedBytes != raw-stored {
				t.Fatalf("byte stats %+v want raw=%d stored=%d", stats, raw, stored)
			}
			if stats.StoredDeltaBytes*5 >= stats.RawMessageBytes {
				t.Fatalf("stored %d bytes is not an 80%% reduction of %d", stats.StoredDeltaBytes, stats.RawMessageBytes)
			}
			t.Logf("%s: total=%d unique=%d raw=%d stored=%d (%.1f%%)", form, stats.TotalMessages, stats.UniqueMessages, stats.RawMessageBytes, stats.StoredDeltaBytes, 100*float64(stats.StoredDeltaBytes)/float64(stats.RawMessageBytes))
			last := out[deltaTurns-1]
			if got, orig := proto.Size(last), proto.Size(input[deltaTurns-1]); got > orig/10 || got > maxMessageManifest {
				t.Fatalf("turn %d primary span is %d bytes (original %d)", deltaTurns, got, orig)
			}
			for k := 0; k+1 < deltaTurns; k++ {
				in, outMsg, next := deltaManifest(t, out[k], "gen_ai.input.messages"), deltaManifest(t, out[k], "gen_ai.output.messages"), deltaManifest(t, out[k+1], "gen_ai.input.messages")
				if len(in.Hashes) != 2*k+1 || len(next.Hashes) != 2*k+3 {
					t.Fatalf("turn %d input manifests carry %d/%d hashes", k+1, len(in.Hashes), len(next.Hashes))
				}
				if !next.HasPrefix(in) || next.HasPrefix(outMsg) {
					t.Fatalf("turn %d prefix lineage not verified", k+2)
				}
				history := append(append([][32]byte(nil), in.Hashes...), outMsg.Hashes...)
				if next.PrefixChains()[len(history)-1] != messageChainHead(history) {
					t.Fatalf("turn %d chain does not extend turn %d input+output", k+2, k+1)
				}
			}
			got, err := reconstructInMemory(0, blobs, out)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, got, input)
			requireDeltaSpansEqual(t, input, before)
		})
	}
}

// TestMessageDeltaGrowth shows per-call message counts across session lengths:
// referenced elements grow quadratically, stored blobs linearly.
func TestMessageDeltaGrowth(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"array", "json"} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			prevRatio := 2.0
			var prev MessageDeltaStats
			for _, n := range []int64{5, 10, 25, 50} {
				_, blobs, stats, err := DecomposeResourceSpans(t.Context(), deltaConversation(int(n), form), 0)
				if err != nil {
					t.Fatal(err)
				}
				if stats.TotalMessages != n*(n+1)+3*n || stats.UniqueMessages != 2*n+3 || int64(len(blobs)) != 2*n+3 {
					t.Fatalf("N=%d: total=%d unique=%d blobs=%d", n, stats.TotalMessages, stats.UniqueMessages, len(blobs))
				}
				// O(N): unique/N is bounded; O(N^2): total/N^2 is bounded below.
				if stats.UniqueMessages > 3*n || stats.TotalMessages < n*n {
					t.Fatalf("N=%d: unique=%d total=%d violate growth bounds", n, stats.UniqueMessages, stats.TotalMessages)
				}
				ratio := float64(stats.StoredDeltaBytes) / float64(stats.RawMessageBytes)
				if ratio >= prevRatio {
					t.Fatalf("N=%d: stored/raw %.3f did not fall below %.3f", n, ratio, prevRatio)
				}
				if prev.TotalMessages > 0 && prev.UniqueMessages > 0 {
					t.Logf("N=%d: total x%.2f unique x%.2f vs previous", n, float64(stats.TotalMessages)/float64(prev.TotalMessages), float64(stats.UniqueMessages)/float64(prev.UniqueMessages))
				}
				prev, prevRatio = stats, ratio
			}
		})
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

// TestMessageDeltaInputImmutable proves decomposition and reconstruction
// neither modify their inputs nor share mutable memory with their outputs.
func TestMessageDeltaInputImmutable(t *testing.T) {
	t.Parallel()
	input := append(deltaConversation(3, "array"), deltaConversation(3, "json")...)
	input = append(input, deltaSpan(9,
		deltaKV("gen_ai.output.messages", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(1000, 7)}}),
		deltaKV("gen_ai.input.messages", deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(300_000, 8)}})),
	), nil)
	input[0].ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.input.messages", deltaMessages("array", []deltaMsg{{"user", "event copy"}}))}}}
	before := cloneDeltaSpans(input)

	out, blobs, _, err := DecomposeResourceSpans(t.Context(), input, 4)
	if err != nil {
		t.Fatal(err)
	}
	requireDeltaSpansEqual(t, input, before)
	for i := range input {
		if input[i] != nil && out[i] == input[i] {
			t.Fatalf("output %d aliases its input", i)
		}
	}
	if out[len(out)-1] != nil {
		t.Fatal("nil input entry was not preserved")
	}
	outBefore := cloneDeltaSpans(out)

	// Blobs and manifests own their bytes: scribbling over the caller's input
	// after decomposition does not change what reconstructs.
	deltaScribbleSpans(input)
	if proto.Equal(input[len(input)-2], before[len(before)-2]) {
		t.Fatal("scribble fixture did not modify the input")
	}
	got, err := reconstructInMemory(4, blobs, out)
	if err != nil {
		t.Fatal(err)
	}
	requireDeltaSpansEqual(t, got, before)
	requireDeltaSpansEqual(t, out, outBefore)

	// Reconstruction output does not share memory with the decomposed input.
	deltaScribbleSpans(got)
	requireDeltaSpansEqual(t, out, outBefore)
	again, err := reconstructInMemory(4, blobs, out)
	if err != nil {
		t.Fatal(err)
	}
	requireDeltaSpansEqual(t, again, before)

	// Mutating the decomposed output does not reach a fresh input.
	fresh := cloneDeltaSpans(before)
	out2, _, _, err := DecomposeResourceSpans(t.Context(), fresh, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, rs := range out2 {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				sp.Name = "changed"
				for _, kv := range sp.GetAttributes() {
					kv.Value = deltaStr("changed")
				}
			}
		}
	}
	requireDeltaSpansEqual(t, fresh, before)
}

func deltaUnknown(m proto.Message) {
	m.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7))
}

func TestMessageDeltaLayouts(t *testing.T) {
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
		layout string
		count  int
	}{
		{"nil value", nil, "none", 0},
		{"empty any", &commonpb.AnyValue{}, "value", 1},
		{"plain string", deltaStr("not json at all"), "value", 1},
		{"json object", deltaStr(`{"role":"user"}`), "value", 1},
		{"json compact", deltaStr(`[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]`), "json", 2},
		{"json pretty", deltaStr("  [\n  {\"role\": \"user\"} ,\n\t{\"role\": \"tool\"}\n]\n "), "json", 2},
		{"json scalars", deltaStr(`[1,"two",null,true,[3]]`), "json", 5},
		{"json empty array", deltaStr(`[]`), "value", 1},
		{"json truncated", deltaStr(`[{"role":"user"},`), "value", 1},
		{"json trailing value", deltaStr(`[1] [2]`), "value", 1},
		{"json invalid utf8 escape", deltaStr(`["\ud800"]`), "json", 1},
		{"int", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}, "value", 1},
		{"bytes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("[1,2]")}}, "value", 1},
		{"kvlist", deltaKvlist(deltaKV("role", deltaStr("user"))), "value", 1},
		{"empty array", deltaArray(), "array", 0},
		{"nil array", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{}}, "array", 0},
		{"array mixed", deltaArray(deltaStr("s"), &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0, 1}}}, deltaKvlist(), deltaArray(deltaStr("nested")), unknownElement), "array", 5},
		{"array unknown fields", unknownArray, "value", 1},
		{"shell unknown fields", unknownShell, "value", 1},
		{"json unknown fields", unknownJSON, "value", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rs := deltaSpan(1, deltaKV("gen_ai.input.messages", c.value), deltaKV("other", deltaStr(`[{"role":"user"}]`)))
			rs.ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "gen_ai.client.inference.operation.details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.output.messages", proto.Clone(rs.ScopeSpans[0].Spans[0].Attributes[0]).(*commonpb.KeyValue).Value)}}}
			in := []*tracepb.ResourceSpans{rs, nil}
			before := cloneDeltaSpans(in)
			out, blobs, stats, err := DecomposeResourceSpans(t.Context(), in, 3)
			if err != nil {
				t.Fatal(err)
			}
			if out[1] != nil || out[0] == rs {
				t.Fatal("decompose must clone non-nil inputs and keep nil entries")
			}
			if !proto.Equal(deltaAttr(t, out[0], "other"), deltaStr(`[{"role":"user"}]`)) {
				t.Fatal("non-content attribute changed")
			}
			m := deltaManifest(t, out[0], "gen_ai.input.messages")
			ev, err := ParseMessageDeltaManifest(out[0].ScopeSpans[0].Spans[0].Events[0].Attributes[0].Value)
			if err != nil {
				t.Fatal(err)
			}
			if m.Layout != c.layout || len(m.Hashes) != c.count || ev.Layout != c.layout {
				t.Fatalf("layout %s/%d (event %s) want %s/%d", m.Layout, len(m.Hashes), ev.Layout, c.layout, c.count)
			}
			if stats.TotalMessages != int64(2*c.count) || int(stats.UniqueMessages) != len(blobs) {
				t.Fatalf("stats %+v for %d elements per attribute", stats, c.count)
			}
			got, err := reconstructInMemory(3, blobs, out)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, got, in)
			requireDeltaSpansEqual(t, in, before)
		})
	}
}

func deltaRandom(n int, seed byte) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{seed}).Read(b)
	return b
}

func TestMessageDeltaOversizedChunks(t *testing.T) {
	t.Parallel()
	random := deltaRandom(260_000, 1)
	doc := []byte(deltaText("rag-document", 9, 1<<20))
	for _, c := range []struct {
		name   string
		raw    []byte
		codec  byte
		chunks int
	}{
		{"incompressible tool output", random, messageCodecRaw, 4},
		{"compressible rag document", doc, messageCodecBrotli, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			value := deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: c.raw}})
			in := []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.output.messages", value))}
			out, blobs, _, err := DecomposeResourceSpans(t.Context(), in, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(blobs) != 1 {
				t.Fatalf("got %d blobs", len(blobs))
			}
			b := blobs[0]
			if b.chunks[0][1] != c.codec || (c.chunks > 0 && b.Chunks() != c.chunks) {
				t.Fatalf("codec %d chunks %d want %d/%d", b.chunks[0][1], b.Chunks(), c.codec, c.chunks)
			}
			for j, chunk := range b.chunks {
				if len(chunk) > messageChunkBytes {
					t.Fatalf("chunk %d is %d bytes", j, len(chunk))
				}
			}
			got, err := reconstructInMemory(0, blobs, out)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, got, in)
			if !bytes.Equal(deltaAttr(t, got[0], "gen_ai.output.messages").GetArrayValue().Values[0].GetBytesValue(), c.raw) {
				t.Fatal("oversized message not restored byte-for-byte")
			}
		})
	}
	if n := messageChunkCount(maxMessageBytes); n*messageChunkBytes < maxMessageBytes || len(messageBlobSpace(subspace.Sub("x"), 0, [32]byte{}).Sub(int64(n)).Bytes()) > 200 {
		t.Fatalf("chunk count %d cannot cover maximum message", n)
	}
	for _, c := range []struct{ stored, want int }{{0, 1}, {79_968, 1}, {79_969, 2}, {159_968, 2}, {159_969, 3}} {
		if got := messageChunkCount(c.stored); got != c.want {
			t.Fatalf("messageChunkCount(%d)=%d want %d", c.stored, got, c.want)
		}
	}
}

func TestMessageDeltaAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	huge := deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: make([]byte, maxMessageBytes)}})
	_, _, _, err := DecomposeResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", huge))}, 0)
	requireDeltaError[*AdmissionError](t, err)

	many := make([]*commonpb.AnyValue, 2001)
	for i := range many {
		many[i] = deltaStr(fmt.Sprint(i))
	}
	_, _, _, err = DecomposeResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(many...)))}, 0)
	requireDeltaError[*AdmissionError](t, err)
	if _, _, _, err := DecomposeResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(many[:1900]...)))}, 0); err != nil {
		t.Fatalf("1900-message attribute must fit the manifest cap: %v", err)
	}

	_, _, _, err = DecomposeResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("a"), nil)))}, 0)
	if err != nil {
		t.Fatalf("nil array element in cloned proto became empty AnyValue and should succeed: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := DecomposeResourceSpans(canceledCtx, deltaConversation(1, "array"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}

	_, blobs, _, err := DecomposeResourceSpans(ctx, deltaConversation(1, "array"), 0)
	if err != nil {
		t.Fatal(err)
	}
	root := subspace.Sub("admission")
	_, err = PersistMessageBlobs(ctx, nil, root, 1, blobs)
	requireDeltaError[*AdmissionError](t, err)
	_, err = PersistMessageBlobs(ctx, nil, root, 0, []*MessageBlob{{}})
	requireDeltaError[*AdmissionError](t, err)
	forged := *blobs[0]
	forged.raw = append([]byte("x"), forged.raw...)
	_, err = PersistMessageBlobs(ctx, nil, root, 0, []*MessageBlob{&forged})
	requireDeltaError[*AdmissionError](t, err)
	_, err = PersistMessageBlobs(ctx, nil, subspace.FromBytes(make([]byte, maxMessageKeyBytes)), 0, blobs)
	requireDeltaError[*ConfigError](t, err)
	_, err = PersistMessageBlobs(ctx, nil, nil, 0, blobs)
	requireDeltaError[*ConfigError](t, err)
	_, err = PersistMessageBlobsDB(ctx, nil, root, 0, blobs)
	requireDeltaError[*ConfigError](t, err)
	_, err = ReconstructResourceSpansDB(ctx, nil, root, 0, nil)
	requireDeltaError[*ConfigError](t, err)
	_, err = ReconstructResourceSpans(ctx, nil, subspace.FromBytes(make([]byte, maxMessageKeyBytes)), 0, nil)
	requireDeltaError[*ConfigError](t, err)
}

func TestMessageDeltaManifestCorruption(t *testing.T) {
	t.Parallel()
	out, blobs, _, err := DecomposeResourceSpans(t.Context(), deltaConversation(2, "json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	good := deltaAttr(t, out[1], "gen_ai.input.messages").GetBytesValue()
	if _, err := ParseMessageDeltaManifest(deltaAttr(t, out[1], "gen_ai.input.messages")); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	mutate := func(f func([]byte) []byte) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: f(append([]byte(nil), good...))}}
	}
	twoValues := &MessageDeltaManifest{layout: messageLayoutValue, Hashes: make([][32]byte, 2)}
	twoValues.Chain = messageChainHead(twoValues.Hashes)
	emptyJSON := &MessageDeltaManifest{layout: messageLayoutJSON}
	emptyJSON.Chain = messageChainHead(nil)
	for _, c := range []struct {
		name  string
		value *commonpb.AnyValue
	}{
		{"original string", deltaStr(`[{"role":"user"}]`)},
		{"nil value", nil},
		{"bad magic", mutate(func(b []byte) []byte { b[0] = 'X'; return b })},
		{"bad version", mutate(func(b []byte) []byte { b[4] = 9; return b })},
		{"bad layout", mutate(func(b []byte) []byte { b[5] = 9; return b })},
		{"flipped hash", mutate(func(b []byte) []byte { b[7] ^= 1; return b })},
		{"truncated", mutate(func(b []byte) []byte { return b[:len(b)-1] })},
		{"trailing", mutate(func(b []byte) []byte { return append(b, 0) })},
		{"bad separator flag", mutate(func(b []byte) []byte { b[len(b)-1] = 7; return b })},
		{"value layout with two hashes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: twoValues.encode()}}},
		{"json layout without elements", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: emptyJSON.encode()}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseMessageDeltaManifest(c.value)
			requireDeltaError[*CorruptionError](t, err)
			bad := cloneDeltaSpans(out)
			for _, kv := range bad[1].ScopeSpans[0].Spans[0].Attributes {
				if kv.Key == "gen_ai.input.messages" {
					kv.Value = c.value
				}
			}
			_, err = reconstructInMemory(0, blobs, bad)
			requireDeltaError[*CorruptionError](t, err)
		})
	}
	// One digest cannot be referenced under two element encodings.
	mixed := cloneDeltaSpans(out)
	h := deltaManifest(t, out[0], "gen_ai.output.messages").Hashes[0]
	arr := &MessageDeltaManifest{layout: messageLayoutArray, Hashes: [][32]byte{h}}
	arr.Chain = messageChainHead(arr.Hashes)
	mixed[0].ScopeSpans[0].Spans[0].Attributes = append(mixed[0].ScopeSpans[0].Spans[0].Attributes, deltaKV("gen_ai.output.messages", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: arr.encode()}}))
	_, err = reconstructInMemory(0, blobs, mixed)
	requireDeltaError[*CorruptionError](t, err)
}

func TestMessageDeltaBlobDecodeCorruption(t *testing.T) {
	t.Parallel()
	root := subspace.Sub("decode")
	value := deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(200_000, 2)}}, deltaStr(deltaText("assistant", 1, 4000)), deltaStr("tiny"))
	_, blobs, _, err := DecomposeResourceSpans(t.Context(), []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.output.messages", value))}, 5)
	if err != nil {
		t.Fatal(err)
	}
	big, text, tiny := blobs[0], blobs[1], blobs[2]
	if big.Chunks() != 3 || big.chunks[0][1] != messageCodecRaw || text.chunks[0][1] != messageCodecBrotli || tiny.chunks[0][1] != messageCodecRaw {
		t.Fatal("fixture does not cover raw multi-chunk, brotli and small raw blobs")
	}
	decode := func(b *MessageBlob, tenant int, encoding string, rows []fdb.KeyValue) error {
		_, err := decodeMessageBlob(messageBlobSpace(root, tenant, b.hash), tenant, b.hash, encoding, rows)
		return err
	}
	for _, b := range blobs {
		raw, err := decodeMessageBlob(messageBlobSpace(root, 5, b.hash), 5, b.hash, b.encoding, deltaRows(root, 5, b))
		if err != nil || !bytes.Equal(raw, b.raw) {
			t.Fatalf("positive control failed: %v", err)
		}
	}
	space := messageBlobSpace(root, 5, big.hash)
	for _, c := range []struct {
		name string
		blob *MessageBlob
		edit func([]fdb.KeyValue) []fdb.KeyValue
	}{
		{"missing", big, func([]fdb.KeyValue) []fdb.KeyValue { return nil }},
		{"malformed key", big, func(r []fdb.KeyValue) []fdb.KeyValue { r[1].Key = space.Pack(tuple.Tuple{"x"}); return r }},
		{"two-element key", big, func(r []fdb.KeyValue) []fdb.KeyValue {
			r[1].Key = space.Pack(tuple.Tuple{int64(1), int64(0)})
			return r
		}},
		{"gap", big, func(r []fdb.KeyValue) []fdb.KeyValue { return append(r[:1], r[2:]...) }},
		{"chunk 0 missing", big, func(r []fdb.KeyValue) []fdb.KeyValue { return r[1:] }},
		{"last chunk missing", big, func(r []fdb.KeyValue) []fdb.KeyValue { return r[:2] }},
		{"extra chunk", big, func(r []fdb.KeyValue) []fdb.KeyValue {
			return append(r, fdb.KeyValue{Key: space.Sub(int64(3)).FDBKey(), Value: []byte{1}})
		}},
		{"oversized chunk", big, func(r []fdb.KeyValue) []fdb.KeyValue { r[1].Value = make([]byte, messageChunkBytes+1); return r }},
		{"short chunk", big, func(r []fdb.KeyValue) []fdb.KeyValue { r[2].Value = r[2].Value[1:]; return r }},
		{"flipped payload", big, func(r []fdb.KeyValue) []fdb.KeyValue { r[1].Value[10] ^= 1; return r }},
		{"bad version", big, func(r []fdb.KeyValue) []fdb.KeyValue { r[0].Value[0] = 2; return r }},
		{"bad codec", big, func(r []fdb.KeyValue) []fdb.KeyValue { r[0].Value[1] = 7; return r }},
		{"truncated header", tiny, func(r []fdb.KeyValue) []fdb.KeyValue { r[0].Value = r[0].Value[:3]; return r }},
		{"raw length mismatch", tiny, func(r []fdb.KeyValue) []fdb.KeyValue { r[0].Value[2]++; return r }},
		{"brotli garbage", text, func(r []fdb.KeyValue) []fdb.KeyValue {
			for i := 8; i < len(r[0].Value); i++ {
				r[0].Value[i] = 0xff
			}
			return r
		}},
		{"brotli length mismatch", text, func(r []fdb.KeyValue) []fdb.KeyValue {
			v := r[0].Value
			raw, n := uvarintAt(v, 2)
			r[0].Value = append(append(append([]byte(nil), v[:2]...), uvarintBytes(raw-1)...), v[2+n:]...)
			return r
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			requireDeltaError[*CorruptionError](t, decode(c.blob, 5, c.blob.encoding, c.edit(deltaRows(root, 5, c.blob))))
		})
	}
	// Bytes stored for tenant 5 never verify as another tenant's or encoding's.
	requireDeltaError[*CorruptionError](t, decode(tiny, 6, tiny.encoding, deltaRows(root, 6, tiny)))
	requireDeltaError[*CorruptionError](t, decode(tiny, 5, messageEncodingJSON, deltaRows(root, 5, tiny)))
	if messageHash(0, messageEncodingAny, []byte("m")) == messageHash(1, messageEncodingAny, []byte("m")) || messageHash(0, messageEncodingAny, []byte("m")) == messageHash(0, messageEncodingJSON, []byte("m")) {
		t.Fatal("message hash is not domain separated by tenant and encoding")
	}
}

func uvarintAt(b []byte, off int) (int, int) {
	r := &byteReader{b: b[off:]}
	before := len(r.b)
	v := r.uvarint(maxMessageBytes)
	return v, before - len(r.b)
}

func uvarintBytes(v int) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// startMessageDeltaCluster serves only freshly allocated test namespaces.
func startMessageDeltaCluster(t *testing.T) string {
	t.Helper()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
}

func deltaBlobRows(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, ss subspace.Subspace) []fdb.KeyValue {
	t.Helper()
	var rows []fdb.KeyValue
	_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		var err error
		rows, err = rc.Transaction().GetRange(ss, fdb.RangeOptions{Limit: 20001}).GetSliceWithError()
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 20000 {
		t.Fatal("blob census exceeded 20000-key bound")
	}
	return rows
}

func deltaRun(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, fn func(*recordlayer.FDBRecordContext) error) error {
	t.Helper()
	_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) { return nil, fn(rc) })
	return err
}

func TestMessageDeltaFDB(t *testing.T) {
	t.Parallel()
	cluster := startMessageDeltaCluster(t)
	fixture := func(t *testing.T) (context.Context, *recordlayer.FDBDatabase, subspace.Subspace) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		t.Cleanup(cancel)
		b := openOtelFixture(t, ctx, cluster, DefaultConfig(), false)
		return ctx, b.db, b.root.Sub("messages")
	}

	for _, form := range []string{"array", "json"} {
		t.Run("conversation_"+form, func(t *testing.T) {
			t.Parallel()
			ctx, db, root := fixture(t)
			input := deltaConversation(deltaTurns, form)
			before := cloneDeltaSpans(input)
			out, blobs, stats, err := DecomposeResourceSpans(ctx, input, 0)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Written != len(blobs) || persisted.Present != 0 {
				t.Fatalf("first persist %+v for %d blobs", persisted, len(blobs))
			}
			rows := deltaBlobRows(t, ctx, db, root.Sub(messageBlobSubspace, int64(0)))
			var chunks, physical int
			for _, b := range blobs {
				chunks += b.Chunks()
				physical += b.StoredBytes()
			}
			valueBytes := 0
			for _, kv := range rows {
				valueBytes += len(kv.Value)
			}
			if len(rows) != chunks || valueBytes != physical || chunks != int(stats.UniqueMessages) {
				t.Fatalf("FDB census %d keys/%d bytes, want %d/%d", len(rows), valueBytes, chunks, physical)
			}
			again, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs)
			if err != nil {
				t.Fatal(err)
			}
			if again.Written != 0 || again.Present != len(blobs) || again.Bytes != 0 {
				t.Fatalf("repeat persist rewrote blobs: %+v", again)
			}
			outBefore := cloneDeltaSpans(out)
			got, err := ReconstructResourceSpansDB(ctx, db, root, 0, out)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, got, input)
			requireDeltaSpansEqual(t, input, before)
			requireDeltaSpansEqual(t, out, outBefore)
		})

		t.Run("incremental_turns_"+form, func(t *testing.T) {
			t.Parallel()
			ctx, db, root := fixture(t)
			input := deltaConversation(deltaTurns, form)
			var decomposed []*tracepb.ResourceSpans
			totalWritten := 0
			for k, rs := range input {
				out, blobs, _, err := DecomposeResourceSpans(ctx, []*tracepb.ResourceSpans{rs}, 0)
				if err != nil {
					t.Fatal(err)
				}
				s, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs)
				if err != nil {
					t.Fatal(err)
				}
				// Turn 1 adds system, two tools, user and reply; later turns add a user and a reply.
				want := 2
				if k == 0 {
					want = 5
				}
				if s.Written != want || s.Present != len(blobs)-want {
					t.Fatalf("turn %d persisted %+v of %d blobs, want %d new", k+1, s, len(blobs), want)
				}
				totalWritten += s.Written
				decomposed = append(decomposed, out...)
			}
			if totalWritten != 2*deltaTurns+3 {
				t.Fatalf("wrote %d blobs across turns", totalWritten)
			}
			got, err := ReconstructResourceSpansDB(ctx, db, root, 0, decomposed)
			if err != nil {
				t.Fatal(err)
			}
			requireDeltaSpansEqual(t, got, input)
		})
	}

	t.Run("record_context_api", func(t *testing.T) {
		t.Parallel()
		ctx, db, root := fixture(t)
		input := deltaConversation(4, "array")
		out, blobs, _, err := DecomposeResourceSpans(ctx, input, 2)
		if err != nil {
			t.Fatal(err)
		}
		err = deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
			if err := configureRecordTimeout(rc); err != nil {
				return err
			}
			if _, err := PersistMessageBlobs(ctx, rc, root, 2, blobs); err != nil {
				return err
			}
			got, err := ReconstructResourceSpans(ctx, rc, root, 2, out)
			if err != nil {
				return err
			}
			requireDeltaSpansEqual(t, got, input)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
			got, err := ReconstructResourceSpans(ctx, rc, root, 2, out)
			if err == nil {
				requireDeltaSpansEqual(t, got, input)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("oversized_messages", func(t *testing.T) {
		t.Parallel()
		ctx, db, root := fixture(t)
		tool := deltaRandom(260_000, 3)
		rag := deltaRandom(1<<20, 4)
		input := []*tracepb.ResourceSpans{deltaSpan(1,
			deltaKV("gen_ai.input.messages", deltaArray(deltaKvlist(deltaKV("role", deltaStr("tool")), deltaKV("content", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: tool}})))),
			deltaKV("gen_ai.output.messages", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: rag}}),
		)}
		out, blobs, _, err := DecomposeResourceSpans(ctx, input, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(blobs) != 2 || blobs[0].Chunks() != 4 || blobs[1].Chunks() != 14 {
			t.Fatalf("unexpected oversized layout: %d blobs", len(blobs))
		}
		err = deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
			_, err := PersistMessageBlobs(ctx, rc, root, 0, blobs)
			return err
		})
		requireDeltaError[*AdmissionError](t, err)
		err = deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
			s, err := PersistMessageBlobs(ctx, rc, root, 0, blobs[:1])
			if err == nil && (s.Written != 1 || s.Chunks != 4) {
				err = fmt.Errorf("single-transaction persist %+v", s)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		s, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs)
		if err != nil {
			t.Fatal(err)
		}
		// Both the 260 KB and 1 MiB blobs are staged: blobs[0] was already persisted above (1 probe),
		// and the 1 MiB blob runs 1 probe + 4 tail transactions + 1 chunk-0 transaction = 7 total.
		if s.Written != 1 || s.Present != 1 || s.Chunks != 14 || s.Transactions != 7 {
			t.Fatalf("staged persist %+v", s)
		}
		for _, b := range blobs {
			rows := deltaBlobRows(t, ctx, db, messageBlobSpace(root, 0, b.hash))
			if len(rows) != b.Chunks() {
				t.Fatalf("blob has %d FDB chunks, want %d", len(rows), b.Chunks())
			}
			for j, kv := range rows {
				if len(kv.Value) > messageChunkBytes || !bytes.Equal(kv.Value, b.chunks[j]) {
					t.Fatalf("chunk %d: %d bytes, mismatch=%v", j, len(kv.Value), !bytes.Equal(kv.Value, b.chunks[j]))
				}
			}
		}
		got, err := ReconstructResourceSpansDB(ctx, db, root, 0, out)
		if err != nil {
			t.Fatal(err)
		}
		requireDeltaSpansEqual(t, got, input)
		if !bytes.Equal(deltaAttr(t, got[0], "gen_ai.output.messages").GetBytesValue(), rag) {
			t.Fatal("1 MiB message not restored byte-for-byte")
		}
	})

	t.Run("tenant_isolation_and_tampering", func(t *testing.T) {
		t.Parallel()
		ctx, db, root := fixture(t)
		input := deltaConversation(3, "json")
		input[2].ScopeSpans[0].Spans[0].Attributes = append(input[2].ScopeSpans[0].Spans[0].Attributes, deltaKV("gen_ai.tool.definitions", deltaArray(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(200_000, 5)}})))
		out, blobs, _, err := DecomposeResourceSpans(ctx, input, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs); err != nil {
			t.Fatal(err)
		}
		_, err = ReconstructResourceSpansDB(ctx, db, root, 1, out)
		requireDeltaError[*CorruptionError](t, err)

		// Copy tenant 0's chunks verbatim into tenant 1's keyspace: the digest
		// domain still rejects them.
		t0, t1 := root.Sub(messageBlobSubspace, int64(0)), root.Sub(messageBlobSubspace, int64(1))
		rows := deltaBlobRows(t, ctx, db, t0)
		for lo := 0; lo < len(rows); lo += 2 {
			batch := rows[lo:min(lo+2, len(rows))]
			if err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
				for _, kv := range batch {
					suffix, err := t0.Unpack(kv.Key)
					if err != nil {
						return err
					}
					rc.Transaction().Set(t1.Pack(suffix), kv.Value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(deltaBlobRows(t, ctx, db, t1)); n != len(rows) {
			t.Fatalf("copied %d of %d chunks", n, len(rows))
		}
		_, err = ReconstructResourceSpansDB(ctx, db, root, 1, out)
		requireDeltaError[*CorruptionError](t, err)
		if _, err := ReconstructResourceSpansDB(ctx, db, root, 0, out); err != nil {
			t.Fatalf("tenant 0 positive control: %v", err)
		}

		big := blobs[len(blobs)-1]
		if big.Chunks() != 3 {
			t.Fatalf("tamper fixture has %d chunks", big.Chunks())
		}
		space := messageBlobSpace(root, 0, big.hash)
		set := func(key fdb.KeyConvertible, value []byte) {
			t.Helper()
			if err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
				if value == nil {
					rc.Transaction().Clear(key)
				} else {
					rc.Transaction().Set(key, value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		tampered := append([]byte(nil), big.chunks[1]...)
		tampered[100] ^= 1
		set(space.Sub(int64(1)), tampered)
		_, err = ReconstructResourceSpansDB(ctx, db, root, 0, out)
		requireDeltaError[*CorruptionError](t, err)
		set(space.Sub(int64(1)), nil)
		_, err = ReconstructResourceSpansDB(ctx, db, root, 0, out)
		requireDeltaError[*CorruptionError](t, err)

		// A missing blob is an explicit error; persisting again repairs it
		// because chunk 0 no longer matches.
		set(space.Sub(int64(0)), nil)
		_, err = ReconstructResourceSpansDB(ctx, db, root, 0, out)
		requireDeltaError[*CorruptionError](t, err)
		s, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs)
		if err != nil {
			t.Fatal(err)
		}
		if s.Written != 1 || s.Present != len(blobs)-1 {
			t.Fatalf("repair persist %+v", s)
		}
		got, err := ReconstructResourceSpansDB(ctx, db, root, 0, out)
		if err != nil {
			t.Fatal(err)
		}
		requireDeltaSpansEqual(t, got, input)
	})

	t.Run("staged_blob_invisible_until_chunk_zero", func(t *testing.T) {
		t.Parallel()
		ctx, db, root := fixture(t)
		rag := deltaRandom(600_000, 6)
		input := []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.output.messages", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: rag}}))}
		out, blobs, _, err := DecomposeResourceSpans(ctx, input, 0)
		if err != nil {
			t.Fatal(err)
		}
		b := blobs[0]
		space := messageBlobSpace(root, 0, b.hash)
		// Simulate a writer that committed only tail chunks before failing.
		for j := 1; j < b.Chunks(); j++ {
			if err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
				rc.Transaction().Set(space.Sub(int64(j)), b.chunks[j])
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		_, err = ReconstructResourceSpansDB(ctx, db, root, 0, out)
		requireDeltaError[*CorruptionError](t, err)
		s, err := PersistMessageBlobsDB(ctx, db, root, 0, blobs)
		if err != nil {
			t.Fatal(err)
		}
		if s.Written != 1 {
			t.Fatalf("staged persist %+v", s)
		}
		got, err := ReconstructResourceSpansDB(ctx, db, root, 0, out)
		if err != nil {
			t.Fatal(err)
		}
		requireDeltaSpansEqual(t, got, input)
	})
}
