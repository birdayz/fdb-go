package otel

// Storage-free GenAI message codec: digests and IDs, manifests, decomposition,
// text extraction and segmentation, part plans, writer and queue bookkeeping,
// packed span references, trace markers and reconstruction plans.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"fdb.dev/layers/otel/otelsql"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// Element encodings; part of the hash domain so equal bytes in different
	// encodings never alias.
	messageEncodingAny  = "otlp_any"
	messageEncodingJSON = "json"

	maxMessageBytes    = 16 << 20 // RFC-283 §5.2 proposed decoded object admission
	maxMessageManifest = 64_000   // encoded manifest bound, as otelsql.MaxAttributeValueBytes; admission refuses more

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
	maxIndexedTokenBytes = otelsql.MaxIndexedTokenBytes
	maxMessageTextDepth  = 10000 // proto.Unmarshal's default recursion limit
	maxMessageRoleBytes  = 1024

	// messageTxnMessages bounds group commits, amortizing repeated TEXT bunch mutations.
	// Shards isolate one exporter's writers, not those of different exporters.
	messageTxnMessages = 128
	messageWorkers     = 8                 // concurrent transactions per message persist call
	messageWriterLimit = messageTextShards // message writers per exporter
	// messageTextShards is the number of MESSAGES_TEXT shard groups per role.
	messageTextShards = otelsql.MessageTextShards
	// messageTextPipeline is the TEXT_INDEX_UPDATE pipeline size.
	messageTextPipeline = 128
)

// messageAttributeKeys are otelsql.MessageAttributeKeys: the GenAI content
// attributes by the code a span's message reference carries.
var messageAttributeKeys = otelsql.MessageAttributeKeys

// messageAttributeCode is key's code in messageAttributeKeys, or 0.
func messageAttributeCode(key string) int64 { return otelsql.MessageAttributeCode(key) }

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

// messageDigest is a message element's MESSAGES content_hash
// (otelsql.MessageContentHash): its identity in memory and in MESSAGES_HASH.
type messageDigest [otelsql.MessageHashBytes]byte

func messageDigestOf(encoding string, element []byte) messageDigest {
	var d messageDigest
	copy(d[:], otelsql.MessageContentHash(encoding, element))
	return d
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
	// spans; TraceMarkers the MESSAGE_TRACES rows saved.
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
func decomposeMessages(ctx context.Context, spans []*tracepb.ResourceSpans) (*messageDecomposition, error) {
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
				h := messageDigestOf(m.encoding(), e)
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
