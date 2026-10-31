package otel

// Content-addressed storage for GenAI message attributes. Turn k of an agent
// conversation repeats turns 1..k-1 in gen_ai.input.messages; storing each
// message once per tenant and replacing the attribute with a list of digests
// keeps per-span storage O(turn) instead of O(history bytes).
//
// This is tenant-scoped deduplication, not RFC-283 §5.2's source-exclusive
// content objects: blobs are immutable, shared by every span of a tenant that
// references them, and have no lifecycle manifest or garbage collection.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"github.com/andybalholm/brotli"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	messageBlobSubspace = "msg_blob_v1"
	messageHashDomain   = "otel_msg_v1\x00"
	messageChainDomain  = "otel_msg_chain_v1\x00"

	// Element encodings; part of the hash domain so equal bytes in different
	// encodings never alias.
	messageEncodingAny  = "otlp_any"
	messageEncodingJSON = "json"

	messageChunkBytes      = 80_000 // whole FDB value, header included
	messageChunkHeaderMax  = 32
	maxMessageBytes        = 16 << 20 // RFC-283 §5.2 proposed decoded object admission
	maxMessageManifest     = 64_000   // the covering-projection cap in index.go
	maxMessageKeyBytes     = 10_000
	messageCompressMin     = 64
	messageBrotliLevel     = 4
	messageTxnSplitBytes   = 250_000 // PersistMessageBlobsDB batch target
	messageFTSSplitBytes   = 80_000  // PersistAndIndexMessageBlobsDB / IndexMessageSpansDB batch target (accounts for per-key FDB txn overhead)
	messageTxnBudgetBytes  = 300_000 // PersistMessageBlobs admission bound
	messageTxnGuardBytes   = 500_000 // approximate-size guard after writes
	messageReconstructWave = 64      // blobs per ReconstructResourceSpansDB transaction

	messageBlobVersion = 1
	messageCodecRaw    = 0
	messageCodecBrotli = 1

	messageManifestMagic   = "OTMD"
	messageManifestVersion = 1
)

// Manifest layouts describe how the elements reassemble into the original
// attribute value.
const (
	messageLayoutNone  byte = iota // KeyValue.Value was nil
	messageLayoutValue             // the whole AnyValue is one element
	messageLayoutArray             // ArrayValue; each element is an AnyValue
	messageLayoutJSON              // StringValue holding a JSON array; elements are raw JSON
)

var messageLayoutNames = [...]string{"none", "value", "array", "json"}

var messageDeltaKeys = map[string]bool{
	"gen_ai.input.messages":      true,
	"gen_ai.output.messages":     true,
	"gen_ai.system_instructions": true,
	"gen_ai.tool.definitions":    true,
}

// IsMessageDeltaAttribute reports whether key is a GenAI content attribute that
// DecomposeResourceSpans replaces with a delta manifest.
func IsMessageDeltaAttribute(key string) bool { return messageDeltaKeys[key] }

// MessageDeltaStats counts one DecomposeResourceSpans call. Byte counts are
// protobuf sizes of attribute values plus encoded FDB chunk values of the
// call's unique blobs; FDB key bytes and cross-call deduplication are excluded.
type MessageDeltaStats struct {
	TotalMessages    int64 `json:"total_messages"`
	UniqueMessages   int64 `json:"unique_messages"`
	ReusedMessages   int64 `json:"reused_messages"`
	RawMessageBytes  int64 `json:"raw_message_bytes"`
	StoredDeltaBytes int64 `json:"stored_delta_bytes"`
	SavedBytes       int64 `json:"saved_bytes"`
}

// MessagePersistStats counts blobs written versus already present in FDB.
type MessagePersistStats struct {
	Written      int   `json:"written"`
	Present      int   `json:"present"`
	Chunks       int   `json:"chunks"`
	Bytes        int64 `json:"bytes"`
	Transactions int   `json:"transactions"`
}

func (s *MessagePersistStats) add(o MessagePersistStats) {
	s.Written += o.Written
	s.Present += o.Present
	s.Chunks += o.Chunks
	s.Bytes += o.Bytes
	s.Transactions += o.Transactions
}

// MessageBlob is one immutable, encoded message element. Its fields are
// unexported so a blob cannot change between decomposition and persistence.
type MessageBlob struct {
	tenant   int
	hash     [32]byte
	encoding string
	raw      []byte
	chunks   [][]byte
}

func (b *MessageBlob) Hash() [32]byte   { return b.hash }
func (b *MessageBlob) Encoding() string { return b.encoding }
func (b *MessageBlob) RawBytes() int    { return len(b.raw) }
func (b *MessageBlob) Chunks() int      { return len(b.chunks) }

// StoredBytes is the sum of the blob's FDB chunk values.
func (b *MessageBlob) StoredBytes() int {
	n := 0
	for _, c := range b.chunks {
		n += len(c)
	}
	return n
}

func messageHash(tenant int, encoding string, element []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(messageHashDomain))
	h.Write([]byte(strconv.Itoa(tenant)))
	h.Write([]byte{0})
	h.Write([]byte(encoding))
	h.Write([]byte{0})
	h.Write(element)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// messageChains returns P_1..P_n with P_0 = sha256(domain) and
// P_k = sha256(P_{k-1} || msgHash_k).
func messageChains(hashes [][32]byte) [][32]byte {
	prev := sha256.Sum256([]byte(messageChainDomain))
	out := make([][32]byte, len(hashes))
	var buf [64]byte
	for i, h := range hashes {
		copy(buf[:32], prev[:])
		copy(buf[32:], h[:])
		prev = sha256.Sum256(buf[:])
		out[i] = prev
	}
	return out
}

func messageChainHead(hashes [][32]byte) [32]byte {
	if len(hashes) == 0 {
		return sha256.Sum256([]byte(messageChainDomain))
	}
	c := messageChains(hashes)
	return c[len(c)-1]
}

// MessageDeltaManifest is the decoded replacement value of one attribute.
type MessageDeltaManifest struct {
	Layout string
	Hashes [][32]byte
	// Chain is P_n over Hashes; it is verified when the manifest is parsed.
	Chain  [32]byte
	layout byte
	head   []byte
	tail   []byte
	seps   [][]byte // nil means every separator is ","
}

// PrefixChains returns the rolling chain hash P_k after each element.
func (m *MessageDeltaManifest) PrefixChains() [][32]byte { return messageChains(m.Hashes) }

// HasPrefix reports whether p's message sequence is a prefix of m's, by
// comparing p's chain head with m's chain at the same length.
func (m *MessageDeltaManifest) HasPrefix(p *MessageDeltaManifest) bool {
	if p == nil || len(p.Hashes) > len(m.Hashes) {
		return false
	}
	return messageChainHead(m.Hashes[:len(p.Hashes)]) == p.Chain
}

func (m *MessageDeltaManifest) encode() []byte {
	out := make([]byte, 0, 8+len(m.Hashes)*32+32+len(m.head)+len(m.tail))
	out = append(out, messageManifestMagic...)
	out = append(out, messageManifestVersion, m.layout)
	out = binary.AppendUvarint(out, uint64(len(m.Hashes)))
	for _, h := range m.Hashes {
		out = append(out, h[:]...)
	}
	out = append(out, m.Chain[:]...)
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

// ParseMessageDeltaManifest decodes and verifies a manifest attribute value.
func ParseMessageDeltaManifest(v *commonpb.AnyValue) (*MessageDeltaManifest, error) {
	wire, ok := v.GetValue().(*commonpb.AnyValue_BytesValue)
	if !ok {
		return nil, fail("corruption", "message manifest", "attribute is not a message delta manifest")
	}
	r := &byteReader{b: wire.BytesValue}
	if string(r.take(len(messageManifestMagic))) != messageManifestMagic || r.byte1() != messageManifestVersion {
		return nil, fail("corruption", "message manifest", "bad manifest magic or version")
	}
	m := &MessageDeltaManifest{layout: r.byte1()}
	n := r.uvarint(maxMessageManifest / 32)
	if r.bad || int(m.layout) >= len(messageLayoutNames) {
		return nil, fail("corruption", "message manifest", "bad manifest layout")
	}
	m.Layout = messageLayoutNames[m.layout]
	m.Hashes = make([][32]byte, n)
	for i := range m.Hashes {
		copy(m.Hashes[i][:], r.take(32))
	}
	copy(m.Chain[:], r.take(32))
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
		return nil, failf("corruption", "message manifest", "%s layout with %d elements", m.Layout, n)
	}
	if messageChainHead(m.Hashes) != m.Chain {
		return nil, fail("corruption", "message manifest", "prefix chain hash mismatch")
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
func splitMessageValue(v *commonpb.AnyValue) (m *MessageDeltaManifest, encoding string, elems [][]byte, err error) {
	if v == nil {
		return &MessageDeltaManifest{layout: messageLayoutNone}, messageEncodingAny, nil, nil
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_ArrayValue:
		if len(v.ProtoReflect().GetUnknown()) == 0 && (x.ArrayValue == nil || len(x.ArrayValue.ProtoReflect().GetUnknown()) == 0) {
			var values []*commonpb.AnyValue
			if x.ArrayValue != nil {
				values = x.ArrayValue.Values
			}
			elems = make([][]byte, 0, len(values))
			for _, e := range values {
				if e == nil {
					return nil, "", nil, fail("admission", "message element encode", "nil array element")
				}
				wire, err := marshalMessageValue(e)
				if err != nil {
					return nil, "", nil, err
				}
				elems = append(elems, wire)
			}
			return &MessageDeltaManifest{layout: messageLayoutArray}, messageEncodingAny, elems, nil
		}
	case *commonpb.AnyValue_StringValue:
		if len(v.ProtoReflect().GetUnknown()) == 0 {
			if parts, head, tail, seps, ok := splitJSONMessages(x.StringValue); ok {
				m := &MessageDeltaManifest{layout: messageLayoutJSON, head: []byte(head), tail: []byte(tail)}
				for _, s := range seps {
					if s != "," {
						m.seps = make([][]byte, len(seps))
						for i, s := range seps {
							m.seps[i] = []byte(s)
						}
						break
					}
				}
				elems = make([][]byte, len(parts))
				for i, p := range parts {
					elems[i] = []byte(p)
				}
				return m, messageEncodingJSON, elems, nil
			}
		}
	}
	wire, err := marshalMessageValue(v)
	if err != nil {
		return nil, "", nil, err
	}
	return &MessageDeltaManifest{layout: messageLayoutValue}, messageEncodingAny, [][]byte{wire}, nil
}

func messageChunkCount(stored int) int {
	first := messageChunkBytes - messageChunkHeaderMax
	if stored <= first {
		return 1
	}
	return 1 + (stored-first+messageChunkBytes-1)/messageChunkBytes
}

func compressMessage(raw []byte) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, messageBrotliLevel)
	if _, err := w.Write(raw); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// encodeMessageChunks is deterministic for a given raw element. Chunk 0 holds
// version, codec, raw length, stored length and chunk count before its payload.
func encodeMessageChunks(raw []byte) [][]byte {
	stored, codec := raw, byte(messageCodecRaw)
	if len(raw) >= messageCompressMin {
		if c := compressMessage(raw); c != nil && len(c) < len(raw) {
			stored, codec = c, messageCodecBrotli
		}
	}
	n := messageChunkCount(len(stored))
	head := []byte{messageBlobVersion, codec}
	head = binary.AppendUvarint(head, uint64(len(raw)))
	head = binary.AppendUvarint(head, uint64(len(stored)))
	head = binary.AppendUvarint(head, uint64(n))
	first := min(len(stored), messageChunkBytes-messageChunkHeaderMax)
	chunks := make([][]byte, 0, n)
	chunks = append(chunks, append(head, stored[:first]...))
	for rest := stored[first:]; len(rest) > 0; {
		k := min(len(rest), messageChunkBytes)
		chunks = append(chunks, rest[:k:k])
		rest = rest[k:]
	}
	return chunks
}

func messageBlobSpace(root subspace.Subspace, tenant int, hash [32]byte) subspace.Subspace {
	return root.Sub(messageBlobSubspace, int64(tenant), hash[:])
}

func checkMessageRoot(op string, root subspace.Subspace, tenant int) error {
	if root == nil || (reflect.ValueOf(root).Kind() == reflect.Ptr && reflect.ValueOf(root).IsNil()) {
		return fail("config", op, "message blob root subspace required")
	}
	var zero [32]byte
	if n := len(messageBlobSpace(root, tenant, zero).Sub(int64(messageChunkCount(maxMessageBytes))).Bytes()); n > maxMessageKeyBytes {
		return failf("config", op, "message blob key of %d bytes exceeds %d", n, maxMessageKeyBytes)
	}
	return nil
}

// decodeMessageBlob validates one blob's chunk rows (as read from its
// subspace) and returns the verified raw element.
func decodeMessageBlob(space subspace.Subspace, tenant int, hash [32]byte, encoding string, rows []fdb.KeyValue) ([]byte, error) {
	if len(rows) == 0 {
		return nil, failf("corruption", "message blob", "missing message blob %x", hash[:8])
	}
	var payload []byte
	for i, kv := range rows {
		t, err := space.Unpack(kv.Key)
		if err != nil || len(t) != 1 {
			return nil, failf("corruption", "message blob", "malformed chunk key in blob %x", hash[:8])
		}
		if idx, ok := t[0].(int64); !ok || idx != int64(i) {
			return nil, failf("corruption", "message blob", "chunk %d missing or out of order in blob %x", i, hash[:8])
		}
		if len(kv.Value) > messageChunkBytes {
			return nil, failf("corruption", "message blob", "chunk value of %d bytes exceeds %d", len(kv.Value), messageChunkBytes)
		}
		if i > 0 {
			payload = append(payload, kv.Value...)
		}
	}
	r := &byteReader{b: rows[0].Value}
	version, codec := r.byte1(), r.byte1()
	rawLen := r.uvarint(maxMessageBytes)
	storedLen := r.uvarint(maxMessageBytes)
	n := r.uvarint(uint64(messageChunkCount(maxMessageBytes)))
	if r.bad || version != messageBlobVersion || (codec != messageCodecRaw && codec != messageCodecBrotli) {
		return nil, failf("corruption", "message blob", "bad header in blob %x", hash[:8])
	}
	payload = append(append([]byte(nil), r.b...), payload...)
	if n != len(rows) || n != messageChunkCount(storedLen) || len(payload) != storedLen || (codec == messageCodecRaw && storedLen != rawLen) {
		return nil, failf("corruption", "message blob", "blob %x has %d chunks/%d bytes, header says %d/%d", hash[:8], len(rows), len(payload), n, storedLen)
	}
	raw := payload
	if codec == messageCodecBrotli {
		var err error
		raw, err = io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(payload)), int64(rawLen)+1))
		if err != nil {
			return nil, typedError("corruption", "message blob", "brotli decode", err)
		}
		if len(raw) != rawLen {
			return nil, failf("corruption", "message blob", "decoded %d bytes, header says %d", len(raw), rawLen)
		}
	}
	if messageHash(tenant, encoding, raw) != hash {
		return nil, failf("corruption", "message blob", "SHA-256 integrity mismatch for blob %x", hash[:8])
	}
	return raw, nil
}

// forEachMessageAttribute visits every content attribute on spans and events.
func forEachMessageAttribute(rs *tracepb.ResourceSpans, fn func(*commonpb.KeyValue) error) error {
	visit := func(kvs []*commonpb.KeyValue) error {
		for _, kv := range kvs {
			if kv != nil && messageDeltaKeys[kv.Key] {
				if err := fn(kv); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, ss := range rs.GetScopeSpans() {
		for _, sp := range ss.GetSpans() {
			if err := visit(sp.GetAttributes()); err != nil {
				return err
			}
			for _, ev := range sp.GetEvents() {
				if err := visit(ev.GetAttributes()); err != nil {
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

// DecomposeResourceSpans returns clones of spans in which every GenAI content
// attribute is replaced by a manifest, plus this call's unique blobs in
// first-reference order. The input is never modified.
func DecomposeResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans, tenant int) ([]*tracepb.ResourceSpans, []*MessageBlob, MessageDeltaStats, error) {
	const op = "message decompose"
	var stats MessageDeltaStats
	out := make([]*tracepb.ResourceSpans, len(spans))
	var blobs []*MessageBlob
	seen := map[[32]byte]bool{}
	for i, rs := range spans {
		if err := canceled(ctx, op); err != nil {
			return nil, nil, MessageDeltaStats{}, err
		}
		if rs == nil {
			continue
		}
		clone := proto.Clone(rs).(*tracepb.ResourceSpans)
		err := forEachMessageAttribute(clone, func(kv *commonpb.KeyValue) error {
			m, encoding, elems, err := splitMessageValue(kv.Value)
			if err != nil {
				return err
			}
			m.Hashes = make([][32]byte, len(elems))
			for j, e := range elems {
				if len(e) > maxMessageBytes {
					return failf("admission", op, "message element of %d bytes exceeds %d", len(e), maxMessageBytes)
				}
				h := messageHash(tenant, encoding, e)
				m.Hashes[j] = h
				stats.TotalMessages++
				if seen[h] {
					continue
				}
				seen[h] = true
				b := &MessageBlob{tenant: tenant, hash: h, encoding: encoding, raw: e, chunks: encodeMessageChunks(e)}
				blobs = append(blobs, b)
				stats.StoredDeltaBytes += int64(b.StoredBytes())
			}
			m.Chain = messageChainHead(m.Hashes)
			wire := m.encode()
			if len(wire) > maxMessageManifest {
				return failf("admission", op, "%s manifest of %d bytes exceeds %d", kv.Key, len(wire), maxMessageManifest)
			}
			if kv.Value != nil {
				stats.RawMessageBytes += int64(proto.Size(kv.Value))
			}
			kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: wire}}
			stats.StoredDeltaBytes += int64(proto.Size(kv.Value))
			return nil
		})
		if err != nil {
			return nil, nil, MessageDeltaStats{}, err
		}
		out[i] = clone
	}
	stats.UniqueMessages = int64(len(blobs))
	stats.ReusedMessages = stats.TotalMessages - stats.UniqueMessages
	stats.SavedBytes = stats.RawMessageBytes - stats.StoredDeltaBytes
	return out, blobs, stats, nil
}

// admitMessageBlobs deduplicates blobs and re-verifies tenant and digest.
func admitMessageBlobs(op string, tenant int, blobs []*MessageBlob) ([]*MessageBlob, error) {
	out := make([]*MessageBlob, 0, len(blobs))
	seen := map[[32]byte]bool{}
	for _, b := range blobs {
		if b == nil || len(b.chunks) == 0 {
			return nil, fail("admission", op, "message blob was not produced by DecomposeResourceSpans")
		}
		if b.tenant != tenant {
			return nil, failf("admission", op, "message blob belongs to tenant %d, not %d", b.tenant, tenant)
		}
		if messageHash(tenant, b.encoding, b.raw) != b.hash {
			return nil, fail("admission", op, "message blob digest mismatch")
		}
		if !seen[b.hash] {
			seen[b.hash] = true
			out = append(out, b)
		}
	}
	return out, nil
}

func messageBlobWriteBytes(root subspace.Subspace, tenant int, b *MessageBlob) int {
	key := len(messageBlobSpace(root, tenant, b.hash).Sub(int64(0)).Bytes())
	return b.StoredBytes() + key*len(b.chunks)
}

func messageTxnGuard(op string, rc *recordlayer.FDBRecordContext) error {
	n, err := rc.Transaction().GetApproximateSize().Get()
	if err != nil {
		return err
	}
	if n > messageTxnGuardBytes {
		return failf("budget", op, "transaction guard exceeded: bytes=%d", n)
	}
	return nil
}

// persistMessageBlobsTxn writes every blob whose chunk 0 is not already the
// expected deterministic encoding. Clearing first removes stale chunks of an
// incomplete earlier write; the blob becomes visible atomically at commit.
func persistMessageBlobsTxn(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, blobs []*MessageBlob) (MessagePersistStats, error) {
	const op = "message persist"
	var stats MessagePersistStats
	if err := canceled(ctx, op); err != nil {
		return stats, err
	}
	snap := rc.ReadTransaction(true)
	pending := make([]fdb.FutureByteSlice, len(blobs))
	for i, b := range blobs {
		pending[i] = snap.Get(messageBlobSpace(root, tenant, b.hash).Sub(int64(0)))
	}
	tx := rc.Transaction()
	for i, b := range blobs {
		current, err := pending[i].Get()
		if err != nil {
			return stats, err
		}
		if bytes.Equal(current, b.chunks[0]) {
			stats.Present++
			continue
		}
		space := messageBlobSpace(root, tenant, b.hash)
		tx.ClearRange(space)
		for j, c := range b.chunks {
			tx.Set(space.Sub(int64(j)), c)
		}
		stats.Written++
		stats.Chunks += len(b.chunks)
		stats.Bytes += int64(messageBlobWriteBytes(root, tenant, b))
	}
	if stats.Written > 0 {
		if err := messageTxnGuard(op, rc); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// PersistMessageBlobs writes blobs in the caller's transaction. Their encoded
// size must fit the 300KB budget; larger sets need PersistMessageBlobsDB.
func PersistMessageBlobs(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, blobs []*MessageBlob) (MessagePersistStats, error) {
	const op = "message persist"
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return MessagePersistStats{}, err
	}
	unique, err := admitMessageBlobs(op, tenant, blobs)
	if err != nil {
		return MessagePersistStats{}, err
	}
	total := 0
	for _, b := range unique {
		total += messageBlobWriteBytes(root, tenant, b)
	}
	if total > messageTxnBudgetBytes {
		return MessagePersistStats{}, failf("admission", op, "message blobs of %d bytes exceed the %d-byte transaction budget; use PersistMessageBlobsDB", total, messageTxnBudgetBytes)
	}
	return persistMessageBlobsTxn(ctx, rc, root, tenant, unique)
}

// PersistMessageBlobsDB writes blobs in bounded transactions of at most about
// 250KB. A blob larger than that is staged: its tail chunks commit first and
// chunk 0, which makes it readable, commits last.
func PersistMessageBlobsDB(ctx context.Context, db *recordlayer.FDBDatabase, root subspace.Subspace, tenant int, blobs []*MessageBlob) (MessagePersistStats, error) {
	const op = "message persist"
	var stats MessagePersistStats
	if db == nil {
		return stats, fail("config", op, "database required")
	}
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return stats, err
	}
	unique, err := admitMessageBlobs(op, tenant, blobs)
	if err != nil {
		return stats, err
	}
	run := func(fn func(*recordlayer.FDBRecordContext) (MessagePersistStats, error)) error {
		var got MessagePersistStats
		_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			if err := configureRecordTimeout(rc); err != nil {
				return nil, err
			}
			var err error
			got, err = fn(rc)
			return nil, err
		})
		if err == nil {
			got.Transactions = 1
			stats.add(got)
		}
		return err
	}
	var batch []*MessageBlob
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		b := batch
		batch, size = nil, 0
		return run(func(rc *recordlayer.FDBRecordContext) (MessagePersistStats, error) {
			return persistMessageBlobsTxn(ctx, rc, root, tenant, b)
		})
	}
	for _, b := range unique {
		n := messageBlobWriteBytes(root, tenant, b)
		if n > messageTxnSplitBytes {
			if err := persistStagedMessageBlob(ctx, run, root, tenant, b); err != nil {
				return stats, err
			}
			continue
		}
		if size+n > messageTxnSplitBytes {
			if err := flush(); err != nil {
				return stats, err
			}
		}
		batch = append(batch, b)
		size += n
	}
	return stats, flush()
}

func persistStagedMessageBlob(ctx context.Context, run func(func(*recordlayer.FDBRecordContext) (MessagePersistStats, error)) error, root subspace.Subspace, tenant int, b *MessageBlob) error {
	const op = "message persist"
	space := messageBlobSpace(root, tenant, b.hash)
	present := false
	if err := run(func(rc *recordlayer.FDBRecordContext) (MessagePersistStats, error) {
		current, err := rc.ReadTransaction(true).Get(space.Sub(int64(0))).Get()
		present = err == nil && bytes.Equal(current, b.chunks[0])
		if present {
			return MessagePersistStats{Present: 1}, nil
		}
		return MessagePersistStats{}, err
	}); err != nil || present {
		return err
	}
	keyBytes := len(space.Sub(int64(0)).Bytes())
	write := func(lo, hi int, final bool) error {
		return run(func(rc *recordlayer.FDBRecordContext) (MessagePersistStats, error) {
			if err := canceled(ctx, op); err != nil {
				return MessagePersistStats{}, err
			}
			var s MessagePersistStats
			for j := lo; j < hi; j++ {
				rc.Transaction().Set(space.Sub(int64(j)), b.chunks[j])
				s.Chunks++
				s.Bytes += int64(len(b.chunks[j]) + keyBytes)
			}
			if final {
				s.Written = 1
			}
			return s, messageTxnGuard(op, rc)
		})
	}
	lo, size := 1, 0
	for j := 1; j < len(b.chunks); j++ {
		n := len(b.chunks[j]) + keyBytes
		if j > lo && size+n > messageTxnSplitBytes {
			if err := write(lo, j, false); err != nil {
				return err
			}
			lo, size = j, 0
		}
		size += n
	}
	if lo < len(b.chunks) {
		if err := write(lo, len(b.chunks), false); err != nil {
			return err
		}
	}
	return write(0, 1, true)
}

type messageSite struct {
	kv       *commonpb.KeyValue
	manifest *MessageDeltaManifest
}

type messageRef struct {
	hash     [32]byte
	encoding string
}

type messagePlan struct {
	spans []*tracepb.ResourceSpans
	sites []messageSite
	refs  []messageRef
}

func manifestEncoding(m *MessageDeltaManifest) string {
	if m.layout == messageLayoutJSON {
		return messageEncodingJSON
	}
	return messageEncodingAny
}

func planMessageReconstruct(ctx context.Context, spans []*tracepb.ResourceSpans) (*messagePlan, error) {
	const op = "message reconstruct"
	p := &messagePlan{spans: make([]*tracepb.ResourceSpans, len(spans))}
	encodings := map[[32]byte]string{}
	for i, rs := range spans {
		if err := canceled(ctx, op); err != nil {
			return nil, err
		}
		if rs == nil {
			continue
		}
		clone := proto.Clone(rs).(*tracepb.ResourceSpans)
		err := forEachMessageAttribute(clone, func(kv *commonpb.KeyValue) error {
			m, err := ParseMessageDeltaManifest(kv.Value)
			if err != nil {
				return err
			}
			enc := manifestEncoding(m)
			for _, h := range m.Hashes {
				if prev, ok := encodings[h]; ok {
					if prev != enc {
						return fail("corruption", op, "message digest referenced with two encodings")
					}
					continue
				}
				encodings[h] = enc
				p.refs = append(p.refs, messageRef{h, enc})
			}
			p.sites = append(p.sites, messageSite{kv, m})
			return nil
		})
		if err != nil {
			return nil, err
		}
		p.spans[i] = clone
	}
	return p, nil
}

func (p *messagePlan) apply(raws map[[32]byte][]byte) ([]*tracepb.ResourceSpans, error) {
	const op = "message reconstruct"
	element := func(h [32]byte) (*commonpb.AnyValue, error) {
		v := &commonpb.AnyValue{}
		if err := proto.Unmarshal(raws[h], v); err != nil {
			return nil, typedError("corruption", op, "message element decode", err)
		}
		return v, nil
	}
	for _, s := range p.sites {
		m := s.manifest
		switch m.layout {
		case messageLayoutNone:
			s.kv.Value = nil
		case messageLayoutValue:
			v, err := element(m.Hashes[0])
			if err != nil {
				return nil, err
			}
			s.kv.Value = v
		case messageLayoutArray:
			arr := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(m.Hashes))}
			for i, h := range m.Hashes {
				v, err := element(h)
				if err != nil {
					return nil, err
				}
				arr.Values[i] = v
			}
			s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: arr}}
		case messageLayoutJSON:
			var b strings.Builder
			b.Write(m.head)
			for i, h := range m.Hashes {
				if i > 0 {
					if m.seps == nil {
						b.WriteByte(',')
					} else {
						b.Write(m.seps[i-1])
					}
				}
				b.Write(raws[h])
			}
			b.Write(m.tail)
			s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: b.String()}}
		}
	}
	return p.spans, nil
}

// fetchMessageBlobs issues one snapshot range read per blob before waiting on
// any of them, then verifies each blob.
func fetchMessageBlobs(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, refs []messageRef, out map[[32]byte][]byte) error {
	if err := canceled(ctx, "message reconstruct"); err != nil {
		return err
	}
	snap := rc.ReadTransaction(true)
	limit := messageChunkCount(maxMessageBytes) + 1
	pending := make([]fdb.RangeResult, len(refs))
	for i, r := range refs {
		pending[i] = snap.GetRange(messageBlobSpace(root, tenant, r.hash), fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})
	}
	for i, r := range refs {
		rows, err := pending[i].GetSliceWithError()
		if err != nil {
			return err
		}
		raw, err := decodeMessageBlob(messageBlobSpace(root, tenant, r.hash), tenant, r.hash, r.encoding, rows)
		if err != nil {
			return err
		}
		out[r.hash] = raw
	}
	return nil
}

// ReconstructResourceSpans restores decomposed spans in the caller's
// transaction, reading every referenced blob in one pipelined wave.
func ReconstructResourceSpans(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, spans []*tracepb.ResourceSpans) ([]*tracepb.ResourceSpans, error) {
	const op = "message reconstruct"
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return nil, err
	}
	p, err := planMessageReconstruct(ctx, spans)
	if err != nil {
		return nil, err
	}
	raws := make(map[[32]byte][]byte, len(p.refs))
	if err := fetchMessageBlobs(ctx, rc, root, tenant, p.refs, raws); err != nil {
		return nil, err
	}
	return p.apply(raws)
}

// ReconstructResourceSpansDB is ReconstructResourceSpans over bounded
// transactions; immutable, digest-verified blobs need no common snapshot.
func ReconstructResourceSpansDB(ctx context.Context, db *recordlayer.FDBDatabase, root subspace.Subspace, tenant int, spans []*tracepb.ResourceSpans) ([]*tracepb.ResourceSpans, error) {
	const op = "message reconstruct"
	if db == nil {
		return nil, fail("config", op, "database required")
	}
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return nil, err
	}
	p, err := planMessageReconstruct(ctx, spans)
	if err != nil {
		return nil, err
	}
	raws := make(map[[32]byte][]byte, len(p.refs))
	for lo := 0; lo < len(p.refs); lo += messageReconstructWave {
		wave := p.refs[lo:min(lo+messageReconstructWave, len(p.refs))]
		_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			if err := configureRecordTimeout(rc); err != nil {
				return nil, err
			}
			return nil, fetchMessageBlobs(ctx, rc, root, tenant, wave, raws)
		})
		if err != nil {
			return nil, err
		}
	}
	return p.apply(raws)
}
