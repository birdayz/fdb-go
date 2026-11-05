package otel

// GenAI message storage on standard Record Layer records.
//
// Tenant-wide digest dedup replaces repeated GenAI message elements with manifests.
// Raw-byte/text-token ceilings plan parts; tails may share transactions, with the head published last.
//
// Monotonic (writer, seq) IDs make TEXT postings append; one backend isolates its writers by shard.
// Independent backends may share shards and conflict.
//
// fulltext_v2 indexes message text by tenant, role and writer shard. MessageTrace
// markers select traces; envelope_refs_v1 reads each trace's packed ref_chunks.
//
// Message records are immutable, shared by every span of the tenant that
// references them, and never collected.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
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

// messagePipelineSizer overlaps token-map reads for TEXT_INDEX_UPDATE only;
// other operations retain Java’s default pipeline sizing.
func messagePipelineSizer(op recordlayer.PipelineOperation) int {
	if op == recordlayer.PipelineOperationTextIndexUpdate {
		return messageTextPipeline
	}
	return recordlayer.DefaultPipelineSizer(op)
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

func messageDescriptor() (protoreflect.MessageDescriptor, error) {
	fd, err := recordFile(messagesFullText)
	if err != nil {
		return nil, err
	}
	return fd.Messages().ByName("Message"), nil
}

// planMessage pairs raw chunk i with text segment i and repeats the role per part.
// Part 0 also carries digest, encoding, raw length, part count and unindexed-token count.
func planMessage(tenant int, el *messageElement, fulltext bool, distinct int) (*messagePlan, error) {
	const op = "message records"
	d, err := messageDescriptor()
	if err != nil {
		return nil, err
	}
	p := &messagePlan{element: el}
	var role string
	var segments []messageSegment
	if fulltext {
		var text string
		role, text, err = extractMessageText(el.encoding, el.raw)
		if err != nil {
			return nil, err
		}
		if len(role) > maxMessageRoleBytes {
			return nil, failf("admission", op, "message role of %d bytes exceeds %d", len(role), maxMessageRoleBytes)
		}
		n := 0
		segments, n, err = segmentMessageText(text, distinct)
		if err != nil {
			return nil, err
		}
		p.unindexed = int64(n)
	}
	var chunks [][]byte
	for rest := el.raw; len(rest) > 0; {
		k := min(len(rest), messagePartRawBytes)
		chunks = append(chunks, rest[:k:k])
		rest = rest[k:]
	}
	parts := max(len(chunks), len(segments), 1)
	f := d.Fields()
	for i := range parts {
		m := dynamicpb.NewMessage(d)
		m.Set(f.ByName("tenant"), protoreflect.ValueOfInt64(int64(tenant)))
		m.Set(f.ByName("part"), protoreflect.ValueOfInt64(int64(i)))
		if fulltext {
			m.Set(f.ByName("role"), protoreflect.ValueOfString(role))
		}
		if i < len(segments) {
			s := segments[i]
			m.Set(f.ByName("text"), protoreflect.ValueOfString(s.text))
			if s.offset > 0 {
				m.Set(f.ByName("token_offset"), protoreflect.ValueOfInt64(int64(s.offset)))
			}
			p.postings += int64(s.postings)
		}
		if i < len(chunks) {
			m.Set(f.ByName("raw"), protoreflect.ValueOfBytes(chunks[i]))
			p.raw += int64(len(chunks[i]))
		}
		if i == 0 {
			m.Set(f.ByName("hash"), protoreflect.ValueOfBytes(el.digest[:]))
			m.Set(f.ByName("encoding"), protoreflect.ValueOfString(el.encoding))
			m.Set(f.ByName("raw_bytes"), protoreflect.ValueOfInt64(int64(len(el.raw))))
			m.Set(f.ByName("parts"), protoreflect.ValueOfInt64(int64(parts)))
			if p.unindexed > 0 {
				m.Set(f.ByName("unindexed_tokens"), protoreflect.ValueOfInt64(p.unindexed))
			}
			if fulltext && len(segments) > 1 {
				m.Mutable(f.ByName("large")).List().Append(protoreflect.ValueOfInt64(1))
			}
		}
		p.records = append(p.records, m)
	}
	return p, nil
}

func setMessageID(m proto.Message, id messageID) {
	r := m.ProtoReflect()
	f := r.Descriptor().Fields()
	r.Set(f.ByName("writer"), protoreflect.ValueOfInt64(id.writer))
	r.Set(f.ByName("seq"), protoreflect.ValueOfInt64(id.seq))
	r.Set(f.ByName("shard"), protoreflect.ValueOfInt64(messageShard(id.writer)))
}

// messageShard isolates TEXT maps among one backend’s writers, preserving append order.
// Writers from different backends may share a shard and conflict safely.
func messageShard(writer int64) int64 { return writer % messageTextShards }

func messageKey(tenant int, id messageID, part int) tuple.Tuple {
	return tuple.Tuple{int64(tenant), id.writer, id.seq, int64(part)}
}

// messageTimerStats reads the BunchedMap counters of one message transaction.
func messageTimerStats(t *recordlayer.StoreTimer, s *MessageStoreStats) {
	s.TextIndexWrites = t.GetCount(recordlayer.CountSaveIndexKey)
	s.TextIndexKeyBytes = t.GetCount(recordlayer.CountSaveIndexKeyBytes)
	s.TextIndexValueBytes = t.GetCount(recordlayer.CountSaveIndexValueBytes)
	s.TextIndexReadBytes = t.GetCount(recordlayer.CountLoadIndexKeyBytes) + t.GetCount(recordlayer.CountLoadIndexValueBytes)
}

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

// acquireWriter waits for a slot and reuses or registers a writer;
// the run-wide counter keeps IDs unique across handles.
func (b *FDBBackend) acquireWriter(ctx context.Context) (*messageWriter, error) {
	b.writers.once.Do(func() { b.writers.slots = make(chan struct{}, messageWriterLimit) })
	select {
	case b.writers.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, wrap("canceled", "message writer", ctx.Err())
	}
	w, err := b.takeWriter(ctx)
	if err != nil {
		<-b.writers.slots
	}
	return w, err
}

func (b *FDBBackend) takeWriter(ctx context.Context) (*messageWriter, error) {
	b.writers.mu.Lock()
	if n := len(b.writers.free); n > 0 {
		w := b.writers.free[n-1]
		b.writers.free = b.writers.free[:n-1]
		b.writers.mu.Unlock()
		return w, nil
	}
	// The writer slots bound the writers to messageTextShards, so a shard is free.
	shard := int64(0)
	for b.writers.shards[shard] {
		shard++
	}
	b.writers.shards[shard] = true
	b.writers.mu.Unlock()
	key := b.keys.messageWriters
	var id int64
	_, err := runTxnSlot(ctx, b.db, "message writer", func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureRecordTimeout(rc); err != nil {
			return nil, err
		}
		if err := b.probeMessageTxn("writer-before-write", nil); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		v, err := rc.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		last := int64(0)
		if len(v) > 0 {
			t, err := tuple.Unpack(v)
			ok := false
			if err == nil && len(t) == 1 {
				last, ok = t[0].(int64)
			}
			if !ok || last < 1 {
				return nil, fail("corruption", "message writer", "malformed writer counter")
			}
		}
		// The first ID above every allocated one in this backend's free shard.
		id = last + 1 + ((shard-(last+1))%messageTextShards+messageTextShards)%messageTextShards
		rc.Transaction().Set(key, tuple.Tuple{id}.Pack())
		return nil, nil
	})
	if err != nil {
		b.writers.mu.Lock()
		b.writers.shards[shard] = false
		b.writers.mu.Unlock()
		return nil, err
	}
	return &messageWriter{id: id}, nil
}

func (b *FDBBackend) releaseWriter(w *messageWriter) {
	b.writers.mu.Lock()
	b.writers.free = append(b.writers.free, w)
	b.writers.mu.Unlock()
	<-b.writers.slots
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// hashIndexRange is the dedup index range of one digest.
func hashIndexRange(space []byte, tenant int, digest messageDigest) (fdb.KeyRange, error) {
	return fdb.PrefixRange(append(append([]byte(nil), space...), tuple.Tuple{int64(tenant), digest[:]}.Pack()...))
}

// decodeHashEntry reads the message ID from a dedup index entry.
func decodeHashEntry(space []byte, kv fdb.KeyValue) (messageID, error) {
	t, err := tuple.Unpack(kv.Key[len(space):])
	var id messageID
	ok := err == nil && len(t) == 5
	var part int64
	if ok {
		id.writer, ok = t[2].(int64)
	}
	if ok {
		id.seq, ok = t[3].(int64)
	}
	if ok {
		part, ok = t[4].(int64)
	}
	if !ok || part != 0 || !id.valid() {
		return messageID{}, fail("corruption", "message dedup index", "malformed message hash index entry")
	}
	return id, nil
}

// readMessagePresence reads, in one snapshot transaction, which elements are
// already stored. It is a hint: writes recheck presence.
func (b *FDBBackend) readMessagePresence(ctx context.Context, tenant int, elements []*messageElement) (map[messageDigest]messageID, error) {
	const op = "message presence"
	var found map[messageDigest]messageID
	_, err := runTxnSlot(ctx, b.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
		found = map[messageDigest]messageID{}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		snap := rc.Transaction().Snapshot()
		hashSpace := store.IndexSubspace(b.md.GetIndex(messageHashIndexName)).Bytes()
		hashes := make([]fdb.RangeResult, len(elements))
		for i, el := range elements {
			r, err := hashIndexRange(hashSpace, tenant, el.digest)
			if err != nil {
				return nil, err
			}
			hashes[i] = snap.GetRange(r, fdb.RangeOptions{Limit: 1})
		}
		for i, el := range elements {
			rows, err := hashes[i].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(rows) > 0 {
				id, err := decodeHashEntry(hashSpace, rows[0])
				if err != nil {
					return nil, err
				}
				found[el.digest] = id
			}
		}
		return nil, nil
	})
	return found, err
}

// messageTxn is one message transaction's body; it runs with the store open
// and a writer held, and returns its counters.
type messageTxn func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error)

func (b *FDBBackend) probeMessageTxn(stage string, store *recordlayer.FDBRecordStore) error {
	if b.messageTxnHook != nil {
		return b.messageTxnHook(stage, store)
	}
	return nil
}

// runMessageTxn runs body in one transaction, counting retries and the
// committed attempt's BunchedMap work.
func (b *FDBBackend) runMessageTxn(ctx context.Context, stats *MessageStoreStats, mu *sync.Mutex, body messageTxn) error {
	const op = "message persist"
	var got MessageStoreStats
	var attempts int64
	started := time.Now()
	_, err := runTxnSlot(ctx, b.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
		attempts++
		timer := recordlayer.NewStoreTimer()
		rc.SetTimer(timer)
		if err := b.probeMessageTxn("message-before-write", nil); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		if got, err = body(store, store.IndexSubspace(b.md.GetIndex(messageHashIndexName)).Bytes()); err != nil {
			return nil, err
		}
		messageTimerStats(timer, &got)
		return nil, b.probeMessageTxn("message-before-commit", store)
	})
	mu.Lock()
	defer mu.Unlock()
	stats.Retries += max(attempts-1, 0)
	stats.TxnNs += time.Since(started).Nanoseconds()
	if err == nil {
		got.Transactions = 1
		stats.Add(got)
	}
	return err
}

// readHashes issues serializable dedup-index reads for plans' digests, so a
// concurrent writer of the same message conflicts with this transaction.
func readHashes(store *recordlayer.FDBRecordStore, hashSpace []byte, tenant int, plans []*messagePlan) ([]messageID, error) {
	pending := make([]fdb.RangeResult, len(plans))
	for i, p := range plans {
		r, err := hashIndexRange(hashSpace, tenant, p.element.digest)
		if err != nil {
			return nil, err
		}
		pending[i] = store.Context().Transaction().GetRange(r, fdb.RangeOptions{Limit: 1})
	}
	out := make([]messageID, len(plans))
	for i := range plans {
		rows, err := pending[i].GetSliceWithError()
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			if out[i], err = decodeHashEntry(hashSpace, rows[0]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// writeMessages sets IDs after storing absent elements via group commit or multipart tails-first writes.
// A committed part 0 therefore denotes a complete message.
func (b *FDBBackend) writeMessages(ctx context.Context, tenant int, plans []*messagePlan, fulltext bool) (MessageStoreStats, error) {
	const op = "message persist"
	// Queue a trace’s messages together to share rare-token bunch reads within one transaction.
	var large []*messagePlan
	var byTrace [][]*messagePlan
	index := map[string]int{}
	for _, p := range plans {
		if len(p.records) > 1 {
			large = append(large, p)
			continue
		}
		i, ok := index[p.element.trace]
		if !ok {
			i = len(byTrace)
			index[p.element.trace] = i
			byTrace = append(byTrace, nil)
		}
		byTrace[i] = append(byTrace[i], p)
	}
	var small []*messagePlan
	for _, unit := range byTrace {
		small = append(small, unit...)
	}
	var stats MessageStoreStats
	var smallErr error
	var wg sync.WaitGroup
	if len(small) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, smallErr = b.queue.store(ctx, b, tenant, small, fulltext)
		}()
	}
	var mu sync.Mutex
	largeStats := MessageStoreStats{}
	err := runMessageJobs(ctx, op, len(large), func(ctx context.Context, i int) error {
		w, err := b.acquireWriter(ctx)
		if err != nil {
			return err
		}
		defer b.releaseWriter(w)
		p := large[i]
		return b.writeWithFallback(ctx, tenant, w, []*messagePlan{p}, fulltext, &largeStats, &mu, func() error {
			return b.writeLargeMessage(ctx, tenant, w, p, &largeStats, &mu)
		})
	})
	wg.Wait()
	stats.Add(largeStats)
	if err == nil {
		err = smallErr
	}
	return stats, err
}

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

// store waits for queued plans to settle or ctx to end; shared transactions use ctx values
// without cancellation because they may contain other callers’ messages.
func (q *messageQueue) store(ctx context.Context, b *FDBBackend, tenant int, plans []*messagePlan, fulltext bool) (MessageStoreStats, error) {
	sub := &messageSubmission{left: len(plans), done: make(chan struct{})}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return MessageStoreStats{}, fail("operation", "message persist", "message queue is closed")
	}
	for _, p := range plans {
		q.pending = append(q.pending, queuedMessage{p, sub})
	}
	start := min(messageWriterLimit-q.loops, (len(q.pending)+messageTxnMessages-1)/messageTxnMessages)
	q.loops += max(start, 0)
	q.workers.Add(max(start, 0))
	q.mu.Unlock()
	loopCtx := context.WithoutCancel(ctx)
	for range start {
		go q.loop(loopCtx, b, tenant, fulltext)
	}
	select {
	case <-sub.done:
		q.mu.Lock()
		defer q.mu.Unlock()
		return sub.stats, sub.err
	case <-ctx.Done():
		err := wrap("canceled", "message persist", ctx.Err())
		q.mu.Lock()
		defer q.mu.Unlock()
		if sub.err == nil && sub.left > 0 {
			// The loops drop its messages still queued.
			sub.err = err
			close(sub.done)
		}
		return MessageStoreStats{}, err
	}
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

// loop releases its writer after each transaction so waiting large-message jobs can acquire it;
// messages queued meanwhile join the next transaction.
func (q *messageQueue) loop(ctx context.Context, b *FDBBackend, tenant int, fulltext bool) {
	defer q.workers.Done()
	for {
		w, err := b.acquireWriter(ctx)
		batch := q.take()
		if len(batch) == 0 {
			if err == nil {
				b.releaseWriter(w)
			}
			return
		}
		if err != nil {
			q.finish(batch, MessageStoreStats{}, err)
			continue
		}
		plans := make([]*messagePlan, len(batch))
		for i, m := range batch {
			plans[i] = m.plan
		}
		var mu sync.Mutex
		var stats MessageStoreStats
		err = b.writeWithFallback(ctx, tenant, w, plans, fulltext, &stats, &mu, func() error {
			return b.writeSmallMessages(ctx, tenant, w, plans, &stats, &mu)
		})
		b.releaseWriter(w)
		q.finish(batch, stats, err)
	}
}

// writeWithFallback runs write and, while it fails with a transaction over
// FDB's size limit, stores plans again cut into smaller segments.
func (b *FDBBackend) writeWithFallback(ctx context.Context, tenant int, w *messageWriter, plans []*messagePlan, fulltext bool, stats *MessageStoreStats, mu *sync.Mutex, write func() error) error {
	err := write()
	for distinct := messageSegmentTokens / 4; fulltext && distinct >= messageSegmentMinTokens && isTransactionTooLarge(err); distinct /= 4 {
		for _, p := range plans {
			if p, err = planMessage(tenant, p.element, true, distinct); err != nil {
				return err
			}
			if err = b.writeLargeMessage(ctx, tenant, w, p, stats, mu); err != nil {
				break
			}
		}
	}
	return err
}

// isTransactionTooLarge reports FDB's transaction_too_large (2101).
func isTransactionTooLarge(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == 2101
}

// writeSmallMessages saves single-part messages in one transaction, numbering
// the absent ones in w.
func (b *FDBBackend) writeSmallMessages(ctx context.Context, tenant int, w *messageWriter, plans []*messagePlan, stats *MessageStoreStats, mu *sync.Mutex) error {
	const op = "message persist"
	// Failed in-flight claims can put the same digest in multiple submissions.
	original := plans
	unique := make([]*messagePlan, 0, len(plans))
	byDigest := make(map[messageDigest]*messagePlan, len(plans))
	for _, p := range plans {
		if byDigest[p.element.digest] == nil {
			byDigest[p.element.digest] = p
			unique = append(unique, p)
		}
	}
	plans = unique
	base := w.next
	// A committed-but-unknown attempt may use every ID even when the retry
	// finds only dedup hits. Reserve the whole range before any attempt.
	w.next = base + int64(len(plans))
	err := b.runMessageTxn(ctx, stats, mu, func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error) {
		var s MessageStoreStats
		found, err := readHashes(store, hashSpace, tenant, plans)
		if err != nil {
			return s, err
		}
		seq := base
		var absent []*messagePlan
		var records []proto.Message
		for k, p := range plans {
			p.element.written = false
			if found[k].valid() {
				p.element.id = found[k]
				s.Present++
				continue
			}
			seq++
			p.element.id = messageID{w.id, seq}
			setMessageID(p.records[0], p.element.id)
			absent = append(absent, p)
			records = append(records, p.records[0])
		}
		if err := canceled(ctx, op); err != nil {
			return s, err
		}
		// One batch: the existence reads and the TEXT bunch reads each take
		// one round trip for all records.
		if _, err := store.SaveRecordBatch(records); err != nil {
			return s, err
		}
		for _, p := range absent {
			p.element.written = true
			s.Written++
			s.Parts++
			s.Postings += p.postings
			s.UnindexedTokens += p.unindexed
		}
		return s, nil
	})
	if err == nil {
		for _, p := range plans {
			if p.element.written {
				p.element.outcome = MessageStoreStats{Written: 1, Parts: 1, Postings: p.postings, UnindexedTokens: p.unindexed}
			} else {
				p.element.outcome.Present = 1
			}
		}
		for _, p := range original {
			canonical := byDigest[p.element.digest]
			if canonical != p {
				p.element.id = canonical.element.id
				p.element.written = false
				p.element.outcome.Present = 1
			}
		}
	}
	return err
}

// writeLargeMessage groups tails below messagePartTxnBytes, then publishes part 0.
// Each transaction checks the digest; size errors trigger finer replanning.
func (b *FDBBackend) writeLargeMessage(ctx context.Context, tenant int, w *messageWriter, p *messagePlan, stats *MessageStoreStats, mu *sync.Mutex) error {
	const op = "message persist"
	if p.element.outcome.Written != 0 || p.element.outcome.Present != 0 {
		return nil // an earlier fallback pass already committed this outcome
	}
	var committed MessageStoreStats
	var committedMu sync.Mutex
	defer func() {
		p.element.outcome.Parts += committed.Parts
		p.element.outcome.Postings += committed.Postings
		p.element.outcome.UnindexedTokens += committed.UnindexedTokens
		if committed.Written > 0 {
			p.element.outcome.Written = 1
		} else if committed.Present > 0 {
			p.element.outcome.Present = 1
		}
		mu.Lock()
		stats.Add(committed)
		mu.Unlock()
	}()
	p.element.written = false
	w.next++
	id := messageID{w.id, w.next}
	for _, r := range p.records {
		setMessageID(r, id)
	}
	present := false
	check := func(store *recordlayer.FDBRecordStore, hashSpace []byte) (bool, error) {
		found, err := readHashes(store, hashSpace, tenant, []*messagePlan{p})
		if err != nil || !found[0].valid() {
			return false, err
		}
		p.element.id = found[0]
		return true, nil
	}
	for next := 1; next < len(p.records) && !present; {
		lo := next
		err := b.runMessageTxn(ctx, &committed, &committedMu, func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error) {
			next = lo
			var err error
			if present, err = check(store, hashSpace); err != nil || present {
				return MessageStoreStats{Present: 1}, err
			}
			var s MessageStoreStats
			for next < len(p.records) {
				if err := canceled(ctx, op); err != nil {
					return s, err
				}
				if _, err := store.SaveRecord(p.records[next]); err != nil {
					return s, err
				}
				next++
				s.Parts++
				n, err := store.Context().Transaction().GetApproximateSize().Get()
				if err != nil {
					return s, err
				}
				if n >= messagePartTxnBytes {
					break
				}
			}
			return s, nil
		})
		if err != nil {
			return err
		}
	}
	if present {
		return nil
	}
	return b.runMessageTxn(ctx, &committed, &committedMu, func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error) {
		present, err := check(store, hashSpace)
		if err != nil || present {
			return MessageStoreStats{Present: 1}, err
		}
		if _, err := store.SaveRecord(p.records[0]); err != nil {
			return MessageStoreStats{}, err
		}
		p.element.id, p.element.written = id, true
		return MessageStoreStats{Written: 1, Parts: 1, Postings: p.postings, UnindexedTokens: p.unindexed}, nil
	})
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

// setEnvelopeRefs lists a span's references in its Envelope, packed.
func setEnvelopeRefs(m proto.Message, uses []messageUse) {
	r := m.ProtoReflect()
	field := r.Descriptor().Fields().ByName("ref_chunks")
	r.Clear(field)
	chunks := packSpanRefs(uses)
	if len(chunks) == 0 {
		return
	}
	list := r.Mutable(field).List()
	for i, c := range chunks {
		e := list.NewElement().Message()
		f := e.Descriptor().Fields()
		e.Set(f.ByName("chunk"), protoreflect.ValueOfInt64(int64(i)))
		e.Set(f.ByName("refs"), protoreflect.ValueOfBytes(c))
		list.Append(protoreflect.ValueOfMessage(e))
	}
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

// persistTraceMarkers saves absent (message, trace) markers before any span commits.
// Snapshot presence checks are safe: concurrent saves conflict on their own existence reads.
func (e *Exporter) persistTraceMarkers(ctx context.Context, d *messageDecomposition) (int64, error) {
	const op = "message trace markers"
	b := e.backend
	tenant := int64(e.cfg.Tenant)
	seen := map[traceMarker]bool{}
	var pending []traceMarker
	for key, uses := range d.uses {
		trace := key[:32]
		for _, u := range uses {
			k := traceMarker{u.element.id, trace}
			if !k.id.valid() {
				return 0, fail("operation", op, "message element has no stored ID")
			}
			if !seen[k] && !b.markers.has(k) {
				seen[k] = true
				pending = append(pending, k)
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].id != pending[j].id {
			return pending[i].id.less(pending[j].id)
		}
		return pending[i].trace < pending[j].trace
	})
	d2, err := recordFile(e.cfg.messageLayout())
	if err != nil {
		return 0, err
	}
	desc := d2.Messages().ByName("MessageTrace")
	var written int64
	for lo := 0; lo < len(pending); lo += messageMarkerTxn {
		group := pending[lo:min(lo+messageMarkerTxn, len(pending))]
		var n int64
		_, err := runTxnSlot(ctx, b.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
			n = 0
			if err := b.probeMessageTxn("marker-before-write", nil); err != nil {
				return nil, err
			}
			if err := b.readManifest(rc, true); err != nil {
				return nil, err
			}
			store, err := b.open(rc)
			if err != nil {
				return nil, err
			}
			records := store.RecordsSubspace()
			reads := make([]fdb.RangeResult, len(group))
			for i, k := range group {
				r, err := fdb.PrefixRange(records.Pack(tuple.Tuple{messageTraceTypeKey, tenant, k.id.writer, k.id.seq, k.trace}))
				if err != nil {
					return nil, err
				}
				reads[i] = rc.Transaction().Snapshot().GetRange(r, fdb.RangeOptions{Limit: 1})
			}
			var absent []proto.Message
			for i, k := range group {
				rows, err := reads[i].GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(rows) > 0 {
					continue
				}
				m := dynamicpb.NewMessage(desc)
				f := desc.Fields()
				m.Set(f.ByName("tenant"), protoreflect.ValueOfInt64(tenant))
				m.Set(f.ByName("writer"), protoreflect.ValueOfInt64(k.id.writer))
				m.Set(f.ByName("seq"), protoreflect.ValueOfInt64(k.id.seq))
				m.Set(f.ByName("trace_id"), protoreflect.ValueOfString(k.trace))
				absent = append(absent, m)
			}
			if len(absent) == 0 {
				return nil, nil
			}
			n = int64(len(absent))
			_, err = store.SaveRecordBatch(absent)
			return nil, err
		})
		if err != nil {
			return written, err
		}
		written += n
		b.markers.add(group)
	}
	return written, nil
}

// inflightMessage is a message one export call of this backend is storing;
// other calls that need it wait for its ID instead of storing it again.
type inflightMessage struct {
	done chan struct{}
	id   messageID // valid once done, if the store succeeded
}

// persistMessages assigns IDs before spans commit and awaits same-backend in-flight elements.
// Cross-backend writers still resolve duplicates through the unique digest index.
func (e *Exporter) persistMessages(ctx context.Context, d *messageDecomposition) (stats MessageStoreStats, err error) {
	b := e.backend
	if len(d.elements) == 0 {
		return stats, nil
	}
	started := time.Now()
	defer func() { stats.WallNs = time.Since(started).Nanoseconds() }()
	found, err := b.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
	if err != nil {
		return stats, err
	}
	stats.Transactions++
	var owned, awaited []*messageElement
	var waits []*inflightMessage
	var claims []*inflightMessage
	for _, el := range d.elements {
		if id, ok := found[el.digest]; ok {
			el.id = id
			stats.Present++
			continue
		}
		claim := &inflightMessage{done: make(chan struct{})}
		if other, loaded := b.inflight.LoadOrStore(el.digest, claim); loaded {
			if err := b.probeMessageTxn("message-await", nil); err != nil {
				return stats, err
			}
			awaited = append(awaited, el)
			waits = append(waits, other.(*inflightMessage))
			continue
		}
		owned = append(owned, el)
		claims = append(claims, claim)
	}
	written, err := e.storeMessages(ctx, owned)
	stats.Add(written)
	// Release claims before waiting to avoid cycles; after failure, waiters must store again
	// because the assigned ID may belong to an uncommitted attempt.
	for i, el := range owned {
		if err == nil {
			claims[i].id = el.id
		}
		b.inflight.Delete(el.digest)
		close(claims[i].done)
	}
	if err != nil {
		return stats, err
	}
	var retry []*messageElement
	for i, el := range awaited {
		select {
		case <-waits[i].done:
		case <-ctx.Done():
			return stats, wrap("canceled", "message persist", ctx.Err())
		}
		if el.id = waits[i].id; el.id.valid() {
			stats.Present++
		} else {
			retry = append(retry, el) // the other call failed
		}
	}
	written, err = e.storeMessages(ctx, retry)
	stats.Add(written)
	return stats, err
}

// storeMessages plans every element before the first write, so an admission
// failure leaves nothing behind, then writes them.
func (e *Exporter) storeMessages(ctx context.Context, elements []*messageElement) (MessageStoreStats, error) {
	if len(elements) == 0 {
		return MessageStoreStats{}, nil
	}
	fulltext := e.cfg.messageLayout() == messagesFullText
	plans := make([]*messagePlan, len(elements))
	if err := parallelMessageWork(len(elements), func(i int) error {
		var err error
		plans[i], err = planMessage(e.cfg.Tenant, elements[i], fulltext, messageSegmentTokens)
		return err
	}); err != nil {
		return MessageStoreStats{}, err
	}
	if err := canceled(ctx, "message persist"); err != nil {
		return MessageStoreStats{}, err
	}
	return e.backend.writeMessages(ctx, e.cfg.Tenant, plans, fulltext)
}

// ---------------------------------------------------------------------------
// Reconstruction
// ---------------------------------------------------------------------------

type storedSite struct {
	kv       *commonpb.KeyValue
	manifest *messageManifest
}

// reconstructMessages restores spans in two read waves: referenced heads first,
// then remaining raw parts of multipart messages, within the caller’s transaction.
func reconstructMessages(ctx context.Context, store *recordlayer.FDBRecordStore, tenant int, spans []*tracepb.ResourceSpans) ([]*tracepb.ResourceSpans, error) {
	p, err := planReconstruct(ctx, spans)
	if err != nil {
		return nil, err
	}
	raws, err := loadMessages(store, tenant, p.refs, p.encodings)
	if err != nil {
		return nil, err
	}
	return p.apply(raws)
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

// loadMessages returns the raw element bytes of each message.
func loadMessages(store *recordlayer.FDBRecordStore, tenant int, refs []messageID, encodings map[messageID]string) (map[messageID][]byte, error) {
	const op = "message reconstruct"
	heads := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], len(refs))
	for i, id := range refs {
		heads[i] = store.LoadRecordAsync(messageKey(tenant, id, 0), nil)
	}
	type rest struct {
		id    messageID
		size  int64
		parts []recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]]
	}
	var more []rest
	out := make(map[messageID][]byte, len(refs))
	for i, id := range refs {
		r, err := heads[i].Get()
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, failf("corruption", op, "missing message %d/%d", id.writer, id.seq)
		}
		m := r.Record.ProtoReflect()
		f := m.Descriptor().Fields()
		parts, size := m.Get(f.ByName("parts")).Int(), m.Get(f.ByName("raw_bytes")).Int()
		if m.Get(f.ByName("encoding")).String() != encodings[id] || parts < 1 || size < 0 || size > maxMessageBytes {
			return nil, failf("corruption", op, "message %d/%d header disagrees with its manifest", id.writer, id.seq)
		}
		raw := m.Get(f.ByName("raw")).Bytes()
		if int64(len(raw)) == size {
			out[id] = raw
			continue
		}
		x := rest{id: id, size: size}
		out[id] = append([]byte(nil), raw...)
		for p := int64(1); p < parts && p*messagePartRawBytes < size; p++ {
			x.parts = append(x.parts, store.LoadRecordAsync(messageKey(tenant, id, int(p)), nil))
		}
		more = append(more, x)
	}
	for _, x := range more {
		for _, pr := range x.parts {
			r, err := pr.Get()
			if err != nil {
				return nil, err
			}
			if r == nil {
				return nil, failf("corruption", op, "missing part of message %d/%d", x.id.writer, x.id.seq)
			}
			m := r.Record.ProtoReflect()
			out[x.id] = append(out[x.id], m.Get(m.Descriptor().Fields().ByName("raw")).Bytes()...)
		}
		if int64(len(out[x.id])) != x.size {
			return nil, failf("corruption", op, "message %d/%d has %d raw bytes, header says %d", x.id.writer, x.id.seq, len(out[x.id]), x.size)
		}
	}
	return out, nil
}
