package otel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ftsBase is deltaSpan's start time origin: turn k starts at ftsBase+k.
const ftsBase = int64(1700000000000000000)

type ftsRoleText struct{ role, text string }

func ftsTrace(k int) string { return hex.EncodeToString(bytes.Repeat([]byte{byte(k + 1)}, 16)) }
func ftsSpan(k int) string  { return hex.EncodeToString(bytes.Repeat([]byte{byte(k + 101)}, 8)) }

func ftsAnyWire(t *testing.T, v *commonpb.AnyValue) []byte {
	t.Helper()
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// ---------------------------------------------------------------------------
// Text extraction and tokenization
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
		_, blobs, _, err := DecomposeResourceSpans(t.Context(), deltaConversation(turns, form), 0)
		if err != nil {
			t.Fatal(err)
		}
		got := map[ftsRoleText]bool{}
		for _, b := range blobs {
			role, text, err := ExtractMessageText(b)
			if err != nil {
				t.Fatal(err)
			}
			got[ftsRoleText{role, text}] = true
		}
		if len(blobs) != 2*turns+3 || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: extracted %d blobs\n got %v\nwant %v", form, len(blobs), got, want)
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
		role, text, err := extractMessageEncodingText(messageEncodingAny, ftsAnyWire(t, c.any))
		if err != nil || role != c.role || text != c.text {
			t.Fatalf("%s otlp_any: (%q, %q, %v) want (%q, %q)", c.name, role, text, err, c.role, c.text)
		}
		// JSON object order must not depend on map iteration.
		for range 20 {
			role, text, err := extractMessageEncodingText(messageEncodingJSON, []byte(c.json))
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
		_, _, err := extractMessageEncodingText(c.encoding, c.raw)
		requireTyped[*CorruptionError](t, name, err)
	}
	_, _, err := ExtractMessageText(nil)
	requireTyped[*AdmissionError](t, "nil blob", err)
}

func TestMessageTokenization(t *testing.T) {
	t.Parallel()
	got, err := TokenizeMessageText("Crème BRÛLÉE, crème! Ångström")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]int{"creme": {0, 2}, "brulee": {1}, "angstrom": {3}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unicode folding: %v", got)
	}
	got, err = TokenizeMessageText(strings.Repeat("a", maxIndexedTokenBytes+1) + " ok")
	if err != nil || !reflect.DeepEqual(got, map[string][]int{"ok": {1}}) {
		t.Fatalf("oversized token must be dropped, positions kept: %v %v", got, err)
	}
	got, err = TokenizeMessageText(strings.Repeat("x ", 2000))
	if err != nil || len(got["x"]) != maxPositionsPerToken || got["x"][maxPositionsPerToken-1] != maxPositionsPerToken-1 {
		t.Fatalf("position cap: %d positions, %v", len(got["x"]), err)
	}
	terms, err := TokenizeQueryText("Ünïcödé  FAÇADE! query_traces")
	if err != nil || !reflect.DeepEqual(terms, []string{"unicode", "facade", "query_traces"}) {
		t.Fatalf("query terms %v %v", terms, err)
	}
	_, err = TokenizeQueryText(strings.Repeat("b", maxIndexedTokenBytes+1))
	requireTyped[*ConfigError](t, "oversized query term", err)
	_, err = TokenizeQueryText(strings.Repeat("c ", maxMessageQueryBytes))
	requireTyped[*ConfigError](t, "oversized query", err)
}

// ---------------------------------------------------------------------------
// Row codecs
// ---------------------------------------------------------------------------

func TestMessagePostingCodec(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		enc, role string
		pos       []int
	}{{messageEncodingAny, "user", []int{0}}, {messageEncodingJSON, "", []int{3, 4, 900, 1 << 20}}} {
		enc, role, pos, err := decodeMessagePosting(encodeMessagePosting(c.enc, c.role, c.pos))
		if err != nil || enc != c.enc || role != c.role || !reflect.DeepEqual(pos, c.pos) {
			t.Fatalf("round trip %+v: %s %s %v %v", c, enc, role, pos, err)
		}
	}
	good := encodeMessagePosting(messageEncodingAny, "user", []int{1, 2})
	tooMany := make([]int, maxPositionsPerToken+1)
	for i := range tooMany {
		tooMany[i] = i
	}
	for name, v := range map[string][]byte{
		"empty":         nil,
		"bad version":   append([]byte{2}, good[1:]...),
		"trailing":      append(append([]byte(nil), good...), 0),
		"truncated":     good[:len(good)-1],
		"repeated":      encodeMessagePosting(messageEncodingAny, "user", []int{3, 3}),
		"no positions":  encodeMessagePosting(messageEncodingAny, "user", nil),
		"bad encoding":  encodeMessagePosting("xml", "user", []int{1}),
		"too many":      encodeMessagePosting(messageEncodingAny, "user", tooMany),
		"oversize role": encodeMessagePosting(messageEncodingAny, strings.Repeat("r", maxMessageRoleBytes+1), []int{1}),
	} {
		_, _, _, err := decodeMessagePosting(v)
		requireTyped[*CorruptionError](t, name, err)
	}
	enc, role, n, err := decodeMessageMeta(encodeMessageMeta(messageEncodingJSON, "tool", 17))
	if err != nil || enc != messageEncodingJSON || role != "tool" || n != 17 {
		t.Fatalf("meta round trip: %s %s %d %v", enc, role, n, err)
	}
	for name, v := range map[string][]byte{
		"garbage":       {0xff},
		"bad version":   tuple.Tuple{int64(2), messageEncodingAny, "", int64(1)}.Pack(),
		"bad encoding":  tuple.Tuple{int64(1), "xml", "", int64(1)}.Pack(),
		"negative":      tuple.Tuple{int64(1), messageEncodingAny, "", int64(-1)}.Pack(),
		"missing field": tuple.Tuple{int64(1), messageEncodingAny, ""}.Pack(),
	} {
		_, _, _, err := decodeMessageMeta(v)
		requireTyped[*CorruptionError](t, name, err)
	}
}

func TestMessageSpanRefCodec(t *testing.T) {
	t.Parallel()
	root := subspace.Sub("fts_codec")
	ref := &messageSpanRef{hash: [32]byte{0, 1, 2}, trace: qaID(5, 16), span: qaID(6, 8), start: 12345, attrKey: "gen_ai.input.messages", encoding: messageEncodingJSON, ordinals: []int{2, 7}, count: 9, prefix: [32]byte{9}, chain: [32]byte{8}}
	for _, order := range []string{"time", "trace"} {
		prefix := messageSpanRefSpace(root, 4, ref.hash)
		key := prefix.Pack(messageRefSuffix(ref.trace, ref.span, ref.start, ref.attrKey, order))
		value := encodeMessageSpanRef(key, ref)
		got, suffix, err := decodeMessageSpanRef(prefix, ref.hash, order, fdb.KeyValue{Key: key, Value: value})
		if err != nil || !reflect.DeepEqual(got, ref) || !bytes.Equal(prefix.Pack(suffix), key) {
			t.Fatalf("%s round trip: %+v %v", order, got, err)
		}
		other := prefix.Pack(messageRefSuffix(ref.trace, qaID(7, 8), ref.start, ref.attrKey, order))
		flipped := append([]byte(nil), value...)
		flipped[len(flipped)/2] ^= 1
		bad := *ref
		bad.ordinals = []int{9}
		mismatch := *ref
		mismatch.attrKey = "gen_ai.output.messages"
		for name, kv := range map[string]fdb.KeyValue{
			"flipped value":       {Key: key, Value: flipped},
			"value under new key": {Key: other, Value: value},
			"truncated":           {Key: key, Value: value[:10]},
			"ordinal past count":  {Key: key, Value: encodeMessageSpanRef(key, &bad)},
			"attribute mismatch":  {Key: key, Value: encodeMessageSpanRef(key, &mismatch)},
		} {
			_, _, err := decodeMessageSpanRef(prefix, ref.hash, order, kv)
			requireTyped[*CorruptionError](t, order+" "+name, err)
		}
		// A row in one order never decodes in the other.
		other2 := map[string]string{"time": "trace", "trace": "time"}[order]
		_, _, err = decodeMessageSpanRef(prefix, ref.hash, other2, fdb.KeyValue{Key: key, Value: value})
		requireTyped[*CorruptionError](t, order+" cross order", err)
	}
}

// ---------------------------------------------------------------------------
// Span reference planning
// ---------------------------------------------------------------------------

func ftsPlanRefs(t *testing.T, spans []*tracepb.ResourceSpans) []*messageSpanPlan {
	t.Helper()
	plans, err := planMessageSpans(t.Context(), 11, spans)
	if err != nil {
		t.Fatal(err)
	}
	return plans
}

func TestMessageSpanPlan(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"array", "json"} {
		input := deltaConversation(4, form)
		out, _, _, err := DecomposeResourceSpans(t.Context(), input, 11)
		if err != nil {
			t.Fatal(err)
		}
		decomposed := ftsPlanRefs(t, out)
		// Raw values and base64 manifests (an indexed copy) plan identically.
		b64 := cloneDeltaSpans(out)
		for _, rs := range b64 {
			for _, kv := range rs.ScopeSpans[0].Spans[0].Attributes {
				if IsMessageDeltaAttribute(kv.Key) {
					kv.Value = deltaStr(base64.StdEncoding.EncodeToString(kv.Value.GetBytesValue()))
				}
			}
		}
		if raw := ftsPlanRefs(t, input); !reflect.DeepEqual(raw, decomposed) {
			t.Fatalf("%s: raw and decomposed plans differ", form)
		}
		if enc := ftsPlanRefs(t, b64); !reflect.DeepEqual(enc, decomposed) {
			t.Fatalf("%s: base64 manifest plan differs", form)
		}
		if len(decomposed) != 4 {
			t.Fatalf("%s: %d plans", form, len(decomposed))
		}
		for k, p := range decomposed {
			if len(p.refs) != 2*(k+1)+3 || p.trace != ftsTrace(k+1) || p.span != ftsSpan(k+1) || p.start != ftsBase+int64(k+1) {
				t.Fatalf("%s turn %d: %d refs, identity %s/%s/%d", form, k+1, len(p.refs), p.trace, p.span, p.start)
			}
			m := deltaManifest(t, out[k], "gen_ai.input.messages")
			chains := m.PrefixChains()
			for _, r := range p.refs {
				if r.attrKey != "gen_ai.input.messages" {
					continue
				}
				i := r.ordinals[0]
				if len(r.ordinals) != 1 || m.Hashes[i] != r.hash || chains[i] != r.prefix || m.Chain != r.chain || r.count != len(m.Hashes) || r.encoding != manifestEncoding(m) {
					t.Fatalf("%s turn %d: input ref %+v disagrees with manifest", form, k+1, r)
				}
			}
		}
	}

	// A repeated message keeps every ordinal; the first site of a key wins.
	a, b := deltaStr("again"), deltaStr("other")
	rs := deltaSpan(1, deltaKV("gen_ai.input.messages", deltaArray(a, b, proto.Clone(a).(*commonpb.AnyValue))))
	rs.ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "ev", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.input.messages", deltaArray(deltaStr("third"), proto.Clone(b).(*commonpb.AnyValue)))}}}
	plans := ftsPlanRefs(t, []*tracepb.ResourceSpans{rs})
	var ords [][]int
	var counts []int
	for _, r := range plans[0].refs {
		ords = append(ords, r.ordinals)
		counts = append(counts, r.count)
	}
	if !reflect.DeepEqual(ords, [][]int{{0, 2}, {1}, {0}}) || !reflect.DeepEqual(counts, []int{3, 3, 2}) {
		t.Fatalf("multi-site ordinals %v counts %v", ords, counts)
	}

	// Later duplicates of a span identity replace earlier ones in place.
	conv := deltaConversation(3, "array")
	dup := proto.Clone(conv[1]).(*tracepb.ResourceSpans)
	dup.ScopeSpans[0].Spans[0].TraceId = conv[0].ScopeSpans[0].Spans[0].TraceId
	dup.ScopeSpans[0].Spans[0].SpanId = conv[0].ScopeSpans[0].Spans[0].SpanId
	plans = ftsPlanRefs(t, []*tracepb.ResourceSpans{conv[0], conv[2], dup, nil})
	if len(plans) != 2 || plans[0].span != ftsSpan(1) || plans[0].start != ftsBase+2 || len(plans[0].refs) != 2*2+3 || plans[1].span != ftsSpan(3) {
		t.Fatalf("duplicate replacement: %d plans", len(plans))
	}

	bad := deltaSpan(1)
	bad.ScopeSpans[0].Spans[0].SpanId = make([]byte, 8)
	_, err := planMessageSpans(t.Context(), 0, []*tracepb.ResourceSpans{bad})
	requireTyped[*AdmissionError](t, "zero span id", err)
	corrupt := deltaSpan(1, deltaKV("gen_ai.input.messages", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte(messageManifestMagic + "junk")}}))
	_, err = planMessageSpans(t.Context(), 0, []*tracepb.ResourceSpans{corrupt})
	requireTyped[*CorruptionError](t, "corrupt manifest", err)
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
	if p := plan(MessageFullTextQuery{Query: "x"}); p.mode != MessageMatchAll {
		t.Fatalf("default mode %q", p.mode)
	}
	base := plan(MessageFullTextQuery{Query: "alpha beta"}).digest
	same := []MessageFullTextQuery{{Query: "BETA alpha"}, {Query: "alpha beta", Mode: MessageMatchAll, Limit: 3}}
	for _, q := range same {
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
	if !bytes.Equal(plan(MessageFullTextQuery{Query: "beta alpha", Mode: MessageMatchPhrase}).digest, plan(MessageFullTextQuery{Query: "beta alpha", Mode: MessageMatchPhrase}).digest) ||
		bytes.Equal(plan(MessageFullTextQuery{Query: "beta alpha", Mode: MessageMatchPhrase}).digest, plan(MessageFullTextQuery{Query: "alpha beta", Mode: MessageMatchPhrase}).digest) {
		t.Fatal("phrase digest must bind term order")
	}
	for name, q := range map[string]MessageFullTextQuery{
		"empty":           {},
		"punctuation":     {Query: " ?! "},
		"bad mode":        {Query: "a", Mode: "fuzzy"},
		"too many terms":  {Query: strings.Repeat("w ", maxMessageQueryTerms+1)},
		"bad attribute":   {Query: "a", AttributeKey: "gen_ai.request.model"},
		"negative limit":  {Query: "a", Limit: -1},
		"bad predicate":   {Query: "a", Predicates: []Attribute{{"event", "k", "string", "v"}}},
		"inverted window": {Query: "a", TimeWindow: &TimeRange{Start: 3, End: 2}},
		"long role":       {Query: "a", Role: strings.Repeat("r", maxMessageRoleBytes+1)},
	} {
		_, err := newMessageQueryPlan(op, q, 7)
		requireTyped[*ConfigError](t, name, err)
	}
}

func TestMessagePlanMatch(t *testing.T) {
	t.Parallel()
	cand := func() *messageCandidate {
		return &messageCandidate{postings: map[string][]int{"new": {0, 7}, "york": {1, 9}, "city": {2}}}
	}
	for _, c := range []struct {
		q         MessageFullTextQuery
		ok        bool
		matched   []string
		positions []int
	}{
		{MessageFullTextQuery{Query: "new york city", Mode: MessageMatchPhrase}, true, []string{"new", "york", "city"}, []int{0}},
		{MessageFullTextQuery{Query: "york new", Mode: MessageMatchPhrase}, false, nil, nil},
		{MessageFullTextQuery{Query: "new city", Mode: MessageMatchPhrase}, false, nil, nil},
		{MessageFullTextQuery{Query: "york york", Mode: MessageMatchPhrase}, false, nil, nil},
		{MessageFullTextQuery{Query: "new city"}, true, []string{"new", "city"}, []int{0, 2, 7}},
		{MessageFullTextQuery{Query: "new boston"}, false, nil, nil},
		{MessageFullTextQuery{Query: "boston city", Mode: MessageMatchAny}, true, []string{"city"}, []int{2}},
		{MessageFullTextQuery{Query: "boston", Mode: MessageMatchAny}, false, nil, nil},
	} {
		p, err := newMessageQueryPlan("test", c.q, 1)
		if err != nil {
			t.Fatal(err)
		}
		x := cand()
		if ok := p.match(x); ok != c.ok || (ok && (!reflect.DeepEqual(x.matched, c.matched) || !reflect.DeepEqual(x.positions, c.positions))) {
			t.Fatalf("%+v: ok=%v matched=%v positions=%v", c.q, ok, x.matched, x.positions)
		}
	}
}

func TestMessageContinuation(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{3}, continuationKeyBytes)
	b := qaBackend("time")
	digest := bytes.Repeat([]byte{1}, 32)
	hash := [32]byte{7}
	last := append(messageRefSuffix(qaID(1, 16), qaID(2, 8), 42, "gen_ai.input.messages", "time"), hash[:]).Pack()
	token := signContinuationKind(secret, messageContinuationKind, b.cfg.Run, b.cfg.Tenant, "time", digest, last)
	c, err := parseContinuationKind(token, messageContinuationKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.authenticate(secret, b, digest); err != nil {
		t.Fatal(err)
	}
	r, err := parseMessageResume("time", c.last)
	if err != nil || r.hash != hash || !bytes.Equal(messageSpanRefSpace(subspace.Sub("x"), 0, hash).Pack(r.suffix), messageSpanRefSpace(subspace.Sub("x"), 0, hash).Pack(messageRefSuffix(qaID(1, 16), qaID(2, 8), 42, "gen_ai.input.messages", "time"))) {
		t.Fatalf("resume %+v %v", r, err)
	}
	// Tokens of one query family never resume another.
	_, err = parseContinuationKind(token, continuationKind)
	requireTyped[*ConfigError](t, "message token as same-span", err)
	_, err = parseContinuationKind(signContinuation(secret, b.cfg.Run, b.cfg.Tenant, "time", digest, last), messageContinuationKind)
	requireTyped[*ConfigError](t, "same-span token as message", err)

	check := func(tok, key, dig []byte) error {
		c, err := parseContinuationKind(tok, messageContinuationKind)
		if err != nil {
			return err
		}
		return c.authenticate(key, b, dig)
	}
	for i := range token {
		mutated := append([]byte(nil), token...)
		mutated[i] ^= 0x10
		err := check(mutated, secret, digest)
		if err == nil {
			t.Fatalf("flip at byte %d accepted", i)
		}
		requireConfigOrCorruption(t, fmt.Sprintf("flip %d", i), err)
	}
	requireTyped[*CorruptionError](t, "wrong secret", check(token, bytes.Repeat([]byte{4}, continuationKeyBytes), digest))
	requireTyped[*CorruptionError](t, "absent secret", check(token, nil, digest))
	requireTyped[*ConfigError](t, "wrong query", check(token, secret, bytes.Repeat([]byte{2}, 32)))
	requireTyped[*ConfigError](t, "wrong tenant", check(signContinuationKind(secret, messageContinuationKind, b.cfg.Run, b.cfg.Tenant+1, "time", digest, last), secret, digest))
	requireTyped[*ConfigError](t, "wrong run", check(signContinuationKind(secret, messageContinuationKind, "qa_run_other", b.cfg.Tenant, "time", digest, last), secret, digest))
	requireTyped[*ConfigError](t, "wrong order", check(signContinuationKind(secret, messageContinuationKind, b.cfg.Run, b.cfg.Tenant, "trace", digest, last), secret, digest))

	for name, c := range map[string]struct {
		order string
		key   []byte
	}{
		"not a tuple":    {"time", []byte{0xff}},
		"trace key":      {"time", append(messageRefSuffix(qaID(1, 16), qaID(2, 8), 42, "gen_ai.input.messages", "trace"), hash[:]).Pack()},
		"short hash":     {"time", append(messageRefSuffix(qaID(1, 16), qaID(2, 8), 42, "gen_ai.input.messages", "time"), hash[:5]).Pack()},
		"bad attribute":  {"trace", append(messageRefSuffix(qaID(1, 16), qaID(2, 8), 42, "other", "trace"), hash[:]).Pack()},
		"positive start": {"time", tuple.Tuple{int64(5), reverseTrace(qaID(1, 16)), qaID(2, 8), "gen_ai.input.messages", hash[:]}.Pack()},
		"invalid trace":  {"trace", tuple.Tuple{"zz", qaID(2, 8), "gen_ai.input.messages", hash[:]}.Pack()},
	} {
		_, err := parseMessageResume(c.order, c.key)
		requireTyped[*CorruptionError](t, name, err)
	}
}

// ---------------------------------------------------------------------------
// Real FDB integration
// ---------------------------------------------------------------------------

func openFTSExporter(t *testing.T, ctx context.Context, cluster string, mutate func(*ExporterConfig)) *Exporter {
	t.Helper()
	return openTestExporter(t, ctx, cluster, func(c *ExporterConfig) {
		c.EnableMessageFullText = true
		if mutate != nil {
			mutate(c)
		}
	})
}

// ftsDrain follows continuations to exhaustion, checking the page contract.
func ftsDrain(t *testing.T, ctx context.Context, e *Exporter, q MessageFullTextQuery) ([]MessageSpanHit, int) {
	t.Helper()
	limit := q.Limit
	if limit == 0 {
		limit = e.Config().K
	}
	all := []MessageSpanHit{}
	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
		page, err := e.QueryMessageFullText(ctx, q)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", pages, q, err)
		}
		if page.Order != e.Config().Order || page.ReadVersion <= 0 || page.Hits == nil {
			t.Fatalf("page metadata: %+v", page)
		}
		if len(page.Hits) > limit || (len(page.Hits) == limit) != (page.Continuation != nil) {
			t.Fatalf("page %d: %d hits, limit %d, continuation=%v", pages, len(page.Hits), limit, page.Continuation != nil)
		}
		all = append(all, page.Hits...)
		if page.Continuation == nil {
			ftsRequireSorted(t, e.Config().Order, all)
			return all, pages
		}
		q.Continuation = page.Continuation
	}
}

func ftsLess(order string, a, b MessageSpanHit) bool {
	if order == "time" {
		if a.Start != b.Start {
			return a.Start > b.Start
		}
		if a.Trace != b.Trace {
			return a.Trace > b.Trace // reverseTrace ascending
		}
	} else if a.Trace != b.Trace {
		return a.Trace < b.Trace
	}
	if a.Span != b.Span {
		return a.Span < b.Span
	}
	if a.AttributeKey != b.AttributeKey {
		return a.AttributeKey < b.AttributeKey
	}
	return a.MessageHash < b.MessageHash
}

func ftsRequireSorted(t *testing.T, order string, hits []MessageSpanHit) {
	t.Helper()
	for i := 1; i < len(hits); i++ {
		if !ftsLess(order, hits[i-1], hits[i]) {
			t.Fatalf("%s hits %d and %d out of order or duplicated: %+v %+v", order, i-1, i, hits[i-1], hits[i])
		}
	}
}

// ftsTurns returns the deltaSpan turn of every hit, in hit order.
func ftsTurns(hits []MessageSpanHit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = int(h.Start - ftsBase)
	}
	return out
}

func ftsRange(lo, hi int, order string) []int {
	var out []int
	for k := lo; k <= hi; k++ {
		out = append(out, k)
	}
	if order == "time" {
		sort.Sort(sort.Reverse(sort.IntSlice(out)))
	}
	return out
}

func ftsCount(t *testing.T, ctx context.Context, e *Exporter, q MessageFullTextQuery) int {
	t.Helper()
	hits, _ := ftsDrain(t, ctx, e, q)
	return len(hits)
}

func ftsSet(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, key fdb.Key, value []byte) {
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

func ftsGet(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, key fdb.Key) []byte {
	t.Helper()
	var out []byte
	if err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
		var err error
		out, err = rc.Transaction().Get(key).Get()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// ftsBlob finds the decomposed blob whose extracted text is text.
func ftsBlob(t *testing.T, blobs []*MessageBlob, text string) *MessageBlob {
	t.Helper()
	for _, b := range blobs {
		if _, got, err := ExtractMessageText(b); err == nil && got == text {
			return b
		}
	}
	t.Fatalf("no blob with text %.40q", text)
	return nil
}

func TestMessageFullTextRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for _, c := range []struct{ form, order string }{{"array", "time"}, {"json", "time"}, {"array", "trace"}} {
		t.Run("conversation/"+c.form+"_"+c.order, func(t *testing.T) {
			t.Parallel()
			testMessageFTSConversation(t, cluster, c.form, c.order)
		})
	}
	t.Run("unicode_events_replacement", func(t *testing.T) { t.Parallel(); testMessageFTSReplacement(t, cluster) })
	t.Run("budgets", func(t *testing.T) { t.Parallel(); testMessageFTSBudget(t, cluster) })
	t.Run("corruption", func(t *testing.T) { t.Parallel(); testMessageFTSCorruption(t, cluster) })
	t.Run("trace_existential", func(t *testing.T) { t.Parallel(); testMessageFTSExistential(t, cluster) })
	t.Run("record_context_api", func(t *testing.T) { t.Parallel(); testMessageFTSRecordContext(t, cluster) })
}

func testMessageFTSConversation(t *testing.T, cluster, form, order string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Order = order })
	tenant, db, root := e.Config().Tenant, e.backend.db, e.backend.root
	input := deltaConversation(deltaTurns, form)
	before := cloneDeltaSpans(input)
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// 25 turns reference 725 messages, 53 of them unique; each is tokenized once.
	n := deltaTurns
	refs := n*(n+1) + 3*n
	if stats.Spans != int64(n) || stats.MessageDeltas.TotalMessages != int64(refs) || stats.MessageDeltas.UniqueMessages != int64(2*n+3) {
		t.Fatalf("export stats %+v", stats)
	}
	if f := stats.MessageFTS; f.IndexedBlobs != 2*n+3 || f.SkippedBlobs != 0 || f.SpanRefs != refs || f.TokenPostings == 0 || f.UniqueTokens == 0 || f.UniqueTokens > f.TokenPostings || f.Bytes == 0 || f.Transactions < 3 {
		t.Fatalf("first FTS stats %+v", f)
	}
	requireDeltaSpansEqual(t, input, before)
	postings := stats.MessageFTS.TokenPostings
	again, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if f := again.MessageFTS; f.IndexedBlobs != 0 || f.SkippedBlobs != 2*n+3 || f.TokenPostings != 0 || f.SpanRefs != refs {
		t.Fatalf("idempotent re-export FTS stats %+v", f)
	}
	for k, want := range before {
		got, err := e.LoadSpanOTLP(ctx, ftsTrace(k+1), ftsSpan(k+1))
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("turn %d", k+1), got, want)
	}
	census := func(name string) int { return len(deltaBlobRows(t, ctx, db, root.Sub(name, int64(tenant)))) }
	if got := []int{census(msgMetaSubspace), census(msgFTSSubspace), census(msgSpanSubspace), census(msgSpanRevSubspace)}; !reflect.DeepEqual(got, []int{2*n + 3, postings, refs, refs}) {
		t.Fatalf("FDB census meta/fts/span/rev %v, want %v", got, []int{2*n + 3, postings, refs, refs})
	}

	// Turn 3's prompt is in 23 inputs; only turn 3 introduced it.
	decomposed, blobs, _, err := DecomposeResourceSpans(ctx, input, tenant)
	if err != nil {
		t.Fatal(err)
	}
	prompt := MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase}
	hits, _ := ftsDrain(t, ctx, e, prompt)
	if !reflect.DeepEqual(ftsTurns(hits), ftsRange(3, n, order)) {
		t.Fatalf("phrase turns %v", ftsTurns(hits))
	}
	userHash := ftsBlob(t, blobs, deltaText("user", 3, 800)).Hash()
	for _, h := range hits {
		k := int(h.Start - ftsBase)
		m := deltaManifest(t, decomposed[k-1], "gen_ai.input.messages")
		if h.Trace != ftsTrace(k) || h.Span != ftsSpan(k) || h.AttributeKey != "gen_ai.input.messages" || h.Role != "user" || h.MessageHash != hex.EncodeToString(userHash[:]) ||
			!reflect.DeepEqual(h.TurnOrdinals, []int{4}) || !reflect.DeepEqual(h.MatchedTerms, []string{"user", "turn", "3"}) || !reflect.DeepEqual(h.Positions, []int{0}) {
			t.Fatalf("turn %d hit %+v", k, h)
		}
		if h.PrefixChain != hex.EncodeToString(m.PrefixChains()[4][:]) || h.ManifestChain != hex.EncodeToString(m.Chain[:]) || m.Hashes[4] != userHash {
			t.Fatalf("turn %d hit chains disagree with the manifest", k)
		}
	}
	first := prompt
	first.FirstTurnOnly = true
	if got, _ := ftsDrain(t, ctx, e, first); len(got) != 1 || got[0].Span != ftsSpan(3) || got[0].TurnOrdinals[0] != 4 {
		t.Fatalf("first-turn prompt: %+v", got)
	}
	reply := MessageFullTextQuery{Query: "Assistant Turn 3", Mode: MessageMatchPhrase, FirstTurnOnly: true}
	if got, _ := ftsDrain(t, ctx, e, reply); len(got) != 1 || got[0].Span != ftsSpan(3) || got[0].AttributeKey != "gen_ai.output.messages" || got[0].Role != "assistant" {
		t.Fatalf("first-turn reply: %+v", got)
	}
	reply.FirstTurnOnly = false
	if got := ftsCount(t, ctx, e, reply); got != 1+(n-3) {
		t.Fatalf("reply references %d", got)
	}

	userRefs, assistantRefs := n*(n+1)/2, n*(n+1)/2 // sum_k k inputs+outputs of each role
	for name, c := range map[string]struct {
		q    MessageFullTextQuery
		want int
	}{
		"non-adjacent phrase":   {MessageFullTextQuery{Query: "user 3", Mode: MessageMatchPhrase}, 0},
		"reversed phrase":       {MessageFullTextQuery{Query: "turn user", Mode: MessageMatchPhrase}, 0},
		"all terms":             {MessageFullTextQuery{Query: "user 3"}, userRefs},
		"every reference":       {MessageFullTextQuery{Query: "turn"}, refs},
		"absent term":           {MessageFullTextQuery{Query: "turn zeppelin"}, 0},
		"any term":              {MessageFullTextQuery{Query: "query_traces fetch_logs zeppelin", Mode: MessageMatchAny}, 2 * n},
		"any term wrong attr":   {MessageFullTextQuery{Query: "query_traces fetch_logs", Mode: MessageMatchAny, AttributeKey: "gen_ai.input.messages"}, 0},
		"tool attribute":        {MessageFullTextQuery{Query: "fetch_logs", AttributeKey: "gen_ai.tool.definitions"}, n},
		"type is structural":    {MessageFullTextQuery{Query: "function"}, 0},
		"system attribute":      {MessageFullTextQuery{Query: "system", AttributeKey: "gen_ai.system_instructions"}, n},
		"system first turn":     {MessageFullTextQuery{Query: "system", FirstTurnOnly: true}, n},
		"system in inputs":      {MessageFullTextQuery{Query: "system", AttributeKey: "gen_ai.input.messages"}, 0},
		"role user":             {MessageFullTextQuery{Query: "turn", Role: " USER "}, userRefs},
		"role assistant":        {MessageFullTextQuery{Query: "turn", Role: "assistant"}, assistantRefs},
		"role user first turn":  {MessageFullTextQuery{Query: "turn", Role: "user", FirstTurnOnly: true}, n},
		"role output":           {MessageFullTextQuery{Query: "turn", Role: "assistant", AttributeKey: "gen_ai.output.messages"}, n},
		"role absent":           {MessageFullTextQuery{Query: "turn", Role: "tool"}, 0},
		"same-span predicate":   {MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Predicates: []Attribute{{"span", "gen_ai.conversation.id", "string", "conv-283"}, {"resource", "service.name", "string", "agent"}}}, n - 2},
		"failing predicate":     {MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Predicates: []Attribute{{"span", "gen_ai.operation.name", "string", "embeddings"}}}, 0},
		"time window":           {MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, TimeWindow: &TimeRange{Start: ftsBase + 10, End: ftsBase + 13}}, 3},
		"empty window":          {MessageFullTextQuery{Query: "turn", TimeWindow: &TimeRange{Start: 5, End: 5}}, 0},
		"window before history": {MessageFullTextQuery{Query: "turn", TimeWindow: &TimeRange{Start: 0, End: ftsBase + 1}}, 0},
	} {
		if got := ftsCount(t, ctx, e, c.q); got != c.want {
			t.Fatalf("%s: %d hits, want %d", name, got, c.want)
		}
	}
	for _, role := range []string{"user", "assistant"} {
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Role: role, Limit: 50})
		for _, h := range hits {
			if h.Role != role {
				t.Fatalf("role %s filter returned %+v", role, h)
			}
		}
	}

	// Pagination at any page size equals one large page.
	whole, pages := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Limit: 1000})
	paged, pagesSmall := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Limit: 5})
	if pages != 1 || pagesSmall != 5 || !reflect.DeepEqual(paged, whole) {
		t.Fatalf("pagination: %d/%d pages, equal=%v", pages, pagesSmall, reflect.DeepEqual(paged, whole))
	}
	everything, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "turn", Limit: 33})
	if len(everything) != refs || e.backend.LastQueryWork().Candidates == 0 {
		t.Fatalf("paged everything: %d, work %+v", len(everything), e.backend.LastQueryWork())
	}

	// Continuations are signed and bound to the query, tenant and kind.
	q := MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Limit: 4}
	page, err := e.QueryMessageFullText(ctx, q)
	if err != nil || page.Continuation == nil {
		t.Fatalf("first page %+v %v", page, err)
	}
	resume := func(e *Exporter, q MessageFullTextQuery, token []byte) (MessageFullTextPage, error) {
		q.Continuation = token
		return e.QueryMessageFullText(ctx, q)
	}
	tampered := append([]byte(nil), page.Continuation...)
	tampered[len(tampered)/2] ^= 0x01
	_, err = resume(e, q, tampered)
	requireConfigOrCorruption(t, "tampered payload", err)
	macFlip := append([]byte(nil), page.Continuation...)
	macFlip[len(macFlip)-1] ^= 0x01
	_, err = resume(e, q, macFlip)
	requireTyped[*CorruptionError](t, "tampered MAC", err)
	_, err = resume(e, MessageFullTextQuery{Query: "user turn 4", Mode: MessageMatchPhrase, Limit: 4}, page.Continuation)
	requireTyped[*ConfigError](t, "wrong query", err)
	_, err = resume(e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase, Limit: 4, FirstTurnOnly: true}, page.Continuation)
	requireTyped[*ConfigError](t, "wrong first-turn filter", err)
	otherTenant := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Run, c.Tenant, c.Order = e.Config().Run, 12, order })
	_, err = resume(otherTenant, q, page.Continuation)
	requireTyped[*ConfigError](t, "wrong tenant", err)
	otherRun := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.Order = order })
	_, err = resume(otherRun, q, page.Continuation)
	requireTyped[*CorruptionError](t, "other run", err)
	if got := ftsCount(t, ctx, otherTenant, MessageFullTextQuery{Query: "turn"}); got != 0 {
		t.Fatalf("tenant isolation: %d hits", got)
	}
	same, err := e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.operation.name", "string", "chat"}}, Limit: 2})
	if err != nil || same.Continuation == nil {
		t.Fatalf("same-span page %+v %v", same, err)
	}
	_, err = resume(e, q, same.Continuation)
	requireTyped[*ConfigError](t, "same-span token", err)
	next, err := resume(e, q, page.Continuation)
	if err != nil || !reflect.DeepEqual(append(page.Hits, next.Hits...), whole[:8]) {
		t.Fatalf("resume after rejections: %+v %v", next.Hits, err)
	}
}

func testMessageFTSReplacement(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	tenant, db, root := e.Config().Tenant, e.backend.db, e.backend.root
	dessert := "Crème Brûlée ÜBER façade"
	msgs := func(form, role, text string) *commonpb.KeyValue {
		return deltaKV("gen_ai.input.messages", deltaMessages(form, []deltaMsg{{role, text}}))
	}
	event := deltaSpan(53)
	event.ScopeSpans[0].Spans[0].Events = []*tracepb.Span_Event{{Name: "gen_ai.client.inference.operation.details", Attributes: []*commonpb.KeyValue{deltaKV("gen_ai.output.messages", deltaMessages("json", []deltaMsg{{"Assistant", "event borne zebra"}}))}}}
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(50, msgs("array", "user", dessert)), deltaSpan(51, msgs("json", "user", dessert)), event}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"creme brulee", "CRÈME BRÛLÉE", "Über FAÇADE"} {
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: q, Mode: MessageMatchPhrase})
		if len(hits) != 2 || hits[0].MessageHash == hits[1].MessageHash || hits[0].Role != "user" || hits[1].Role != "user" {
			t.Fatalf("%q: %+v", q, hits)
		}
	}
	for q, want := range map[string]int{"brulee creme": 0, "user": 0, "text": 0} {
		if got := ftsCount(t, ctx, e, MessageFullTextQuery{Query: q, Mode: MessageMatchPhrase}); got != want {
			t.Fatalf("%q: %d hits, want %d", q, got, want)
		}
	}
	zebra, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "zebra", Role: "assistant"})
	if len(zebra) != 1 || zebra[0].Span != ftsSpan(53) || zebra[0].AttributeKey != "gen_ai.output.messages" {
		t.Fatalf("event message: %+v", zebra)
	}

	// Re-exporting a span replaces its references; the last duplicate wins.
	moved := deltaSpan(51, msgs("json", "user", dessert))
	moved.ScopeSpans[0].Spans[0].StartTimeUnixNano += 1000
	stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{
		deltaSpan(50, msgs("array", "user", "plain vanilla")),
		moved,
		deltaSpan(52, msgs("array", "user", "alpha bravo")),
		deltaSpan(52, msgs("array", "user", "charlie delta")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DuplicateSpans != 1 || stats.MessageFTS.IndexedBlobs != 3 || stats.MessageFTS.SkippedBlobs != 1 || stats.MessageFTS.SpanRefs != 3 {
		t.Fatalf("replacement stats %+v", stats)
	}
	hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "creme brulee", Mode: MessageMatchPhrase})
	if len(hits) != 1 || hits[0].Span != ftsSpan(51) || hits[0].Start != ftsBase+51+1000 {
		t.Fatalf("moved span: %+v", hits)
	}
	for q, want := range map[string]int{"vanilla": 1, "alpha": 0, "charlie delta": 1, "zebra": 1} {
		if got := ftsCount(t, ctx, e, MessageFullTextQuery{Query: q}); got != want {
			t.Fatalf("%q after replacement: %d hits, want %d", q, got, want)
		}
	}
	for _, name := range []string{msgSpanSubspace, msgSpanRevSubspace} {
		if n := len(deltaBlobRows(t, ctx, db, root.Sub(name, int64(tenant)))); n != 4 {
			t.Fatalf("%s holds %d rows after replacement, want 4", name, n)
		}
	}
	got, err := e.LoadSpanOTLP(ctx, ftsTrace(52), ftsSpan(52))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "last duplicate", got, deltaSpan(52, msgs("array", "user", "charlie delta")))
}

func testMessageFTSBudget(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	for name, mutate := range map[string]func(*ExporterConfig){
		"candidates": func(c *ExporterConfig) { c.MaxCandidates = 3 },
		"bytes":      func(c *ExporterConfig) { c.MaxBytes = 128 },
	} {
		e := openFTSExporter(t, ctx, cluster, mutate)
		if _, err := e.ExportResourceSpans(ctx, deltaConversation(5, "array")); err != nil {
			t.Fatal(err)
		}
		page, err := e.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn"})
		requireOtelBudget(t, err)
		if page.Candidates == 0 || page.Continuation != nil || e.backend.LastQueryWork().Candidates != page.Candidates {
			t.Fatalf("%s: budget failure must report work and carry no continuation: %+v", name, page)
		}
		_, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "turn"}}})
		requireOtelBudget(t, err)
		empty, err := e.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "zeppelin"})
		if err != nil || len(empty.Hits) != 0 || empty.Candidates != 0 {
			t.Fatalf("%s: absent term: %+v %v", name, empty, err)
		}
	}
	off := openTestExporter(t, ctx, cluster, nil)
	_, err := off.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "turn"})
	requireTyped[*ConfigError](t, "full text disabled", err)
	_, err = off.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "turn"}}})
	requireTyped[*ConfigError](t, "existential full text disabled", err)
}

func testMessageFTSCorruption(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	tenant, db, root := e.Config().Tenant, e.backend.db, e.backend.root
	input := deltaConversation(4, "array")
	if _, err := e.ExportResourceSpans(ctx, input); err != nil {
		t.Fatal(err)
	}
	_, blobs, _, err := DecomposeResourceSpans(ctx, input, tenant)
	if err != nil {
		t.Fatal(err)
	}
	b := ftsBlob(t, blobs, deltaText("user", 2, 800))
	h := b.Hash()
	q := MessageFullTextQuery{Query: "user turn 2", Mode: MessageMatchPhrase}
	healthy := func(label string) {
		t.Helper()
		if got := ftsCount(t, ctx, e, q); got != 3 {
			t.Fatalf("%s: %d hits, want 3", label, got)
		}
	}
	healthy("positive control")
	corrupted := func(label string) {
		t.Helper()
		_, err := e.QueryMessageFullText(ctx, q)
		requireTyped[*CorruptionError](t, label, err)
	}
	tamper := func(label string, key fdb.Key, value []byte) {
		t.Helper()
		original := ftsGet(t, ctx, db, key)
		if original == nil {
			t.Fatalf("%s: fixture row missing", label)
		}
		ftsSet(t, ctx, db, key, value)
		corrupted(label)
		ftsSet(t, ctx, db, key, original)
		healthy(label + " restored")
	}

	posting := messageFTSTerm(root, tenant, "user").Sub(h[:]).FDBKey()
	_, _, positions, err := decodeMessagePosting(ftsGet(t, ctx, db, posting))
	if err != nil || positions[0] != 0 {
		t.Fatalf("fixture posting %v %v", positions, err)
	}
	tamper("posting positions", posting, encodeMessagePosting(b.Encoding(), "user", []int{0, 1}))
	tamper("posting role", posting, encodeMessagePosting(b.Encoding(), "assistant", positions))
	tamper("posting bytes", posting, []byte{1, 2, 3})
	tamper("metadata token count", messageMetaKey(root, tenant, h), encodeMessageMeta(b.Encoding(), "user", 1))

	chunk := messageBlobSpace(root, tenant, h).Sub(int64(0)).FDBKey()
	flipped := append([]byte(nil), ftsGet(t, ctx, db, chunk)...)
	flipped[len(flipped)-1] ^= 1
	tamper("blob chunk", chunk, flipped)

	refs := deltaBlobRows(t, ctx, db, messageSpanRefSpace(root, tenant, h))
	if len(refs) != 3 {
		t.Fatalf("fixture has %d references", len(refs))
	}
	value := append([]byte(nil), refs[1].Value...)
	value[3] ^= 1
	tamper("span reference", refs[1].Key, value)

	// Missing metadata and a missing blob are explicit too.
	meta := messageMetaKey(root, tenant, h)
	original := ftsGet(t, ctx, db, meta)
	ftsSet(t, ctx, db, meta, nil)
	corrupted("missing metadata")
	ftsSet(t, ctx, db, meta, original)
	original = ftsGet(t, ctx, db, chunk)
	ftsSet(t, ctx, db, chunk, nil)
	corrupted("missing blob")
	ftsSet(t, ctx, db, chunk, original)
	healthy("final")
}

func testMessageFTSExistential(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	if _, err := e.ExportResourceSpans(ctx, deltaConversation(6, "array")); err != nil {
		t.Fatal(err)
	}
	eval := exKV("gen_ai.evaluation.name", exStr("relevance"))
	if _, err := e.ExportLogs(ctx, []LogRecord{{EventID: "eval-5", Timestamp: ftsBase + 100, TraceID: ftsTrace(5), Attributes: []*commonpb.KeyValue{eval}}}); err != nil {
		t.Fatal(err)
	}
	chat := Attribute{"span", "gen_ai.operation.name", "string", "chat"}
	prompt := MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase}
	traces := func(r TraceExistentialResult) []int {
		var out []int
		for _, h := range r.Hits {
			out = append(out, int(h.Latest-ftsBase))
		}
		return out
	}
	got, err := e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{chat}, MessageClauses: []MessageFullTextQuery{prompt}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(traces(got), []int{6, 5, 4, 3}) || got.Candidates == 0 {
		t.Fatalf("span+message existential: %v", traces(got))
	}
	for _, h := range got.Hits {
		k := int(h.Latest - ftsBase)
		mw := h.MessageWitnesses
		if h.Trace != ftsTrace(k) || len(h.Witnesses) != 1 || len(mw) != 1 || len(mw[0]) != 1 || mw[0][0].Span != ftsSpan(k) || mw[0][0].AttributeKey != "gen_ai.input.messages" {
			t.Fatalf("turn %d witnesses: %+v", k, h)
		}
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{chat}, LogClauses: []Attribute{{LogScopeRecord, "gen_ai.evaluation.name", "string", "relevance"}}, MessageClauses: []MessageFullTextQuery{prompt}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Trace != ftsTrace(5) || got.Hits[0].Latest != ftsBase+100 || len(got.Hits[0].LogWitnesses) != 1 || len(got.Hits[0].MessageWitnesses[0]) != 1 {
		t.Fatalf("span+log+message existential: %+v", got.Hits)
	}
	// Two message clauses may be satisfied by different references of one trace.
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{
		{Query: "user turn 2", Mode: MessageMatchPhrase},
		{Query: "assistant turn 4", Mode: MessageMatchPhrase},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(traces(got), []int{6, 5, 4}) {
		t.Fatalf("two message clauses: %v", traces(got))
	}
	if w := got.Hits[2].MessageWitnesses; len(got.Hits[2].Witnesses) != 0 || len(w) != 2 || w[0][0].AttributeKey != "gen_ai.input.messages" || w[1][0].AttributeKey != "gen_ai.output.messages" {
		t.Fatalf("turn 4 message witnesses: %+v", got.Hits[2])
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "assistant turn 2", Mode: MessageMatchPhrase, FirstTurnOnly: true}}, TimeWindow: &TimeRange{Start: 0, End: ftsBase + 6}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{2}) {
		t.Fatalf("first-turn existential: %v %v", traces(got), err)
	}
	got, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{MessageClauses: []MessageFullTextQuery{{Query: "user turn 2", Mode: MessageMatchPhrase, TimeWindow: &TimeRange{Start: ftsBase + 3, End: ftsBase + 10}}}, TimeWindow: &TimeRange{Start: 0, End: ftsBase + 5}})
	if err != nil || !reflect.DeepEqual(traces(got), []int{4, 3}) {
		t.Fatalf("intersected windows: %v %v", traces(got), err)
	}
	for name, q := range map[string]TraceExistentialQuery{
		"message limit":        {MessageClauses: []MessageFullTextQuery{{Query: "turn", Limit: 2}}},
		"message continuation": {MessageClauses: []MessageFullTextQuery{{Query: "turn", Continuation: []byte{1}}}},
		"message mode":         {MessageClauses: []MessageFullTextQuery{{Query: "turn", Mode: "regex"}}},
		"message no terms":     {MessageClauses: []MessageFullTextQuery{{Query: "--"}}},
	} {
		_, err := e.QueryTraceExistential(ctx, q)
		requireTyped[*ConfigError](t, name, err)
	}
}

func testMessageFTSRecordContext(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	b := openOtelFixture(t, ctx, cluster, DefaultConfig(), false)
	db, root := b.db, b.root.Sub("messages")
	input := deltaConversation(3, "json")
	out, blobs, _, err := DecomposeResourceSpans(ctx, input, 2)
	if err != nil {
		t.Fatal(err)
	}
	for pass := range 2 {
		err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
			if err := configureStoreContext(rc); err != nil {
				return err
			}
			persist, fts, err := PersistAndIndexMessageBlobs(ctx, rc, root, 2, blobs)
			if err != nil {
				return err
			}
			if pass == 0 && (persist.Written != 9 || fts.IndexedBlobs != 9 || fts.SkippedBlobs != 0) || pass == 1 && (persist.Present != 9 || fts.IndexedBlobs != 0 || fts.SkippedBlobs != 9) {
				return fmt.Errorf("pass %d: persist %+v fts %+v", pass, persist, fts)
			}
			refs, err := IndexMessageSpans(ctx, rc, root, 2, "trace", out)
			if err != nil {
				return err
			}
			if refs.SpanRefs != 21 {
				return fmt.Errorf("pass %d: span refs %+v", pass, refs)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(deltaBlobRows(t, ctx, db, root.Sub(msgSpanSubspace, int64(2)))); n != 21 {
		t.Fatalf("%d span references", n)
	}
	got, err := ReconstructResourceSpansDB(ctx, db, root, 2, out)
	if err != nil {
		t.Fatal(err)
	}
	requireDeltaSpansEqual(t, got, input)

	big := []*tracepb.ResourceSpans{deltaSpan(1, deltaKV("gen_ai.output.messages", deltaArray(
		&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(200_000, 11)}},
		&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: deltaRandom(200_000, 12)}},
	)))}
	_, bigBlobs, _, err := DecomposeResourceSpans(ctx, big, 2)
	if err != nil {
		t.Fatal(err)
	}
	err = deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
		_, _, err := PersistAndIndexMessageBlobs(ctx, rc, root, 2, bigBlobs)
		return err
	})
	requireTyped[*AdmissionError](t, "single-transaction budget", err)
	persist, fts, err := PersistAndIndexMessageBlobsDB(ctx, db, root, 2, bigBlobs)
	if err != nil || persist.Written != 2 || fts.IndexedBlobs != 2 || fts.TokenPostings != 0 {
		t.Fatalf("DB variant: %+v %+v %v", persist, fts, err)
	}

	_, _, err = PersistAndIndexMessageBlobsDB(ctx, nil, root, 2, blobs)
	requireTyped[*ConfigError](t, "nil database", err)
	_, err = IndexMessageSpansDB(ctx, nil, root, 2, "time", out)
	requireTyped[*ConfigError](t, "nil database spans", err)
	_, err = IndexMessageSpansDB(ctx, db, root, 2, "sideways", out)
	requireTyped[*ConfigError](t, "bad order", err)
	_, _, err = PersistAndIndexMessageBlobsDB(ctx, db, root, 3, blobs)
	requireTyped[*AdmissionError](t, "wrong tenant", err)
}
