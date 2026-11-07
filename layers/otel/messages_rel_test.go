package otel

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// relMessageStore is the exporter's message hook set (relMessageHooks in
// exporter_rel.go).
var _ interface {
	persist(context.Context, *messageDecomposition) (MessageStoreStats, error)
	refChunks([]messageUse) ([][]byte, error)
	reconstruct(context.Context, *recordlayer.FDBRecordStore, *tracepb.ResourceSpans, *queryWork) error
	close()
} = (*relMessageStore)(nil)

// TestPlanRelMessageParts: MESSAGES row part i holds raw chunk i and text
// segment i, part 0 the content hash (v2 domain) and the whole message's
// columns, and the Large flag marks multipart text.
func TestPlanRelMessageParts(t *testing.T) {
	t.Parallel()
	text := largeText(3 << 20)
	value := deltaKvlist(deltaKV("role", deltaStr("Tool")), deltaKV("content", deltaStr(text)))
	raw := ftsAnyWire(t, value)
	el := &messageElement{digest: messageDigestOf(messageEncodingAny, raw), encoding: messageEncodingAny, raw: raw}
	hash := otelsql.MessageContentHash(el.encoding, el.raw)
	p, err := planRelMessage(el, hash, true, messageSegmentTokens)
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
	var postings int64
	for i, r := range p.records {
		m := r.(*otelv1pb.MESSAGES)
		if m.GetPART() != int64(i) || m.GetROLE() != "tool" || m.GetCONTENT() != segs[i].text || m.GetTOKEN_OFFSET() != int64(segs[i].offset) {
			t.Fatalf("part %d fields", i)
		}
		if (m.CONTENT_HASH != nil) != (i == 0) || (m.PARTS != nil) != (i == 0) || (len(m.GetLARGE().GetValues()) == 1) != (i == 0) {
			t.Fatalf("part %d header fields", i)
		}
		postings += int64(segs[i].postings)
		rebuilt = append(rebuilt, m.GetRAW()...)
	}
	head := p.records[0].(*otelv1pb.MESSAGES)
	if !bytes.Equal(head.GetCONTENT_HASH(), hash) || head.GetPARTS() != int64(len(p.records)) || head.GetRAW_BYTES() != int64(len(raw)) || head.GetENCODING() != messageEncodingAny {
		t.Fatalf("part 0: %v", head)
	}
	if !bytes.Equal(rebuilt, raw) || p.raw != int64(len(raw)) || p.postings != postings {
		t.Fatalf("parts hold %d raw bytes of %d, %d postings of %d", len(rebuilt), len(raw), p.postings, postings)
	}
	setRelMessageID(p.records[0], messageID{35, 7})
	if head.GetWRITER() != 35 || head.GetSEQ() != 7 || head.GetSHARD() != 3 {
		t.Fatalf("assigned ID: %v", head)
	}

	small, err := planRelMessage(&messageElement{encoding: messageEncodingAny, raw: ftsAnyWire(t, deltaStr("hi"))}, hash, true, messageSegmentTokens)
	if err != nil || len(small.records) != 1 || small.records[0].(*otelv1pb.MESSAGES).LARGE != nil {
		t.Fatalf("small message: %v", err)
	}
	// Without full text, the parts carry raw bytes only: no TEXT entries.
	deltas, err := planRelMessage(el, hash, false, messageSegmentTokens)
	if err != nil || len(deltas.records) != (len(raw)+messagePartRawBytes-1)/messagePartRawBytes || deltas.postings != 0 {
		t.Fatalf("deltas plan: %d records, %v", len(deltas.records), err)
	}
	for _, r := range deltas.records {
		if m := r.(*otelv1pb.MESSAGES); m.ROLE != nil || m.CONTENT != nil || m.LARGE != nil {
			t.Fatalf("deltas part %d has text columns", m.GetPART())
		}
	}
}

// The content of MESSAGES_TEXT holds OverlongTokenPlaceholder for a token over
// the indexed bound, which the index does hold: postings count it, and
// UNINDEXED_TOKENS counts the replaced token.
func TestPlanRelMessageOverlongTokenPostings(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		text     string
		postings int64
	}{
		"plain":            {"alpha beta alpha", 2},
		"overlong":         {"alpha " + strings.Repeat("x", maxIndexedTokenBytes+1) + " beta", 3},
		"two overlong":     {strings.Repeat("y", maxIndexedTokenBytes+1) + " alpha " + strings.Repeat("x", maxIndexedTokenBytes+1), 2},
		"placeholder word": {"alpha " + otelsql.OverlongTokenPlaceholder + " " + strings.Repeat("x", maxIndexedTokenBytes+1), 2},
	} {
		raw := ftsAnyWire(t, deltaStr(c.text))
		p, err := planRelMessage(&messageElement{encoding: messageEncodingAny, raw: raw}, []byte("h"), true, messageSegmentTokens)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		head := p.records[0].(*otelv1pb.MESSAGES)
		n, err := distinctTextTokens(head.GetCONTENT())
		if err != nil {
			t.Fatal(err)
		}
		if p.postings != c.postings || n != c.postings || head.GetUNINDEXED_TOKENS() != p.unindexed {
			t.Fatalf("%s: %d postings (stored content has %d), want %d; unindexed %d/%d", name, p.postings, n, c.postings, head.GetUNINDEXED_TOKENS(), p.unindexed)
		}
	}
}

// Placeholder IDs chunk as the IDs a commit assigns, so a span admitted with
// placeholder references keeps its chunk count.
func TestRelRefChunksPlaceholderSizing(t *testing.T) {
	t.Parallel()
	els := make([]*messageElement, 3000)
	uses := make([]messageUse, len(els))
	for i := range els {
		els[i] = &messageElement{}
		uses[i] = messageUse{element: els[i], attribute: 1, ordinals: []int{i}, newest: i == len(els)-1}
	}
	m := &relMessageStore{}
	before, err := m.refChunks(uses)
	if err != nil {
		t.Fatal(err)
	}
	for i, el := range els {
		el.id = messageID{int64(i%16 + 1), int64(i + 1)}
	}
	after, err := m.refChunks(uses)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 2 || len(after) != len(before) {
		t.Fatalf("%d chunks with placeholders, %d with IDs", len(before), len(after))
	}
	var refs []otelsql.MessageRef
	for _, c := range after {
		if n := len(tuple.Tuple{c}.Pack()); n > otelsql.MaxRefChunkKeyBytes {
			t.Fatalf("chunk key element of %d bytes", n)
		}
		got, err := otelsql.UnpackRefChunk(c)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, got...)
	}
	if len(refs) != len(uses) || refs[0].Writer != 1 || refs[len(refs)-1].Seq != int64(len(els)) || !refs[len(refs)-1].Newest {
		t.Fatalf("%d refs round trip", len(refs))
	}
	if _, err := m.refChunks([]messageUse{{element: els[0], attribute: 9, ordinals: []int{0}}}); err == nil {
		t.Fatal("unknown attribute code packed")
	}
}

// TestRelMessageRefKey pins the message hit sort key (§3.2): the key is
// (start, trace, span, attribute key, writer, seq), ordered by index order
// (D3) then attribute key and message ID.
func TestRelMessageRefKey(t *testing.T) {
	t.Parallel()
	tr := func(n int) []byte { return qaBytes(t, qaID(n, 16)) }
	sp := func(n int) []byte { return qaBytes(t, qaID(n, 8)) }
	k := relMessageRefKey{42, tr(1), sp(2), "gen_ai.input.messages", messageID{3, 9}}
	got, err := parseRelMessageRefKey(k.pack())
	if err != nil || got.start != k.start || !bytes.Equal(got.trace, k.trace) || !bytes.Equal(got.span, k.span) || got.attr != k.attr || got.id != k.id {
		t.Fatalf("round trip %+v %v", got, err)
	}
	ordered := []relMessageRefKey{
		{43, tr(1), sp(2), "gen_ai.output.messages", messageID{1, 1}},
		{42, tr(2), sp(1), "gen_ai.input.messages", messageID{1, 1}},
		{42, tr(1), sp(3), "gen_ai.input.messages", messageID{1, 1}},
		{42, tr(1), sp(2), "gen_ai.input.messages", messageID{3, 9}},
		{42, tr(1), sp(2), "gen_ai.input.messages", messageID{3, 10}},
		{42, tr(1), sp(2), "gen_ai.input.messages", messageID{4, 1}},
		{42, tr(1), sp(2), "gen_ai.output.messages", messageID{1, 1}},
	}
	for i := range ordered {
		for j := range ordered {
			if ordered[i].before(ordered[j]) != (i < j) {
				t.Fatalf("key %d before %d = %v", i, j, ordered[i].before(ordered[j]))
			}
		}
	}
	for name, raw := range map[string][]byte{
		"v1 trace order": tuple.Tuple{qaID(1, 16), qaID(2, 8), "gen_ai.input.messages", int64(3), int64(9)}.Pack(),
		"zero seq":       relMessageRefKey{42, tr(1), sp(2), "gen_ai.input.messages", messageID{3, 0}}.pack(),
		"bad attribute":  relMessageRefKey{42, tr(1), sp(2), "gen_ai.request.model", messageID{3, 9}}.pack(),
		"short trace":    relMessageRefKey{42, sp(1), sp(2), "gen_ai.input.messages", messageID{3, 9}}.pack(),
		"zero span":      relMessageRefKey{42, tr(1), make([]byte, 8), "gen_ai.input.messages", messageID{3, 9}}.pack(),
		"negative start": relMessageRefKey{-1, tr(1), sp(2), "gen_ai.input.messages", messageID{3, 9}}.pack(),
		"garbage":        {0xff},
		"short":          tuple.Tuple{int64(1)}.Pack(),
	} {
		_, err := parseRelMessageRefKey(raw)
		requireTyped[*CorruptionError](t, name, err)
	}
}

// The plan digest binds the typed predicate set (D1), not its order, and
// every other part of the query; message attributes are refused as
// predicates.
func TestRelMessageQueryPlan(t *testing.T) {
	t.Parallel()
	const op = "message full-text query"
	a := Attribute{"span", "gen_ai.request.model", "string", "gpt-4o"}
	b := Attribute{"span", "cached", "bool", "true"}
	c := Attribute{"resource", "ratio", "double", "0.5"}
	plan := func(q MessageFullTextQuery) *relMessageQueryPlan {
		t.Helper()
		p, err := newRelMessageQueryPlan(op, q, 7)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		return p
	}
	base := plan(MessageFullTextQuery{Query: "user turn", Predicates: []Attribute{a, b, c}})
	if base.base.limit != 7 || len(base.attrs) != 3 || base.attrs[1].q.index != spansBoolAttrIndex || base.attrs[2].q.index != spansDoubleAttrIndex {
		t.Fatalf("plan %+v", base)
	}
	if !bytes.Equal(plan(MessageFullTextQuery{Query: "user turn", Predicates: []Attribute{c, a, b, a}}).digest, base.digest) {
		t.Fatal("predicate order or repetition changed the digest")
	}
	for name, q := range map[string]MessageFullTextQuery{
		"predicate":  {Query: "user turn", Predicates: []Attribute{a, b}},
		"no preds":   {Query: "user turn"},
		"terms":      {Query: "user turns", Predicates: []Attribute{a, b, c}},
		"mode":       {Query: "user turn", Mode: MessageMatchAny, Predicates: []Attribute{a, b, c}},
		"role":       {Query: "user turn", Role: "user", Predicates: []Attribute{a, b, c}},
		"first turn": {Query: "user turn", FirstTurnOnly: true, Predicates: []Attribute{a, b, c}},
		"attribute":  {Query: "user turn", AttributeKey: "gen_ai.input.messages", Predicates: []Attribute{a, b, c}},
		"window":     {Query: "user turn", TimeWindow: &TimeRange{Start: 1, End: 9}, Predicates: []Attribute{a, b, c}},
	} {
		if bytes.Equal(plan(q).digest, base.digest) {
			t.Fatalf("%s does not change the digest", name)
		}
	}
	many := make([]Attribute, maxQueryPredicates+1)
	for i := range many {
		many[i] = Attribute{"span", fmt.Sprintf("k%d", i), "string", "v"}
	}
	for name, q := range map[string]MessageFullTextQuery{
		"message attribute": {Query: "x", Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}},
		"bytes":             {Query: "x", Predicates: []Attribute{{"span", "k", "bytes", "AQI="}}},
		"non-canonical":     {Query: "x", Predicates: []Attribute{{"span", "k", "double", "0.50"}}},
		"log scope":         {Query: "x", Predicates: []Attribute{{LogScopeRecord, "k", "string", "v"}}},
		"event scope":       {Query: "x", Predicates: []Attribute{{"event", "k", "string", "v"}}},
		"too many":          {Query: "x", Predicates: many},
		"no terms":          {Query: "--"},
		"mode":              {Query: "x", Mode: "regex"},
	} {
		_, err := newRelMessageQueryPlan(op, q, 7)
		requireTyped[*ConfigError](t, name, err)
	}
}

// messagePK reads only MESSAGES primary keys of the store's shape.
func TestRelMessagePK(t *testing.T) {
	t.Parallel()
	md := relTemplateMetadata(t)
	pk, err := relKey(md, "t", messagesRecordType, int64(5), int64(6), int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if id, part, err := messagePK(md, "t", pk); err != nil || id != (messageID{5, 6}) || part != 2 {
		t.Fatalf("decode %v: %v %d %v", pk, id, part, err)
	}
	traces, err := relKey(md, "t", messageTracesRecordType, int64(5), int64(6), int64(2))
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]tuple.Tuple{
		"short":      pk[:len(pk)-1],
		"zero seq":   append(pk[:len(pk)-2:len(pk)-2], int64(0), int64(0)),
		"other type": traces,
		"string":     append(pk[:len(pk)-1:len(pk)-1], "0"),
	} {
		_, _, err := messagePK(md, "t", bad)
		requireTyped[*CorruptionError](t, name, err)
	}
	if _, err := relKey(md, "t", "NO_SUCH_TABLE"); err == nil {
		t.Fatal("unknown table keyed")
	}
}

// relTemplateMetadata is OTEL_V1's metadata as a tenant store's, bound to
// the compiled types, without a cluster.
func relTemplateMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(otelsql.TemplateDDL())
	if err != nil {
		t.Fatal(err)
	}
	p, err := tmpl.Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	md, err := recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// relKey is tableKey on metadata alone.
func relKey(md *recordlayer.RecordMetaData, op, name string, cols ...tuple.TupleElement) (tuple.Tuple, error) {
	rt := md.GetRecordType(name)
	if rt == nil {
		return nil, failf("ownership", op, "%s is not in the store's metadata", name)
	}
	if rt.PrimaryKeyHasRecordTypePrefix() {
		return append(tuple.Tuple{rt.GetRecordTypeKey()}, cols...), nil
	}
	return tuple.Tuple(cols), nil
}

// unpackKey yields space.Unpack's elements and errors for the keys of the
// marker and reference indexes, for every single-byte change of them, and for
// ints tuple.Unpack does not decode as int64.
func TestUnpackKeyMatchesTupleUnpack(t *testing.T) {
	t.Parallel()
	space := subspace.FromBytes([]byte{0x15, 0x07, 0x02, 'T', 0x00})
	keys := [][]byte{
		space.Pack(tuple.Tuple{[]byte{0, 1, 2, 0, 0xff}, int64(1) << 40, []byte{9, 0, 9, 9, 9, 9, 9, 9}, int64(0), []byte{0x15, 0x01, 0x05, 0x00}, []byte{1}, int64(-3)}),
		space.Pack(tuple.Tuple{int64(3), int64(1), int64(1 << 55), []byte("0123456789abcdef"), int64(-1)}),
		space.Pack(tuple.Tuple{int64(-1 << 50), []byte{}, int64(255), int64(-255), int64(1<<63 - 1), int64(-1 << 63)}),
		space.Pack(tuple.Tuple{"text", 1.5, nil, tuple.Tuple{int64(1)}, true}),
		space.Pack(tuple.Tuple{uint64(1 << 63), int64(5)}),
		append(space.Bytes(), 0x15, 0x00, 0x16, 0x00, 0x00, 0x13, 0xff, 0x01, 0x41),
		{0x15, 0x08, 0x01, 0x00},
	}
	check := func(key []byte) {
		want, werr := space.Unpack(fdb.Key(key))
		got, gerr := unpackKey(space, key, make([]keyElem, 0, 4))
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%x: unpackKey error %v, Unpack error %v", key, gerr, werr)
		}
		if werr != nil {
			return
		}
		if len(got) != len(want) {
			t.Fatalf("%x: %d elements, want %d", key, len(got), len(want))
		}
		for i, el := range want {
			v, isInt := got[i].int()
			b, isBytes := got[i].bytes()
			switch w := el.(type) {
			case int64:
				if !isInt || v != w {
					t.Fatalf("%x element %d: %+v, want int64 %d", key, i, got[i], w)
				}
			case []byte:
				if !isBytes || !bytes.Equal(b, w) {
					t.Fatalf("%x element %d: %+v, want bytes %x", key, i, got[i], w)
				}
			default:
				if isInt || isBytes || fmt.Sprint(got[i].other) != fmt.Sprint(w) {
					t.Fatalf("%x element %d: %+v, want %T %v", key, i, got[i], w, w)
				}
			}
		}
	}
	for _, key := range keys {
		check(key)
		for i := range key {
			for _, c := range []byte{0x00, 0x01, 0x02, 0x0c, 0x0d, 0x13, 0x14, 0x15, 0x1b, 0x1c, 0x1d, 0xff, key[i] + 1, key[i] - 1} {
				m := append([]byte(nil), key...)
				m[i] = c
				check(m)
			}
			check(key[:i])
		}
	}
}
