package otel

// Storage-free GenAI message codec: digests and IDs, manifests, decomposition,
// text extraction and segmentation, part plans, writer and queue bookkeeping,
// packed span references, trace markers and reconstruction plans.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/rivo/uniseg"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	messageHashDomain = "otel_msg_v1\x00"
	// messageHashBytes is the stored SHA-256 prefix of the dedup index.
	messageHashBytes = 16

	// Element encodings; part of the hash domain so equal bytes in different
	// encodings never alias.
	messageEncodingAny  = "otlp_any"
	messageEncodingJSON = "json"

	maxMessageBytes    = 16 << 20 // RFC-283 §5.2 proposed decoded object admission
	maxMessageManifest = 64_000   // the covering-projection cap in index.go

	messageManifestMagic   = "OTMD"
	messageManifestVersion = 2

	// Parts independently bound raw bytes and indexed text; Record Layer splits raw values.
	// Tail parts can share transactions; part 0 commits last to publish the complete message.
	messagePartRawBytes = 1 << 20
	// Token/byte caps keep position lists below the value limit; transaction-size errors
	// replan with one quarter as many distinct tokens, down to messageSegmentMinTokens.
	messageSegmentTokens    = 1024
	messageSegmentMinTokens = 16
	messageSegmentBytes     = 64 << 10
	// Consecutive segments share messageSegmentOverlap tokens, so every phrase
	// of up to maxMessageQueryTerms tokens lies wholly inside one segment.
	maxMessageQueryTerms  = 16
	messageSegmentOverlap = maxMessageQueryTerms - 1
	// Parts of one large message share a transaction while its approximate
	// size stays below messagePartTxnBytes.
	messagePartTxnBytes = 2 << 20

	// Tokens longer than maxIndexedTokenBytes are not indexed (their position
	// is kept): a token is part of an index key, which FDB caps at 10KB.
	maxIndexedTokenBytes = 1024
	maxMessageTextDepth  = 10000 // proto.Unmarshal's default recursion limit
	maxMessageRoleBytes  = 1024

	// messageTxnMessages bounds group commits, amortizing repeated TEXT bunch mutations.
	// Shards isolate writers within this backend, not across backends.
	messageTxnMessages = 128
	messageWorkers     = 8                 // concurrent transactions per message persist call
	messageWriterLimit = messageTextShards // message writers per backend
	// messageTextShards is the number of TEXT index groups per (tenant, role).
	messageTextShards = 16
	// messageTextPipeline is the TEXT_INDEX_UPDATE pipeline size.
	messageTextPipeline = 128

	messageTokenizerName = "otel_message_v1"
)

// Attribute codes packed in Envelope.ref_chunks.
var messageAttributeKeys = [...]string{1: "gen_ai.input.messages", 2: "gen_ai.output.messages", 3: "gen_ai.system_instructions", 4: "gen_ai.tool.definitions"}

func messageAttributeCode(key string) int64 {
	for i, k := range messageAttributeKeys {
		if i > 0 && k == key {
			return int64(i)
		}
	}
	return -1
}

// IsMessageDeltaAttribute reports whether key is a GenAI content attribute
// whose value the message layouts replace with a manifest.
func IsMessageDeltaAttribute(key string) bool { return messageAttributeCode(key) > 0 }

// Manifest layouts describe how the elements reassemble into the original
// attribute value.
const (
	messageLayoutNone  byte = iota // KeyValue.Value was nil
	messageLayoutValue             // the whole AnyValue is one element
	messageLayoutArray             // ArrayValue; each element is an AnyValue
	messageLayoutJSON              // StringValue holding a JSON array; elements are raw JSON
)

const messageLayoutCount = 4

type messageDigest [messageHashBytes]byte

func messageHash(tenant int, encoding string, element []byte) messageDigest {
	h := sha256.New()
	h.Write([]byte(messageHashDomain))
	h.Write([]byte(strconv.Itoa(tenant)))
	h.Write([]byte{0})
	h.Write([]byte(encoding))
	h.Write([]byte{0})
	h.Write(element)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	var out messageDigest
	copy(out[:], sum[:])
	return out
}

// messageID names a stored message: the writer that stored it and its
// sequence number there. Both are positive.
type messageID struct {
	writer, seq int64
}

func (id messageID) valid() bool { return id.writer > 0 && id.seq > 0 }

func (id messageID) less(o messageID) bool {
	return id.writer < o.writer || (id.writer == o.writer && id.seq < o.seq)
}

func (id messageID) String() string {
	return strconv.FormatInt(id.writer, 10) + "." + strconv.FormatInt(id.seq, 10)
}

// MessageDeltaStats counts one export call's decomposition. Byte counts are
// protobuf sizes of attribute values and of the call's unique elements.
type MessageDeltaStats struct {
	TotalMessages    int64 `json:"total_messages"`
	UniqueMessages   int64 `json:"unique_messages"`
	ReusedMessages   int64 `json:"reused_messages"`
	RawMessageBytes  int64 `json:"raw_message_bytes"`
	StoredDeltaBytes int64 `json:"stored_delta_bytes"`
	SavedBytes       int64 `json:"saved_bytes"`
}

// MessageStoreStats counts committed message persistence, including TEXT Set
// replacements and AppendIfFits operands; write counts do not count postings.
type MessageStoreStats struct {
	Written         int64 `json:"written"`
	Present         int64 `json:"present"`
	Parts           int64 `json:"parts"`
	Postings        int64 `json:"postings"`
	UnindexedTokens int64 `json:"unindexed_tokens"`
	// References counts the (message, attribute) references of committed
	// spans; TraceMarkers the MessageTrace records saved.
	References   int64 `json:"references"`
	TraceMarkers int64 `json:"trace_markers"`
	Transactions int64 `json:"transactions"`
	Retries      int64 `json:"retries"`
	// Set/AppendIfFits operations, their keys and replacement/append operand
	// bytes, and bunch bytes read. Appends are already included in write counts.
	TextIndexWrites     int64 `json:"text_index_writes"`
	TextIndexKeyBytes   int64 `json:"text_index_key_bytes"`
	TextIndexValueBytes int64 `json:"text_index_value_bytes"`
	TextIndexReadBytes  int64 `json:"text_index_read_bytes"`
	// WallNs is the persist phase's wall time; TxnNs sums the message
	// transactions' durations, write-slot waits and retries included.
	WallNs int64 `json:"wall_ns"`
	TxnNs  int64 `json:"txn_ns"`
}

// Add sums every counter of o into s.
func (s *MessageStoreStats) Add(o MessageStoreStats) {
	s.Written += o.Written
	s.Present += o.Present
	s.Parts += o.Parts
	s.Postings += o.Postings
	s.UnindexedTokens += o.UnindexedTokens
	s.References += o.References
	s.TraceMarkers += o.TraceMarkers
	s.Transactions += o.Transactions
	s.Retries += o.Retries
	s.TextIndexWrites += o.TextIndexWrites
	s.TextIndexKeyBytes += o.TextIndexKeyBytes
	s.TextIndexValueBytes += o.TextIndexValueBytes
	s.TextIndexReadBytes += o.TextIndexReadBytes
	s.WallNs += o.WallNs
	s.TxnNs += o.TxnNs
}

// ---------------------------------------------------------------------------
// Manifests
// ---------------------------------------------------------------------------

// messageManifest is the decoded replacement value of one attribute.
type messageManifest struct {
	layout byte
	ids    []messageID
	head   []byte
	tail   []byte
	seps   [][]byte // nil means every separator is ","
}

func (m *messageManifest) encoding() string {
	if m.layout == messageLayoutJSON {
		return messageEncodingJSON
	}
	return messageEncodingAny
}

func (m *messageManifest) encode() []byte {
	out := make([]byte, 0, 8+len(m.ids)*6+len(m.head)+len(m.tail))
	out = append(out, messageManifestMagic...)
	out = append(out, messageManifestVersion, m.layout)
	out = binary.AppendUvarint(out, uint64(len(m.ids)))
	for _, id := range m.ids {
		out = binary.AppendUvarint(out, uint64(id.writer))
		out = binary.AppendUvarint(out, uint64(id.seq))
	}
	if m.layout != messageLayoutJSON {
		return out
	}
	out = binary.AppendUvarint(out, uint64(len(m.head)))
	out = append(out, m.head...)
	out = binary.AppendUvarint(out, uint64(len(m.tail)))
	out = append(out, m.tail...)
	if m.seps == nil {
		return append(out, 0)
	}
	out = append(out, 1)
	for _, s := range m.seps {
		out = binary.AppendUvarint(out, uint64(len(s)))
		out = append(out, s...)
	}
	return out
}

type byteReader struct {
	b   []byte
	bad bool
}

func (r *byteReader) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *byteReader) uvarint(max uint64) int {
	if r.bad {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 || v > max {
		r.bad = true
		return 0
	}
	r.b = r.b[n:]
	return int(v)
}

func (r *byteReader) byte1() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

// parseMessageManifest decodes a manifest attribute value.
func parseMessageManifest(v *commonpb.AnyValue) (*messageManifest, error) {
	wire, ok := v.GetValue().(*commonpb.AnyValue_BytesValue)
	if !ok {
		return nil, fail("corruption", "message manifest", "attribute is not a message manifest")
	}
	r := &byteReader{b: wire.BytesValue}
	if string(r.take(len(messageManifestMagic))) != messageManifestMagic || r.byte1() != messageManifestVersion {
		return nil, fail("corruption", "message manifest", "bad manifest magic or version")
	}
	m := &messageManifest{layout: r.byte1()}
	n := r.uvarint(maxMessageManifest / 2)
	if r.bad || m.layout >= messageLayoutCount {
		return nil, fail("corruption", "message manifest", "bad manifest layout")
	}
	m.ids = make([]messageID, n)
	for i := range m.ids {
		m.ids[i] = messageID{int64(r.uvarint(math.MaxInt64)), int64(r.uvarint(math.MaxInt64))}
		if !r.bad && !m.ids[i].valid() {
			r.bad = true
		}
	}
	if m.layout == messageLayoutJSON {
		m.head = r.take(r.uvarint(maxMessageManifest))
		m.tail = r.take(r.uvarint(maxMessageManifest))
		switch r.byte1() {
		case 0:
		case 1:
			m.seps = make([][]byte, max(n-1, 0))
			for i := range m.seps {
				m.seps[i] = r.take(r.uvarint(maxMessageManifest))
			}
		default:
			r.bad = true
		}
	}
	if r.bad || len(r.b) != 0 {
		return nil, fail("corruption", "message manifest", "truncated or trailing manifest bytes")
	}
	switch {
	case m.layout == messageLayoutNone && n != 0, m.layout == messageLayoutValue && n != 1, m.layout == messageLayoutJSON && n < 1:
		return nil, failf("corruption", "message manifest", "layout %d with %d elements", m.layout, n)
	}
	return m, nil
}

// splitJSONMessages returns the raw elements and framing of a JSON array, or
// ok=false when s is not a non-empty array that reassembles byte-for-byte.
func splitJSONMessages(s string) (elems []string, head, tail string, seps []string, ok bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, "", "", nil, false
	}
	prev := -1
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, "", "", nil, false
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		if start < 0 || start < prev || s[start:end] != string(raw) {
			return nil, "", "", nil, false
		}
		if prev < 0 {
			head = s[:start]
		} else {
			seps = append(seps, s[prev:start])
		}
		elems = append(elems, s[start:end])
		prev = end
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') || prev < 0 {
		return nil, "", "", nil, false
	}
	if strings.TrimLeft(s[dec.InputOffset():], " \t\r\n") != "" {
		return nil, "", "", nil, false
	}
	tail = s[prev:]
	var b strings.Builder
	b.WriteString(head)
	for i, e := range elems {
		if i > 0 {
			b.WriteString(seps[i-1])
		}
		b.WriteString(e)
	}
	b.WriteString(tail)
	return elems, head, tail, seps, b.String() == s
}

func marshalMessageValue(v *commonpb.AnyValue) ([]byte, error) {
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if err != nil {
		return nil, wrap("admission", "message element encode", err)
	}
	return wire, nil
}

// splitMessageValue chooses the layout for one attribute value. Array layout
// requires that dropping the shell loses nothing: no unknown fields there.
func splitMessageValue(v *commonpb.AnyValue) (*messageManifest, [][]byte, error) {
	if v == nil {
		return &messageManifest{layout: messageLayoutNone}, nil, nil
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_ArrayValue:
		if len(v.ProtoReflect().GetUnknown()) == 0 && (x.ArrayValue == nil || len(x.ArrayValue.ProtoReflect().GetUnknown()) == 0) {
			var values []*commonpb.AnyValue
			if x.ArrayValue != nil {
				values = x.ArrayValue.Values
			}
			elems := make([][]byte, 0, len(values))
			for _, e := range values {
				if e == nil {
					return nil, nil, fail("admission", "message element encode", "nil array element")
				}
				wire, err := marshalMessageValue(e)
				if err != nil {
					return nil, nil, err
				}
				elems = append(elems, wire)
			}
			return &messageManifest{layout: messageLayoutArray}, elems, nil
		}
	case *commonpb.AnyValue_StringValue:
		if len(v.ProtoReflect().GetUnknown()) == 0 {
			if parts, head, tail, seps, ok := splitJSONMessages(x.StringValue); ok {
				m := &messageManifest{layout: messageLayoutJSON, head: []byte(head), tail: []byte(tail)}
				for _, s := range seps {
					if s != "," {
						m.seps = make([][]byte, len(seps))
						for i, s := range seps {
							m.seps[i] = []byte(s)
						}
						break
					}
				}
				elems := make([][]byte, len(parts))
				for i, p := range parts {
					elems[i] = []byte(p)
				}
				return m, elems, nil
			}
		}
	}
	wire, err := marshalMessageValue(v)
	if err != nil {
		return nil, nil, err
	}
	return &messageManifest{layout: messageLayoutValue}, [][]byte{wire}, nil
}

// forEachMessageAttribute visits every content attribute of each span and its
// events, with the span it belongs to. A nil attribute marks every span's start.
func forEachMessageAttribute(rs *tracepb.ResourceSpans, fn func(*tracepb.Span, *commonpb.KeyValue) error) error {
	visit := func(sp *tracepb.Span, kvs []*commonpb.KeyValue) error {
		for _, kv := range kvs {
			if kv != nil && IsMessageDeltaAttribute(kv.Key) {
				if err := fn(sp, kv); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, ss := range rs.GetScopeSpans() {
		for _, sp := range ss.GetSpans() {
			if sp == nil {
				continue
			}
			if err := fn(sp, nil); err != nil {
				return err
			}
			if err := visit(sp, sp.GetAttributes()); err != nil {
				return err
			}
			for _, ev := range sp.GetEvents() {
				if err := visit(sp, ev.GetAttributes()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func canceled(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return wrap("canceled", op, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Decomposition
// ---------------------------------------------------------------------------

// messageElement is one encoded message element, unique within its call. Its
// ID is set when the call stores or finds it.
type messageElement struct {
	digest   messageDigest
	encoding string
	raw      []byte
	id       messageID
	trace    string            // of the first span referencing it
	written  bool              // stored by this call, not found present
	outcome  MessageStoreStats // committed per-message work, including fallback parts
}

// messageUse records positions in the first attribute value and whether the reference
// is that value’s newest element (the turn that introduced it).
type messageUse struct {
	element   *messageElement
	attribute int64
	ordinals  []int
	newest    bool
}

// messageSite is one decomposed attribute value.
type messageSite struct {
	kv       *commonpb.KeyValue
	manifest *messageManifest
	elements []*messageElement
}

type messageDecomposition struct {
	spans    []*tracepb.ResourceSpans
	elements []*messageElement // unique, in first-reference order
	sites    []messageSite
	// uses maps a span key (trace hex + span hex) to its unique references in
	// attribute order; a later duplicate of a span identity replaces it.
	uses  map[string][]messageUse
	stats MessageDeltaStats
}

func spanKey(sp *tracepb.Span) string {
	return hex.EncodeToString(sp.TraceId) + hex.EncodeToString(sp.SpanId)
}

// messagePlaceholderID encodes at least as long as any real ID, so a span
// admitted with placeholder manifests stays admissible with the real ones.
var messagePlaceholderID = messageID{math.MaxInt64, math.MaxInt64}

// decomposeMessages clones spans with placeholder GenAI manifests and collects unique elements.
// encodeManifests installs IDs later; the input remains unchanged.
func decomposeMessages(ctx context.Context, spans []*tracepb.ResourceSpans, tenant int) (*messageDecomposition, error) {
	const op = "message decompose"
	d := &messageDecomposition{spans: make([]*tracepb.ResourceSpans, len(spans)), uses: map[string][]messageUse{}}
	byDigest := map[messageDigest]*messageElement{}
	for i, rs := range spans {
		if err := canceled(ctx, op); err != nil {
			return nil, err
		}
		if rs == nil {
			continue
		}
		clone := proto.Clone(rs).(*tracepb.ResourceSpans)
		var current *tracepb.Span
		var uses []messageUse
		seen := map[int64]bool{} // attribute codes of the current span
		flush := func() {
			if current != nil {
				d.uses[spanKey(current)] = uses
			}
			uses, seen = nil, map[int64]bool{}
		}
		err := forEachMessageAttribute(clone, func(sp *tracepb.Span, kv *commonpb.KeyValue) error {
			if kv == nil {
				flush()
				current = sp
				return nil
			}
			m, raws, err := splitMessageValue(kv.Value)
			if err != nil {
				return err
			}
			site := messageSite{kv: kv, manifest: m, elements: make([]*messageElement, len(raws))}
			code := messageAttributeCode(kv.Key)
			// The span's first value of an attribute describes its references.
			first := !seen[code]
			seen[code] = true
			position := map[*messageElement]int{}
			for j, e := range raws {
				if len(e) > maxMessageBytes {
					return failf("admission", op, "message element of %d bytes exceeds %d", len(e), maxMessageBytes)
				}
				h := messageHash(tenant, m.encoding(), e)
				el := byDigest[h]
				if el == nil {
					el = &messageElement{digest: h, encoding: m.encoding(), raw: e, trace: hex.EncodeToString(sp.TraceId)}
					byDigest[h] = el
					d.elements = append(d.elements, el)
					d.stats.StoredDeltaBytes += int64(len(e))
				}
				site.elements[j] = el
				d.stats.TotalMessages++
				if !first {
					continue
				}
				if i, ok := position[el]; ok {
					uses[i].ordinals = append(uses[i].ordinals, j)
				} else {
					position[el] = len(uses)
					uses = append(uses, messageUse{element: el, attribute: code, ordinals: []int{j}})
				}
			}
			if first && len(raws) > 0 {
				uses[position[site.elements[len(raws)-1]]].newest = true
			}
			if kv.Value != nil {
				d.stats.RawMessageBytes += int64(proto.Size(kv.Value))
			}
			d.sites = append(d.sites, site)
			return nil
		})
		if err != nil {
			return nil, err
		}
		flush()
		d.spans[i] = clone
	}
	if err := d.encodeManifests(true); err != nil {
		return nil, err
	}
	d.stats.UniqueMessages = int64(len(d.elements))
	d.stats.ReusedMessages = d.stats.TotalMessages - d.stats.UniqueMessages
	return d, nil
}

// encodeManifests replaces each decomposed attribute value by its manifest,
// with placeholder IDs or the elements' IDs.
func (d *messageDecomposition) encodeManifests(placeholder bool) error {
	const op = "message decompose"
	var manifestBytes int64
	for _, s := range d.sites {
		s.manifest.ids = make([]messageID, len(s.elements))
		for i, el := range s.elements {
			switch {
			case placeholder:
				s.manifest.ids[i] = messagePlaceholderID
			case !el.id.valid():
				return fail("operation", op, "message element has no stored ID")
			default:
				s.manifest.ids[i] = el.id
			}
		}
		wire := s.manifest.encode()
		if len(wire) > maxMessageManifest {
			return failf("admission", op, "%s manifest of %d bytes exceeds %d", s.kv.Key, len(wire), maxMessageManifest)
		}
		s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: wire}}
		manifestBytes += int64(proto.Size(s.kv.Value))
	}
	if !placeholder {
		var raw int64
		for _, el := range d.elements {
			raw += int64(len(el.raw))
		}
		d.stats.StoredDeltaBytes = raw + manifestBytes
		d.stats.SavedBytes = d.stats.RawMessageBytes - d.stats.StoredDeltaBytes
	}
	return nil
}

// ---------------------------------------------------------------------------
// Text extraction and segmentation
// ---------------------------------------------------------------------------

func normalizeMessageRole(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// messageStructuralKey names map fields whose values are metadata, not text.
func messageStructuralKey(k string) bool { return k == "role" || k == "type" }

// extractMessageText normalizes the top-level role and joins searchable strings in document order.
// It excludes role/type values and yields the same result for either element encoding.
func extractMessageText(encoding string, raw []byte) (role string, text string, err error) {
	const op = "message text"
	switch encoding {
	case messageEncodingAny:
		v := &commonpb.AnyValue{}
		if err := proto.Unmarshal(raw, v); err != nil {
			return "", "", typedError("corruption", op, "message element decode", err)
		}
		for _, kv := range v.GetKvlistValue().GetValues() {
			if kv.GetKey() != "role" {
				continue
			}
			if s, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
				role = normalizeMessageRole(s.StringValue)
				break
			}
		}
		parts, err := appendAnyText(nil, v, 0)
		if err != nil {
			return "", "", err
		}
		return role, strings.Join(parts, "\n"), nil
	case messageEncodingJSON:
		// A token walk keeps object fields in document order, as Kvlists are.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		w := &jsonTextWalker{dec: dec}
		tok, err := w.next()
		if err != nil {
			return "", "", err
		}
		role, _, err := w.value(tok, 0, true, true)
		if err != nil {
			return "", "", err
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return "", "", fail("corruption", op, "trailing data after JSON message")
		}
		return role, strings.Join(w.parts, "\n"), nil
	default:
		return "", "", failf("corruption", op, "unknown message encoding %q", encoding)
	}
}

func appendAnyText(parts []string, v *commonpb.AnyValue, depth int) ([]string, error) {
	if depth > maxMessageTextDepth {
		return nil, failf("admission", "message text", "message nests deeper than %d", maxMessageTextDepth)
	}
	var err error
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		parts = append(parts, x.StringValue)
	case *commonpb.AnyValue_ArrayValue:
		for _, e := range x.ArrayValue.GetValues() {
			if parts, err = appendAnyText(parts, e, depth+1); err != nil {
				return nil, err
			}
		}
	case *commonpb.AnyValue_KvlistValue:
		for _, kv := range x.KvlistValue.GetValues() {
			if messageStructuralKey(kv.GetKey()) {
				continue
			}
			if parts, err = appendAnyText(parts, kv.GetValue(), depth+1); err != nil {
				return nil, err
			}
		}
	}
	return parts, nil
}

type jsonTextWalker struct {
	dec   *json.Decoder
	parts []string
}

func (w *jsonTextWalker) next() (json.Token, error) {
	tok, err := w.dec.Token()
	if err != nil {
		return nil, typedError("corruption", "message text", "JSON message decode", err)
	}
	return tok, nil
}

// value consumes the JSON value that starts with tok, collecting its strings
// when emit is set. At the top level it also returns the first string role.
func (w *jsonTextWalker) value(tok json.Token, depth int, emit, top bool) (string, bool, error) {
	const op = "message text"
	if depth > maxMessageTextDepth {
		return "", false, failf("admission", op, "message nests deeper than %d", maxMessageTextDepth)
	}
	role, found := "", false
	switch t := tok.(type) {
	case string:
		if emit {
			w.parts = append(w.parts, t)
		}
		return "", false, nil
	case json.Delim:
		var closing json.Delim
		switch t {
		case '[':
			closing = ']'
			for w.dec.More() {
				item, err := w.next()
				if err != nil {
					return "", false, err
				}
				if _, _, err := w.value(item, depth+1, emit, false); err != nil {
					return "", false, err
				}
			}
		case '{':
			closing = '}'
			for w.dec.More() {
				kt, err := w.next()
				if err != nil {
					return "", false, err
				}
				key, ok := kt.(string)
				if !ok {
					return "", false, fail("corruption", op, "JSON object key is not a string")
				}
				vt, err := w.next()
				if err != nil {
					return "", false, err
				}
				structural := messageStructuralKey(key)
				if top && key == "role" && !found {
					if s, ok := vt.(string); ok {
						role, found = normalizeMessageRole(s), true
					}
				}
				if _, _, err := w.value(vt, depth+1, emit && !structural, false); err != nil {
					return "", false, err
				}
			}
		default:
			return "", false, fail("corruption", op, "unexpected JSON delimiter")
		}
		end, err := w.next()
		if err != nil {
			return "", false, err
		}
		if end != closing {
			return "", false, fail("corruption", op, "unbalanced JSON message")
		}
	}
	// Numbers, booleans and null carry no text.
	return role, found, nil
}

// messageSegment holds bounded searchable-projection text, not necessarily original bytes.
// Offset counts original token positions (including skipped tokens); postings counts distinct indexed tokens.
type messageSegment struct {
	text     string
	offset   int
	postings int
}

// segmentMessageText bounds searchable projections by distinct tokens and bytes, preserving token positions.
// Segments share messageSegmentOverlap tokens; raw bytes stay separate, and overlong-token count is returned.
func segmentMessageText(text string, distinct int) ([]messageSegment, int, error) {
	const op = "message text segments"
	tok := messageTokenizer{}
	if len(text) > messageSegmentBytes {
		// Raw message bytes are retained separately. Bound every overlong
		// word, including words whose normalization produces a short token.
		var bounded strings.Builder
		state := -1
		for rest := text; len(rest) > 0; {
			var word string
			word, rest, state = uniseg.FirstWordInString(rest, state)
			ts, err := tok.TokenizeToList(word, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
			if err != nil {
				return nil, 0, typedError("config", op, "tokenizer", err)
			}
			switch {
			case len(ts) == 0:
				bounded.WriteByte(' ')
			case len(ts) == 1 && len(word) > maxIndexedTokenBytes+1:
				bounded.WriteByte(' ')
				if ts[0] == "" {
					bounded.WriteString(strings.Repeat("x", maxIndexedTokenBytes+1))
				} else {
					bounded.WriteString(ts[0])
				}
				bounded.WriteByte(' ')
			default:
				bounded.WriteString(word)
			}
		}
		text = bounded.String()
	}
	whole, err := tok.TokenizeToList(text, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
	if err != nil {
		return nil, 0, typedError("config", op, "tokenizer", err)
	}
	unindexed := 0
	set := map[string]bool{}
	for _, t := range whole {
		if t == "" {
			unindexed++
		} else {
			set[t] = true
		}
	}
	if len(whole) == 0 {
		return []messageSegment{{}}, 0, nil
	}
	if len(set) <= distinct && len(text) <= messageSegmentBytes {
		return []messageSegment{{text: text, postings: len(set)}}, unindexed, nil
	}
	// Byte spans of the tokens: the word segments of the whole text that yield
	// a token, as the default tokenizer's break iterator does.
	type span struct{ start, end int }
	spans := make([]span, 0, len(whole))
	state, pos := -1, 0
	for rest := text; len(rest) > 0; {
		var word string
		word, rest, state = uniseg.FirstWordInString(rest, state)
		ts, err := tok.TokenizeToList(word, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
		if err != nil {
			return nil, 0, typedError("config", op, "tokenizer", err)
		}
		if len(ts) > 1 {
			return nil, 0, fail("admission", op, "a word segment yields several tokens")
		}
		if len(ts) == 1 {
			spans = append(spans, span{pos, pos + len(word)})
		}
		pos += len(word)
	}
	if len(spans) != len(whole) {
		return nil, 0, fail("admission", op, "word segments disagree with the tokenizer")
	}
	var out []messageSegment
	for s := 0; ; {
		e, seen := s, map[string]bool{}
		for e < len(whole) {
			if t := whole[e]; t != "" && !seen[t] && len(seen) == distinct {
				break
			}
			if e > s && spans[e].end-spans[s].start > messageSegmentBytes {
				break
			}
			if whole[e] != "" {
				seen[whole[e]] = true
			}
			e++
		}
		if e-s <= messageSegmentOverlap && e < len(whole) {
			return nil, 0, fail("admission", op, "tokens too long to segment with the phrase overlap")
		}
		seg := text[spans[s].start:spans[e-1].end]
		if len(seg) > messageSegmentBytes {
			return nil, 0, fail("admission", op, "indexed segment exceeds byte limit")
		}
		got, err := tok.TokenizeToList(seg, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
		if err != nil {
			return nil, 0, typedError("config", op, "tokenizer", err)
		}
		if len(got) != e-s {
			return nil, 0, fail("admission", op, "segment does not tokenize to its tokens")
		}
		for i, t := range got {
			if t != whole[s+i] {
				return nil, 0, fail("admission", op, "segment does not tokenize to its tokens")
			}
		}
		out = append(out, messageSegment{text: seg, offset: s, postings: len(seen)})
		if e == len(whole) {
			return out, unindexed, nil
		}
		s = e - messageSegmentOverlap
	}
}

// ---------------------------------------------------------------------------
// Message records
// ---------------------------------------------------------------------------

// messagePlan is one element's Message records, part 0 (the header) first.
// Writer and seq are set when a transaction assigns the element its ID.
type messagePlan struct {
	element   *messageElement
	records   []proto.Message
	raw       int64 // raw bytes in the records
	postings  int64 // distinct (token, part) postings
	unindexed int64
}

// messageParts is one element's part layout: raw chunk i and text segment i
// form part i, and every part repeats the role.
type messageParts struct {
	role      string
	segments  []messageSegment
	chunks    [][]byte
	unindexed int64
}

// splitMessageParts cuts an element into raw chunks and, with fulltext, the
// segments of its searchable text.
func splitMessageParts(el *messageElement, fulltext bool, distinct int) (messageParts, error) {
	const op = "message records"
	var mp messageParts
	if fulltext {
		var text string
		var err error
		mp.role, text, err = extractMessageText(el.encoding, el.raw)
		if err != nil {
			return messageParts{}, err
		}
		if len(mp.role) > maxMessageRoleBytes {
			return messageParts{}, failf("admission", op, "message role of %d bytes exceeds %d", len(mp.role), maxMessageRoleBytes)
		}
		n := 0
		mp.segments, n, err = segmentMessageText(text, distinct)
		if err != nil {
			return messageParts{}, err
		}
		mp.unindexed = int64(n)
	}
	for rest := el.raw; len(rest) > 0; {
		k := min(len(rest), messagePartRawBytes)
		mp.chunks = append(mp.chunks, rest[:k:k])
		rest = rest[k:]
	}
	return mp, nil
}

// messageShard isolates TEXT maps among one backend’s writers, preserving append order.
// Writers from different backends may share a shard and conflict safely.
func messageShard(writer int64) int64 { return writer % messageTextShards }

// ---------------------------------------------------------------------------
// Writers
// ---------------------------------------------------------------------------

// messageWriter numbers the messages its holder stores. A writer is held by
// one transaction at a time, so its seqs are stored in increasing order.
type messageWriter struct {
	id   int64
	next int64 // the last seq handed out
}

// messageWriters limits boundary-bunch rewrites: fewer writers reach past
// the next writer’s first posting sooner.
type messageWriters struct {
	once   sync.Once
	slots  chan struct{}
	mu     sync.Mutex
	free   []*messageWriter
	shards [messageTextShards]bool // shards of this backend's writers
}

// nextWriterID is the first ID above every allocated one (last) in this
// backend's free shard.
func nextWriterID(last, shard int64) int64 {
	return last + 1 + ((shard-(last+1))%messageTextShards+messageTextShards)%messageTextShards
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// messageQueue groups small messages into size/count-bounded transactions on backend writers,
// amortizing read-version, digest, TEXT bunch and commit costs across waiting calls.
type messageQueue struct {
	mu      sync.Mutex
	pending []queuedMessage
	loops   int
	closed  bool
	workers sync.WaitGroup
}

type queuedMessage struct {
	plan *messagePlan
	sub  *messageSubmission
}

// messageSubmission is one call's queued messages.
type messageSubmission struct {
	left  int // queued messages not yet stored or dropped
	err   error
	stats MessageStoreStats
	done  chan struct{}
}

// close rejects new submissions and joins workers without canceling shared transactions.
func (q *messageQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.workers.Wait()
}

// take removes the next transaction's messages, skipping those of failed
// submissions; with none queued the loop ends.
func (q *messageQueue) take() []queuedMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queuedMessage
	var raw int64
	n := 0
	for ; n < len(q.pending) && len(out) < messageTxnMessages; n++ {
		m := q.pending[n]
		if m.sub.err != nil {
			q.pending[n] = queuedMessage{}
			q.settleLocked(m.sub)
			continue
		}
		if len(out) > 0 && raw+m.plan.raw > messagePartRawBytes {
			break
		}
		out = append(out, m)
		q.pending[n] = queuedMessage{}
		raw += m.plan.raw
	}
	q.pending = q.pending[n:]
	if len(q.pending) == 0 {
		q.pending = nil
	}
	if len(out) == 0 {
		q.loops--
	}
	return out
}

// settleLocked counts one of sub's messages as done; callers hold q.mu.
func (q *messageQueue) settleLocked(sub *messageSubmission) {
	if sub.left--; sub.left == 0 && sub.err == nil {
		close(sub.done)
	}
}

// finish charges per-message counters to their submissions; the first message’s
// submission receives shared transaction, retry, TEXT-work and time counters.
func (q *messageQueue) finish(batch []queuedMessage, stats MessageStoreStats, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	txn := stats
	txn.Written, txn.Present, txn.Parts, txn.Postings, txn.UnindexedTokens = 0, 0, 0, 0, 0
	for i, m := range batch {
		s := &m.sub.stats
		if i == 0 {
			s.Add(txn)
		}
		s.Parts += m.plan.element.outcome.Parts
		s.Postings += m.plan.element.outcome.Postings
		s.UnindexedTokens += m.plan.element.outcome.UnindexedTokens
		s.Written += m.plan.element.outcome.Written
		s.Present += m.plan.element.outcome.Present
		if err != nil {
			if m.sub.err == nil {
				m.sub.err = err
				close(m.sub.done)
			}
			m.sub.left--
			continue
		}
		q.settleLocked(m.sub)
	}
}

// runMessageJobs runs independent jobs on at most messageWorkers goroutines;
// the first failure cancels remaining work and is returned.
func runMessageJobs(ctx context.Context, op string, n int, job func(context.Context, int) error) error {
	switch n {
	case 0:
		return nil
	case 1:
		return job(ctx, 0)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		done     int
		next     atomic.Int64
		workers  sync.WaitGroup
	)
	for range min(messageWorkers, n) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n || workCtx.Err() != nil {
					return
				}
				err := job(workCtx, i)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					done++
				}
				mu.Unlock()
			}
		}()
	}
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}
	if done != n {
		return wrap("canceled", op, ctx.Err())
	}
	return nil
}

// parallelMessageWork runs CPU-only fn(i) for i in [0, n) on at most
// messageWorkers goroutines and returns the error of the lowest failing index.
func parallelMessageWork(n int, fn func(int) error) error {
	errs := make([]error, n)
	var next atomic.Int64
	var failed atomic.Bool
	var workers sync.WaitGroup
	for range min(messageWorkers, n) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if errs[i] = fn(i); errs[i] != nil {
					failed.Store(true)
				}
			}
		}()
	}
	workers.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Span back references
// ---------------------------------------------------------------------------

// messageRefChunkBytes bounds one envelope_refs_v1 value (FDB values are at
// most 100KB); a span with more references has several entries.
const messageRefChunkBytes = 16 << 10

// spanRef records a message’s positions in the span’s first attribute value
// and whether it is that value’s newest element.
type spanRef struct {
	id        messageID
	attribute int64
	newest    bool
	ordinals  []int
}

// spanRefFields is the number of tuple elements of one packed reference:
// (writer, seq, attribute, newest, (ordinals)).
const spanRefFields = 5

// packSpanRefs packs ordered fields into messageRefChunkBytes-bounded tuples.
// Placeholder IDs encode at least as long as real IDs, keeping preflight sizing conservative.
func packSpanRefs(uses []messageUse) [][]byte {
	var chunks [][]byte
	var cur []byte
	for _, u := range uses {
		id := u.element.id
		if !id.valid() {
			id = messagePlaceholderID
		}
		ords := make(tuple.Tuple, len(u.ordinals))
		for i, o := range u.ordinals {
			ords[i] = int64(o)
		}
		newest := int64(0)
		if u.newest {
			newest = 1
		}
		ref := tuple.Tuple{id.writer, id.seq, u.attribute, newest, ords}.Pack()
		if len(cur) > 0 && len(cur)+len(ref) > messageRefChunkBytes {
			chunks = append(chunks, cur)
			cur = nil
		}
		cur = append(cur, ref...)
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// unpackSpanRefs decodes one chunk of packSpanRefs.
func unpackSpanRefs(b []byte) ([]spanRef, error) {
	bad := fail("corruption", "message references", "malformed span reference chunk")
	t, err := tuple.Unpack(b)
	if err != nil || len(t)%spanRefFields != 0 {
		return nil, bad
	}
	out := make([]spanRef, 0, len(t)/spanRefFields)
	for i := 0; i < len(t); i += spanRefFields {
		w, wok := t[i].(int64)
		s, sok := t[i+1].(int64)
		a, aok := t[i+2].(int64)
		n, nok := t[i+3].(int64)
		ords, ook := t[i+4].(tuple.Tuple)
		r := spanRef{id: messageID{w, s}, attribute: a, newest: n == 1, ordinals: make([]int, len(ords))}
		if !wok || !sok || !aok || !nok || !ook || !r.id.valid() || a < 1 || a >= int64(len(messageAttributeKeys)) || (n != 0 && n != 1) || len(ords) == 0 {
			return nil, bad
		}
		for j, o := range ords {
			v, ok := o.(int64)
			if !ok || v < 0 {
				return nil, bad
			}
			r.ordinals[j] = int(v)
		}
		out = append(out, r)
	}
	return out, nil
}

// traceMarker is a MessageTrace record: message id occurs in trace.
type traceMarker struct {
	id    messageID
	trace string
}

// markerCache remembers the markers this backend knows to be committed; a run
// never deletes one. It holds at most markerCacheSize, forgetting the oldest.
type markerCache struct {
	mu   sync.Mutex
	set  map[traceMarker]struct{}
	ring []traceMarker
	next int
}

const markerCacheSize = 1 << 18

func (c *markerCache) has(k traceMarker) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.set[k]
	return ok
}

func (c *markerCache) add(ks []traceMarker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set == nil {
		c.set = make(map[traceMarker]struct{}, markerCacheSize)
		c.ring = make([]traceMarker, 0, markerCacheSize)
	}
	for _, k := range ks {
		if _, ok := c.set[k]; ok {
			continue
		}
		if len(c.ring) < markerCacheSize {
			c.ring = append(c.ring, k)
		} else {
			delete(c.set, c.ring[c.next])
			c.ring[c.next] = k
			c.next = (c.next + 1) % markerCacheSize
		}
		c.set[k] = struct{}{}
	}
}

// messageMarkerTxn bounds the markers checked and saved per transaction.
const messageMarkerTxn = 512

// inflightMessage is a message one export call of this backend is storing;
// other calls that need it wait for its ID instead of storing it again.
type inflightMessage struct {
	done chan struct{}
	id   messageID // valid once done, if the store succeeded
}

// ---------------------------------------------------------------------------
// Reconstruction
// ---------------------------------------------------------------------------

type storedSite struct {
	kv       *commonpb.KeyValue
	manifest *messageManifest
}

// reconstructPlan is the manifests of clones of decomposed spans and the
// messages they reference, in first-reference order.
type reconstructPlan struct {
	spans     []*tracepb.ResourceSpans
	sites     []storedSite
	refs      []messageID
	encodings map[messageID]string
}

func planReconstruct(ctx context.Context, spans []*tracepb.ResourceSpans) (*reconstructPlan, error) {
	const op = "message reconstruct"
	out := make([]*tracepb.ResourceSpans, len(spans))
	var sites []storedSite
	encodings := map[messageID]string{}
	var refs []messageID
	for i, rs := range spans {
		if err := canceled(ctx, op); err != nil {
			return nil, err
		}
		if rs == nil {
			continue
		}
		clone := proto.Clone(rs).(*tracepb.ResourceSpans)
		err := forEachMessageAttribute(clone, func(_ *tracepb.Span, kv *commonpb.KeyValue) error {
			if kv == nil {
				return nil
			}
			m, err := parseMessageManifest(kv.Value)
			if err != nil {
				return err
			}
			for _, id := range m.ids {
				if prev, ok := encodings[id]; ok {
					if prev != m.encoding() {
						return fail("corruption", op, "message referenced with two encodings")
					}
					continue
				}
				encodings[id] = m.encoding()
				refs = append(refs, id)
			}
			sites = append(sites, storedSite{kv, m})
			return nil
		})
		if err != nil {
			return nil, err
		}
		out[i] = clone
	}
	return &reconstructPlan{spans: out, sites: sites, refs: refs, encodings: encodings}, nil
}

// apply installs the original attribute values from the raw elements.
func (p *reconstructPlan) apply(raws map[messageID][]byte) ([]*tracepb.ResourceSpans, error) {
	const op = "message reconstruct"
	sites, out := p.sites, p.spans
	element := func(id messageID) (*commonpb.AnyValue, error) {
		v := &commonpb.AnyValue{}
		if err := proto.Unmarshal(raws[id], v); err != nil {
			return nil, typedError("corruption", op, "message element decode", err)
		}
		return v, nil
	}
	for _, s := range sites {
		m := s.manifest
		switch m.layout {
		case messageLayoutNone:
			s.kv.Value = nil
		case messageLayoutValue:
			v, err := element(m.ids[0])
			if err != nil {
				return nil, err
			}
			s.kv.Value = v
		case messageLayoutArray:
			arr := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(m.ids))}
			for i, id := range m.ids {
				v, err := element(id)
				if err != nil {
					return nil, err
				}
				arr.Values[i] = v
			}
			s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: arr}}
		case messageLayoutJSON:
			var b strings.Builder
			b.Write(m.head)
			for i, id := range m.ids {
				if i > 0 {
					if m.seps == nil {
						b.WriteByte(',')
					} else {
						b.Write(m.seps[i-1])
					}
				}
				b.Write(raws[id])
			}
			b.Write(m.tail)
			s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: b.String()}}
		}
	}
	return out, nil
}
