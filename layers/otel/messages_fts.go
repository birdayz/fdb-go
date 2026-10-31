package otel

// Full-text search over content-addressed GenAI messages (messages_delta.go).
//
// Tier 1 tokenizes each unique MessageBlob at most once per tenant: msg_fts_v1
// holds one posting per (token, message) and msg_meta_v1 marks the message as
// indexed. Tier 2, msg_span_v1, maps a message back to every (span, attribute)
// whose manifest references it, so an N-turn conversation costs O(N) token
// postings and O(N^2) compact span references instead of O(N^2) postings per
// token. msg_span_rev_v1 lists each span's references so a re-exported span
// replaces, rather than accumulates, its references.
//
// Positions are capped at maxPositionsPerToken per token per message and
// tokens longer than maxIndexedTokenBytes are not indexed; a phrase occurring
// only beyond the cap is not found. Queries verify every message they report
// against its digest-checked blob by re-tokenizing it, so a tampered posting,
// metadata row or blob is a CorruptionError rather than a wrong answer.

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"sort"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	msgMetaSubspace    = "msg_meta_v1"
	msgFTSSubspace     = "msg_fts_v1"
	msgSpanSubspace    = "msg_span_v1"
	msgSpanRevSubspace = "msg_span_rev_v1"

	msgFTSVersion        = 1
	maxPositionsPerToken = 1024
	maxIndexedTokenBytes = 512
	maxMessageTextDepth  = 10000 // proto.Unmarshal's default recursion limit
	maxMessageRoleBytes  = 1024
	maxMessageQueryBytes = 4096
	maxMessageQueryTerms = 16
	maxMessageSpanRefs   = 8192    // unique (attribute, message) references per span
	maxMessageValueBytes = 100_000 // FDB value limit
	msgMetaProbeWave     = 256     // metadata probes per PersistAndIndexMessageBlobsDB transaction
	msgSpanGroupSpans    = 64      // spans per IndexMessageSpansDB transaction
	msgRefScanRows       = 256     // span references per stream read
	msgSpanRefDomain     = "otel_msg_span_v1\x00"
	msgSpanRefSumBytes   = 16

	messageContinuationKind = "message_fts"
)

const (
	MessageMatchAll    = "all"
	MessageMatchAny    = "any"
	MessageMatchPhrase = "phrase"
)

// MessageFTSStats counts message full-text indexing work. Bytes are FDB key
// plus value bytes of committed index writes; blob bytes are in MessagePersistStats.
type MessageFTSStats struct {
	IndexedBlobs  int   `json:"indexed_blobs"`
	SkippedBlobs  int   `json:"skipped_blobs"`
	UniqueTokens  int   `json:"unique_tokens"`
	TokenPostings int   `json:"token_postings"`
	SpanRefs      int   `json:"span_refs"`
	Bytes         int64 `json:"bytes"`
	Transactions  int   `json:"transactions"`
}

func (s *MessageFTSStats) add(o MessageFTSStats) {
	s.IndexedBlobs += o.IndexedBlobs
	s.SkippedBlobs += o.SkippedBlobs
	s.UniqueTokens += o.UniqueTokens
	s.TokenPostings += o.TokenPostings
	s.SpanRefs += o.SpanRefs
	s.Bytes += o.Bytes
	s.Transactions += o.Transactions
}

// MessageFullTextQuery finds (span, attribute, message) references whose
// message text matches Query. Mode "all" (default) needs every term, "any"
// one term, and "phrase" every term at consecutive positions. Role and
// AttributeKey filter the message and the referencing attribute; Predicates
// must hold on the same span. FirstTurnOnly keeps references in which the
// message is the attribute's newest element, i.e. the span that introduced it.
type MessageFullTextQuery struct {
	Query         string      `json:"query"`
	Mode          string      `json:"mode,omitempty"`
	AttributeKey  string      `json:"attribute_key,omitempty"`
	Role          string      `json:"role,omitempty"`
	Predicates    []Attribute `json:"predicates,omitempty"`
	TimeWindow    *TimeRange  `json:"time_window,omitempty"`
	FirstTurnOnly bool        `json:"first_turn_only,omitempty"`
	Limit         int         `json:"limit,omitempty"`
	Continuation  []byte      `json:"continuation,omitempty"`
}

// MessageSpanHit is one verified reference. TurnOrdinals are the 0-based
// positions of the message in the attribute's manifest; PrefixChain is the
// manifest's rolling chain hash after its first occurrence. Positions are the
// matched word positions (phrase start positions in phrase mode).
type MessageSpanHit struct {
	Trace         string   `json:"trace_id"`
	Span          string   `json:"span_id"`
	Start         int64    `json:"start_ns"`
	AttributeKey  string   `json:"attribute_key"`
	Role          string   `json:"role,omitempty"`
	MessageHash   string   `json:"message_hash"`
	TurnOrdinals  []int    `json:"turn_ordinals"`
	PrefixChain   string   `json:"prefix_chain"`
	ManifestChain string   `json:"manifest_chain"`
	MatchedTerms  []string `json:"matched_terms"`
	Positions     []int    `json:"positions,omitempty"`
}

// MessageFullTextPage is one snapshot page in the run's order (span sort key,
// then attribute key, then message hash). A full page carries a continuation.
type MessageFullTextPage struct {
	Hits         []MessageSpanHit `json:"hits"`
	Continuation []byte           `json:"continuation,omitempty"`
	Order        string           `json:"order"`
	ReadVersion  int64            `json:"read_version"`
	Candidates   int              `json:"candidates"`
	Bytes        int64            `json:"logical_read_bytes"`
}

// ---------------------------------------------------------------------------
// Text extraction and tokenization
// ---------------------------------------------------------------------------

func normalizeMessageRole(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// messageStructuralKey names map fields whose values are metadata, not text.
func messageStructuralKey(k string) bool { return k == "role" || k == "type" }

// ExtractMessageText returns a message's normalized top-level role and its
// searchable text: every string value except those under "role" and "type"
// keys, in document order, joined by newlines. Both element encodings of the
// same message yield the same result.
func ExtractMessageText(b *MessageBlob) (role string, text string, err error) {
	if b == nil {
		return "", "", fail("admission", "message text", "nil message blob")
	}
	return extractMessageEncodingText(b.encoding, b.raw)
}

func extractMessageEncodingText(encoding string, raw []byte) (string, string, error) {
	const op = "message text"
	switch encoding {
	case messageEncodingAny:
		v := &commonpb.AnyValue{}
		if err := proto.Unmarshal(raw, v); err != nil {
			return "", "", typedError("corruption", op, "message element decode", err)
		}
		role := ""
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

// TokenizeMessageText tokenizes text with the Record Layer default tokenizer
// (UAX #29 words, NFKD, lowercase, marks stripped) into 0-based word
// positions, dropping tokens over maxIndexedTokenBytes and keeping at most
// maxPositionsPerToken positions per token.
func TokenizeMessageText(text string) (map[string][]int, error) {
	m, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToMap(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil {
		return nil, typedError("config", "message tokenize", "default tokenizer", err)
	}
	for tok, pos := range m {
		if tok == "" || len(tok) > maxIndexedTokenBytes {
			delete(m, tok)
			continue
		}
		if len(pos) > maxPositionsPerToken {
			m[tok] = pos[:maxPositionsPerToken:maxPositionsPerToken]
		}
	}
	return m, nil
}

// TokenizeQueryText returns a query's tokens in order. A token longer than
// maxIndexedTokenBytes is a ConfigError: it is never indexed, so no answer
// for it would be exhaustive.
func TokenizeQueryText(text string) ([]string, error) {
	terms, err := tokenizeQueryTerms(text)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = t.term
	}
	return out, nil
}

type messagePhraseTerm struct {
	term   string
	offset int
}

func tokenizeQueryTerms(text string) ([]messagePhraseTerm, error) {
	const op = "message query"
	if len(text) > maxMessageQueryBytes {
		return nil, failf("config", op, "query exceeds %d bytes", maxMessageQueryBytes)
	}
	list, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(text, 0, recordlayer.TokenizerModeQuery)
	if err != nil {
		return nil, typedError("config", op, "default tokenizer", err)
	}
	var out []messagePhraseTerm
	for i, tok := range list {
		if tok == "" {
			continue
		}
		if len(tok) > maxIndexedTokenBytes {
			return nil, failf("config", op, "query term exceeds the %d-byte indexed token cap", maxIndexedTokenBytes)
		}
		out = append(out, messagePhraseTerm{tok, i})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Tier 1 rows
// ---------------------------------------------------------------------------

func messageMetaKey(root subspace.Subspace, tenant int, hash [32]byte) fdb.Key {
	return root.Sub(msgMetaSubspace, int64(tenant), hash[:]).FDBKey()
}

func messageFTSTerm(root subspace.Subspace, tenant int, token string) subspace.Subspace {
	return root.Sub(msgFTSSubspace, int64(tenant), token)
}

func validMessageEncoding(e string) bool { return e == messageEncodingAny || e == messageEncodingJSON }

func encodeMessagePosting(encoding, role string, positions []int) []byte {
	out := make([]byte, 0, 4+len(encoding)+len(role)+2*len(positions))
	out = append(out, msgFTSVersion)
	out = binary.AppendUvarint(out, uint64(len(encoding)))
	out = append(out, encoding...)
	out = binary.AppendUvarint(out, uint64(len(role)))
	out = append(out, role...)
	out = binary.AppendUvarint(out, uint64(len(positions)))
	prev := 0
	for i, p := range positions {
		if i == 0 {
			out = binary.AppendUvarint(out, uint64(p))
		} else {
			out = binary.AppendUvarint(out, uint64(p-prev))
		}
		prev = p
	}
	return out
}

func decodeMessagePosting(v []byte) (encoding, role string, positions []int, err error) {
	r := &byteReader{b: v}
	version := r.byte1()
	encoding = string(r.take(r.uvarint(32)))
	role = string(r.take(r.uvarint(maxMessageRoleBytes)))
	n := r.uvarint(maxPositionsPerToken)
	positions = make([]int, n)
	for i := range positions {
		d := r.uvarint(math.MaxInt32)
		switch {
		case i == 0:
			positions[i] = d
		case d == 0:
			r.bad = true
		default:
			positions[i] = positions[i-1] + d
		}
	}
	if r.bad || len(r.b) != 0 || version != msgFTSVersion || n == 0 || !validMessageEncoding(encoding) {
		return "", "", nil, fail("corruption", "message posting", "malformed message token posting")
	}
	return encoding, role, positions, nil
}

func encodeMessageMeta(encoding, role string, tokens int) []byte {
	return tuple.Tuple{int64(msgFTSVersion), encoding, role, int64(tokens)}.Pack()
}

func decodeMessageMeta(v []byte) (encoding, role string, tokens int, err error) {
	bad := fail("corruption", "message metadata", "malformed message metadata")
	t, err := tuple.Unpack(v)
	if err != nil || len(t) != 4 {
		return "", "", 0, bad
	}
	version, vok := t[0].(int64)
	encoding, eok := t[1].(string)
	role, rok := t[2].(string)
	n, nok := t[3].(int64)
	if !vok || !eok || !rok || !nok || version != msgFTSVersion || !validMessageEncoding(encoding) || n < 0 || n > math.MaxInt32 {
		return "", "", 0, bad
	}
	return encoding, role, int(n), nil
}

type messageFTSWrite struct {
	key   fdb.Key
	value []byte
}

// messageFTSRows tokenizes one blob into its posting rows, sorted by token,
// followed by its metadata row, which therefore commits last.
func messageFTSRows(root subspace.Subspace, tenant int, b *MessageBlob) ([]messageFTSWrite, []string, error) {
	const op = "message fts index"
	role, text, err := extractMessageEncodingText(b.encoding, b.raw)
	if err != nil {
		return nil, nil, err
	}
	if len(role) > maxMessageRoleBytes {
		return nil, nil, failf("admission", op, "message role of %d bytes exceeds %d", len(role), maxMessageRoleBytes)
	}
	tokens, err := TokenizeMessageText(text)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(tokens))
	for tok := range tokens {
		keys = append(keys, tok)
	}
	sort.Strings(keys)
	rows := make([]messageFTSWrite, 0, len(keys)+1)
	for _, tok := range keys {
		key := messageFTSTerm(root, tenant, tok).Sub(b.hash[:]).FDBKey()
		value := encodeMessagePosting(b.encoding, role, tokens[tok])
		if len(key) > maxMessageKeyBytes || len(value) > maxMessageValueBytes {
			return nil, nil, failf("admission", op, "token posting of %d/%d bytes exceeds FDB limits", len(key), len(value))
		}
		rows = append(rows, messageFTSWrite{key, value})
	}
	rows = append(rows, messageFTSWrite{messageMetaKey(root, tenant, b.hash), encodeMessageMeta(b.encoding, role, len(keys))})
	return rows, keys, nil
}

func checkMessageFTSRoot(op string, root subspace.Subspace, tenant int) error {
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return err
	}
	var zero [32]byte
	widest := messageFTSTerm(root, tenant, strings.Repeat("\xff", maxIndexedTokenBytes)).Sub(zero[:]).Bytes()
	if len(widest) > maxMessageKeyBytes {
		return failf("config", op, "message token key of %d bytes exceeds %d", len(widest), maxMessageKeyBytes)
	}
	return nil
}

// probeMessageMeta reports which blobs already have version-1 metadata.
func probeMessageMeta(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, blobs []*MessageBlob) ([]bool, error) {
	const op = "message fts index"
	if err := canceled(ctx, op); err != nil {
		return nil, err
	}
	snap := rc.ReadTransaction(true)
	pending := make([]fdb.FutureByteSlice, len(blobs))
	for i, b := range blobs {
		pending[i] = snap.Get(messageMetaKey(root, tenant, b.hash))
	}
	out := make([]bool, len(blobs))
	for i, b := range blobs {
		v, err := pending[i].Get()
		if err != nil {
			return nil, err
		}
		if len(v) == 0 {
			continue
		}
		enc, _, _, err := decodeMessageMeta(v)
		if err != nil {
			return nil, err
		}
		if enc != b.encoding {
			return nil, fail("corruption", "message metadata", "metadata encoding disagrees with the message digest")
		}
		out[i] = true
	}
	return out, nil
}

func writeMessageFTSRows(rc *recordlayer.FDBRecordContext, rows []messageFTSWrite) int64 {
	var n int64
	for _, r := range rows {
		rc.Transaction().Set(r.key, r.value)
		n += int64(len(r.key) + len(r.value))
	}
	return n
}

// PersistAndIndexMessageBlobs persists blobs and indexes every blob without
// metadata in the caller's transaction; blob and index writes together must
// fit the 300KB budget, otherwise use PersistAndIndexMessageBlobsDB.
func PersistAndIndexMessageBlobs(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, blobs []*MessageBlob) (MessagePersistStats, MessageFTSStats, error) {
	const op = "message fts index"
	var fts MessageFTSStats
	if rc == nil {
		return MessagePersistStats{}, fts, fail("config", op, "record context required")
	}
	if err := checkMessageFTSRoot(op, root, tenant); err != nil {
		return MessagePersistStats{}, fts, err
	}
	unique, err := admitMessageBlobs(op, tenant, blobs)
	if err != nil {
		return MessagePersistStats{}, fts, err
	}
	indexed, err := probeMessageMeta(ctx, rc, root, tenant, unique)
	if err != nil {
		return MessagePersistStats{}, fts, err
	}
	total := 0
	var rows []messageFTSWrite
	tokens := map[string]bool{}
	for i, b := range unique {
		total += messageBlobWriteBytes(root, tenant, b)
		if indexed[i] {
			fts.SkippedBlobs++
			continue
		}
		r, toks, err := messageFTSRows(root, tenant, b)
		if err != nil {
			return MessagePersistStats{}, MessageFTSStats{}, err
		}
		for _, w := range r {
			total += len(w.key) + len(w.value)
		}
		for _, t := range toks {
			tokens[t] = true
		}
		rows = append(rows, r...)
		fts.IndexedBlobs++
		fts.TokenPostings += len(r) - 1
	}
	if total > messageTxnBudgetBytes {
		return MessagePersistStats{}, MessageFTSStats{}, failf("admission", op, "message blobs and postings of %d bytes exceed the %d-byte transaction budget; use PersistAndIndexMessageBlobsDB", total, messageTxnBudgetBytes)
	}
	persist, err := persistMessageBlobsTxn(ctx, rc, root, tenant, unique)
	if err != nil {
		return persist, MessageFTSStats{}, err
	}
	fts.UniqueTokens = len(tokens)
	if len(rows) > 0 {
		fts.Bytes = writeMessageFTSRows(rc, rows)
		if err := messageTxnGuard(op, rc); err != nil {
			return persist, MessageFTSStats{}, err
		}
	}
	return persist, fts, nil
}

// PersistAndIndexMessageBlobsDB persists blobs with PersistMessageBlobsDB and
// tokenizes only blobs without metadata, in bounded transactions. A blob's
// metadata commits after all of its postings, so an interrupted call is
// completed by the next one.
func PersistAndIndexMessageBlobsDB(ctx context.Context, db *recordlayer.FDBDatabase, root subspace.Subspace, tenant int, blobs []*MessageBlob) (MessagePersistStats, MessageFTSStats, error) {
	const op = "message fts index"
	var fts MessageFTSStats
	if db == nil {
		return MessagePersistStats{}, fts, fail("config", op, "database required")
	}
	if err := checkMessageFTSRoot(op, root, tenant); err != nil {
		return MessagePersistStats{}, fts, err
	}
	unique, err := admitMessageBlobs(op, tenant, blobs)
	if err != nil {
		return MessagePersistStats{}, fts, err
	}
	indexed := make([]bool, len(unique))
	for lo := 0; lo < len(unique); lo += msgMetaProbeWave {
		wave := unique[lo:min(lo+msgMetaProbeWave, len(unique))]
		var got []bool
		_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			if err := configureStoreContext(rc); err != nil {
				return nil, err
			}
			var err error
			got, err = probeMessageMeta(ctx, rc, root, tenant, wave)
			return nil, err
		})
		if err != nil {
			return MessagePersistStats{}, fts, err
		}
		copy(indexed[lo:], got)
		fts.Transactions++
	}
	persist, err := PersistMessageBlobsDB(ctx, db, root, tenant, unique)
	if err != nil {
		return persist, fts, err
	}
	var batch []messageFTSWrite
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		rows := batch
		batch, size = nil, 0
		var n int64
		_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			if err := configureStoreContext(rc); err != nil {
				return nil, err
			}
			if err := canceled(ctx, op); err != nil {
				return nil, err
			}
			n = writeMessageFTSRows(rc, rows)
			return nil, messageTxnGuard(op, rc)
		})
		if err == nil {
			fts.Bytes += n
			fts.Transactions++
		}
		return err
	}
	tokens := map[string]bool{}
	for i, b := range unique {
		if indexed[i] {
			fts.SkippedBlobs++
			continue
		}
		rows, toks, err := messageFTSRows(root, tenant, b)
		if err != nil {
			return persist, fts, err
		}
		for _, r := range rows {
			n := len(r.key) + len(r.value)
			if size+n > messageFTSSplitBytes {
				if err := flush(); err != nil {
					return persist, fts, err
				}
			}
			batch = append(batch, r)
			size += n
		}
		for _, t := range toks {
			tokens[t] = true
		}
		fts.IndexedBlobs++
		fts.TokenPostings += len(rows) - 1
	}
	if err := flush(); err != nil {
		return persist, fts, err
	}
	fts.UniqueTokens = len(tokens)
	return persist, fts, nil
}

// ---------------------------------------------------------------------------
// Tier 2 rows
// ---------------------------------------------------------------------------

// messageSpanRef is one (span, attribute, message) back reference.
type messageSpanRef struct {
	hash     [32]byte
	trace    string
	span     string
	start    int64
	attrKey  string
	encoding string
	ordinals []int
	count    int // elements in the attribute's manifest
	prefix   [32]byte
	chain    [32]byte
}

func messageSpanRefSpace(root subspace.Subspace, tenant int, hash [32]byte) subspace.Subspace {
	return root.Sub(msgSpanSubspace, int64(tenant), hash[:])
}

func messageSpanRevSpace(root subspace.Subspace, tenant int, trace, span string) subspace.Subspace {
	return root.Sub(msgSpanRevSubspace, int64(tenant), trace, span)
}

func messageRefSuffix(trace, span string, start int64, attrKey, order string) tuple.Tuple {
	return append(postingSuffix(Span{Trace: trace, ID: span, Start: start}, order), attrKey)
}

func messageSpanRefSum(key, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(msgSpanRefDomain))
	h.Write(binary.AppendUvarint(nil, uint64(len(key))))
	h.Write(key)
	h.Write(body)
	return h.Sum(nil)[:msgSpanRefSumBytes]
}

// encodeMessageSpanRef binds the value to its key with a SHA-256 checksum.
func encodeMessageSpanRef(key []byte, r *messageSpanRef) []byte {
	out := make([]byte, 0, 96+len(r.attrKey)+2*len(r.ordinals))
	out = append(out, msgFTSVersion)
	out = binary.AppendUvarint(out, uint64(r.start))
	out = binary.AppendUvarint(out, uint64(len(r.attrKey)))
	out = append(out, r.attrKey...)
	out = binary.AppendUvarint(out, uint64(len(r.encoding)))
	out = append(out, r.encoding...)
	out = binary.AppendUvarint(out, uint64(r.count))
	out = binary.AppendUvarint(out, uint64(len(r.ordinals)))
	prev := 0
	for i, o := range r.ordinals {
		if i == 0 {
			out = binary.AppendUvarint(out, uint64(o))
		} else {
			out = binary.AppendUvarint(out, uint64(o-prev))
		}
		prev = o
	}
	out = append(out, r.prefix[:]...)
	out = append(out, r.chain[:]...)
	return append(out, messageSpanRefSum(key, out)...)
}

// decodeMessageSpanRef verifies one msg_span_v1 row below prefix and returns
// the reference and its sort suffix.
func decodeMessageSpanRef(prefix subspace.Subspace, hash [32]byte, order string, kv fdb.KeyValue) (*messageSpanRef, tuple.Tuple, error) {
	bad := func(msg string) (*messageSpanRef, tuple.Tuple, error) {
		return nil, nil, fail("corruption", "message span reference", msg)
	}
	if len(kv.Value) < 1+msgSpanRefSumBytes {
		return bad("truncated reference")
	}
	body, sum := kv.Value[:len(kv.Value)-msgSpanRefSumBytes], kv.Value[len(kv.Value)-msgSpanRefSumBytes:]
	if !bytes.Equal(sum, messageSpanRefSum(kv.Key, body)) {
		return bad("reference checksum mismatch")
	}
	k, err := prefix.Unpack(kv.Key)
	if err != nil {
		return bad("malformed reference key")
	}
	ref := &messageSpanRef{hash: hash}
	keyAttr, keyStart, ok := "", int64(-1), false
	if order == "time" {
		if len(k) != 4 {
			return bad("malformed time reference key")
		}
		neg, nok := k[0].(int64)
		rev, rok := k[1].([]byte)
		span, sok := k[2].(string)
		attr, aok := k[3].(string)
		ok = nok && rok && sok && aok && neg <= 0
		ref.trace, ref.span, keyAttr, keyStart = restoreTrace(rev), span, attr, -neg
	} else {
		if len(k) != 3 {
			return bad("malformed trace reference key")
		}
		trace, tok := k[0].(string)
		span, sok := k[1].(string)
		attr, aok := k[2].(string)
		ok = tok && sok && aok
		ref.trace, ref.span, keyAttr = trace, span, attr
	}
	if !ok || !validIdentity(ref.trace, 16) || !validIdentity(ref.span, 8) || !messageDeltaKeys[keyAttr] {
		return bad("malformed reference identity")
	}
	r := &byteReader{b: body}
	version := r.byte1()
	start := r.uvarint(math.MaxInt64)
	ref.attrKey = string(r.take(r.uvarint(maxIndexedKeyBytes)))
	ref.encoding = string(r.take(r.uvarint(32)))
	ref.count = r.uvarint(maxMessageManifest / 32)
	ref.ordinals = make([]int, r.uvarint(uint64(ref.count)))
	for i := range ref.ordinals {
		d := r.uvarint(uint64(ref.count))
		switch {
		case i == 0:
			ref.ordinals[i] = d
		case d == 0:
			r.bad = true
		default:
			ref.ordinals[i] = ref.ordinals[i-1] + d
		}
	}
	copy(ref.prefix[:], r.take(32))
	copy(ref.chain[:], r.take(32))
	ref.start = int64(start)
	switch {
	case r.bad || len(r.b) != 0 || version != msgFTSVersion || !validMessageEncoding(ref.encoding):
		return bad("malformed reference value")
	case len(ref.ordinals) == 0 || ref.ordinals[len(ref.ordinals)-1] >= ref.count:
		return bad("reference ordinals outside the manifest")
	case ref.attrKey != keyAttr || (order == "time" && ref.start != keyStart):
		return bad("reference value disagrees with its key")
	}
	return ref, k, nil
}

// resolveMessageManifest accepts a decomposed manifest (bytes, or the base64
// string an indexed copy carries) or a raw attribute value, which it splits
// and hashes exactly as DecomposeResourceSpans does.
func resolveMessageManifest(tenant int, v *commonpb.AnyValue) (*MessageDeltaManifest, error) {
	const op = "message span index"
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_BytesValue:
		if bytes.HasPrefix(x.BytesValue, []byte(messageManifestMagic)) {
			return ParseMessageDeltaManifest(v)
		}
	case *commonpb.AnyValue_StringValue:
		if wire, err := base64.StdEncoding.DecodeString(x.StringValue); err == nil && bytes.HasPrefix(wire, []byte(messageManifestMagic)) {
			if m, err := ParseMessageDeltaManifest(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: wire}}); err == nil {
				return m, nil
			}
		}
	}
	m, encoding, elems, err := splitMessageValue(v)
	if err != nil {
		return nil, err
	}
	m.Hashes = make([][32]byte, len(elems))
	for i, e := range elems {
		if len(e) > maxMessageBytes {
			return nil, failf("admission", op, "message element of %d bytes exceeds %d", len(e), maxMessageBytes)
		}
		m.Hashes[i] = messageHash(tenant, encoding, e)
	}
	m.Chain = messageChainHead(m.Hashes)
	if n := len(m.encode()); n > maxMessageManifest {
		return nil, failf("admission", op, "manifest of %d bytes exceeds %d", n, maxMessageManifest)
	}
	return m, nil
}

type messageSpanPlan struct {
	trace, span string
	start       int64
	refs        []*messageSpanRef
}

// planMessageSpans derives every span's references. Later duplicates of a
// span identity replace earlier ones, as in prepareExport. When one span
// references a message under the same attribute key at several sites (span
// and event attributes), the first site's manifest describes the reference.
func planMessageSpans(ctx context.Context, tenant int, spans []*tracepb.ResourceSpans) ([]*messageSpanPlan, error) {
	const op = "message span index"
	var out []*messageSpanPlan
	position := map[string]int{}
	for _, rs := range spans {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if sp == nil {
					continue
				}
				if err := canceled(ctx, op); err != nil {
					return nil, err
				}
				if !validSpanIdentity(sp.TraceId, 16) || !validSpanIdentity(sp.SpanId, 8) {
					return nil, fail("admission", op, "expected nonzero 16-byte trace and 8-byte span identifiers")
				}
				if sp.StartTimeUnixNano > math.MaxInt64 {
					return nil, fail("admission", op, "start_time_unix_nano exceeds int64")
				}
				p := &messageSpanPlan{trace: hex.EncodeToString(sp.TraceId), span: hex.EncodeToString(sp.SpanId), start: int64(sp.StartTimeUnixNano)}
				index := map[string]*messageSpanRef{}
				visit := func(kvs []*commonpb.KeyValue) error {
					for _, kv := range kvs {
						if kv == nil || !messageDeltaKeys[kv.Key] {
							continue
						}
						m, err := resolveMessageManifest(tenant, kv.Value)
						if err != nil {
							return err
						}
						chains := m.PrefixChains()
						owned := map[*messageSpanRef]bool{}
						for i, h := range m.Hashes {
							id := kv.Key + "\x00" + string(h[:])
							r := index[id]
							if r == nil {
								r = &messageSpanRef{hash: h, trace: p.trace, span: p.span, start: p.start, attrKey: kv.Key, encoding: manifestEncoding(m), count: len(m.Hashes), prefix: chains[i], chain: m.Chain}
								index[id] = r
								owned[r] = true
								p.refs = append(p.refs, r)
							}
							if owned[r] {
								r.ordinals = append(r.ordinals, i)
							}
						}
					}
					return nil
				}
				if err := visit(sp.Attributes); err != nil {
					return nil, err
				}
				for _, ev := range sp.Events {
					if err := visit(ev.GetAttributes()); err != nil {
						return nil, err
					}
				}
				if len(p.refs) > maxMessageSpanRefs {
					return nil, failf("admission", op, "span references %d messages, more than %d", len(p.refs), maxMessageSpanRefs)
				}
				if i, ok := position[p.trace+p.span]; ok {
					out[i] = p
				} else {
					position[p.trace+p.span] = len(out)
					out = append(out, p)
				}
			}
		}
	}
	return out, nil
}

// messageRefUnit sets or clears one reference and its reverse row together.
type messageRefUnit struct {
	clear      bool
	fwd, rev   fdb.Key
	fwdV, revV []byte
}

func (u messageRefUnit) size() int { return len(u.fwd) + len(u.rev) + len(u.fwdV) + len(u.revV) }

func messageRefSets(root subspace.Subspace, tenant int, order string, p *messageSpanPlan) ([]messageRefUnit, int, error) {
	const op = "message span index"
	rev := messageSpanRevSpace(root, tenant, p.trace, p.span)
	revV := tuple.Tuple{int64(msgFTSVersion), order, p.start}.Pack()
	out := make([]messageRefUnit, len(p.refs))
	size := 0
	for i, r := range p.refs {
		fwd := messageSpanRefSpace(root, tenant, r.hash).Pack(messageRefSuffix(p.trace, p.span, p.start, r.attrKey, order))
		u := messageRefUnit{fwd: fwd, fwdV: encodeMessageSpanRef(fwd, r), rev: rev.Pack(tuple.Tuple{r.hash[:], r.attrKey}), revV: revV}
		if len(u.fwd) > maxMessageKeyBytes || len(u.rev) > maxMessageKeyBytes || len(u.fwdV) > maxMessageValueBytes {
			return nil, 0, failf("admission", op, "span reference of %d/%d bytes exceeds FDB limits", len(u.fwd), len(u.fwdV))
		}
		out[i] = u
		size += u.size()
	}
	return out, size, nil
}

// staleMessageRefs returns clears for a span's stored references that its new
// plan no longer has under the same order and start.
func staleMessageRefs(root subspace.Subspace, tenant int, order string, p *messageSpanPlan, rows []fdb.KeyValue) ([]messageRefUnit, error) {
	bad := fail("corruption", "message span reverse index", "malformed reverse reference")
	rev := messageSpanRevSpace(root, tenant, p.trace, p.span)
	current := make(map[string]bool, len(p.refs))
	for _, r := range p.refs {
		current[string(r.hash[:])+"\x00"+r.attrKey] = true
	}
	var out []messageRefUnit
	for _, kv := range rows {
		k, err := rev.Unpack(kv.Key)
		if err != nil || len(k) != 2 {
			return nil, bad
		}
		h, hok := k[0].([]byte)
		attr, aok := k[1].(string)
		v, err := tuple.Unpack(kv.Value)
		if err != nil || len(v) != 3 || !hok || !aok || len(h) != 32 || !messageDeltaKeys[attr] {
			return nil, bad
		}
		version, vok := v[0].(int64)
		oldOrder, ook := v[1].(string)
		oldStart, sok := v[2].(int64)
		if !vok || !ook || !sok || version != msgFTSVersion || (oldOrder != "time" && oldOrder != "trace") || oldStart < 0 {
			return nil, bad
		}
		if current[string(h)+"\x00"+attr] && oldOrder == order && oldStart == p.start {
			continue
		}
		var hash [32]byte
		copy(hash[:], h)
		fwd := messageSpanRefSpace(root, tenant, hash).Pack(messageRefSuffix(p.trace, p.span, oldStart, attr, oldOrder))
		out = append(out, messageRefUnit{clear: true, fwd: fwd, rev: append(fdb.Key(nil), kv.Key...)})
	}
	return out, nil
}

func applyMessageRefUnits(rc *recordlayer.FDBRecordContext, units []messageRefUnit) MessageFTSStats {
	var s MessageFTSStats
	tx := rc.Transaction()
	for _, u := range units {
		if u.clear {
			tx.Clear(u.fwd)
			tx.Clear(u.rev)
		} else {
			tx.Set(u.fwd, u.fwdV)
			tx.Set(u.rev, u.revV)
			s.SpanRefs++
		}
		s.Bytes += int64(u.size())
	}
	return s
}

// indexMessageSpanGroup reads the group's reverse rows in one pipelined wave
// and computes stale clears followed by new sets. It applies them when they
// fit budget; otherwise it returns them unapplied.
func indexMessageSpanGroup(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, order string, group []*messageSpanPlan, sets [][]messageRefUnit, budget int) (MessageFTSStats, []messageRefUnit, error) {
	const op = "message span index"
	if err := canceled(ctx, op); err != nil {
		return MessageFTSStats{}, nil, err
	}
	tx := rc.Transaction()
	pending := make([]fdb.RangeResult, len(group))
	for i, p := range group {
		pending[i] = tx.GetRange(messageSpanRevSpace(root, tenant, p.trace, p.span), fdb.RangeOptions{Limit: maxMessageSpanRefs + 1, Mode: fdb.StreamingModeWantAll})
	}
	var units, added []messageRefUnit
	size := 0
	for i, p := range group {
		rows, err := pending[i].GetSliceWithError()
		if err != nil {
			return MessageFTSStats{}, nil, err
		}
		if len(rows) > maxMessageSpanRefs {
			return MessageFTSStats{}, nil, fail("corruption", op, "span has more reverse references than any admitted span")
		}
		stale, err := staleMessageRefs(root, tenant, order, p, rows)
		if err != nil {
			return MessageFTSStats{}, nil, err
		}
		units = append(units, stale...)
		added = append(added, sets[i]...)
	}
	// Clears precede sets so a reference whose start moved is rewritten, not lost.
	units = append(units, added...)
	for _, u := range units {
		size += u.size()
	}
	if size > budget {
		return MessageFTSStats{}, units, nil
	}
	s := applyMessageRefUnits(rc, units)
	if len(units) > 0 {
		if err := messageTxnGuard(op, rc); err != nil {
			return MessageFTSStats{}, nil, err
		}
	}
	return s, nil, nil
}

func checkMessageOrder(op, order string) error {
	if order != "time" && order != "trace" {
		return fail("config", op, "order must be time or trace")
	}
	return nil
}

// IndexMessageSpans writes the span references of spans in the caller's
// transaction, replacing each span's previous references. Values may be
// decomposed manifests or raw attribute values; the referenced blobs are
// indexed separately by PersistAndIndexMessageBlobs.
func IndexMessageSpans(ctx context.Context, rc *recordlayer.FDBRecordContext, root subspace.Subspace, tenant int, order string, spans []*tracepb.ResourceSpans) (MessageFTSStats, error) {
	const op = "message span index"
	if rc == nil {
		return MessageFTSStats{}, fail("config", op, "record context required")
	}
	if err := checkMessageOrder(op, order); err != nil {
		return MessageFTSStats{}, err
	}
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return MessageFTSStats{}, err
	}
	plans, err := planMessageSpans(ctx, tenant, spans)
	if err != nil {
		return MessageFTSStats{}, err
	}
	sets := make([][]messageRefUnit, len(plans))
	for i, p := range plans {
		if sets[i], _, err = messageRefSets(root, tenant, order, p); err != nil {
			return MessageFTSStats{}, err
		}
	}
	s, overflow, err := indexMessageSpanGroup(ctx, rc, root, tenant, order, plans, sets, messageTxnBudgetBytes)
	if err != nil {
		return MessageFTSStats{}, err
	}
	if overflow != nil {
		return MessageFTSStats{}, failf("admission", op, "span references exceed the %d-byte transaction budget; use IndexMessageSpansDB", messageTxnBudgetBytes)
	}
	return s, nil
}

// IndexMessageSpansDB is IndexMessageSpans over bounded transactions. A group
// whose writes fit one transaction reads and replaces its references
// atomically; a larger one commits its clears and sets in later transactions,
// each reference together with its reverse row.
func IndexMessageSpansDB(ctx context.Context, db *recordlayer.FDBDatabase, root subspace.Subspace, tenant int, order string, spans []*tracepb.ResourceSpans) (MessageFTSStats, error) {
	const op = "message span index"
	var stats MessageFTSStats
	if db == nil {
		return stats, fail("config", op, "database required")
	}
	if err := checkMessageOrder(op, order); err != nil {
		return stats, err
	}
	if err := checkMessageRoot(op, root, tenant); err != nil {
		return stats, err
	}
	plans, err := planMessageSpans(ctx, tenant, spans)
	if err != nil {
		return stats, err
	}
	run := func(fn func(*recordlayer.FDBRecordContext) (MessageFTSStats, error)) error {
		var got MessageFTSStats
		_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			if err := configureStoreContext(rc); err != nil {
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
	writeChunks := func(units []messageRefUnit) error {
		for lo := 0; lo < len(units); {
			hi, size := lo, 0
			for hi < len(units) && (hi == lo || size+units[hi].size() <= messageFTSSplitBytes) {
				size += units[hi].size()
				hi++
			}
			chunk := units[lo:hi]
			if err := run(func(rc *recordlayer.FDBRecordContext) (MessageFTSStats, error) {
				if err := canceled(ctx, op); err != nil {
					return MessageFTSStats{}, err
				}
				s := applyMessageRefUnits(rc, chunk)
				return s, messageTxnGuard(op, rc)
			}); err != nil {
				return err
			}
			lo = hi
		}
		return nil
	}
	var group []*messageSpanPlan
	var groupSets [][]messageRefUnit
	size := 0
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		g, gs := group, groupSets
		group, groupSets, size = nil, nil, 0
		var overflow []messageRefUnit
		if err := run(func(rc *recordlayer.FDBRecordContext) (MessageFTSStats, error) {
			s, o, err := indexMessageSpanGroup(ctx, rc, root, tenant, order, g, gs, messageFTSSplitBytes)
			overflow = o
			return s, err
		}); err != nil {
			return err
		}
		return writeChunks(overflow)
	}
	for _, p := range plans {
		sets, n, err := messageRefSets(root, tenant, order, p)
		if err != nil {
			return stats, err
		}
		if len(group) > 0 && (len(group) == msgSpanGroupSpans || size+n > messageFTSSplitBytes) {
			if err := flush(); err != nil {
				return stats, err
			}
		}
		group = append(group, p)
		groupSets = append(groupSets, sets)
		size += n
	}
	return stats, flush()
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

type messageQueryPlan struct {
	mode      string
	terms     []string // distinct, in query order
	phrase    []messagePhraseTerm
	attrKey   string
	role      string
	preds     []Attribute
	window    timeWindow
	firstTurn bool
	limit     int
	digest    []byte
}

func newMessageQueryPlan(op string, q MessageFullTextQuery, k int) (*messageQueryPlan, error) {
	mode := q.Mode
	if mode == "" {
		mode = MessageMatchAll
	}
	if mode != MessageMatchAll && mode != MessageMatchAny && mode != MessageMatchPhrase {
		return nil, failf("config", op, "unsupported message match mode %q", q.Mode)
	}
	phrase, err := tokenizeQueryTerms(q.Query)
	if err != nil {
		return nil, err
	}
	if len(phrase) == 0 || len(phrase) > maxMessageQueryTerms {
		return nil, failf("config", op, "expected 1..%d searchable query terms", maxMessageQueryTerms)
	}
	p := &messageQueryPlan{mode: mode, phrase: phrase, attrKey: q.AttributeKey, role: normalizeMessageRole(q.Role), firstTurn: q.FirstTurnOnly}
	base := phrase[0].offset
	seen := map[string]bool{}
	for i := range p.phrase {
		p.phrase[i].offset -= base
		if t := p.phrase[i].term; !seen[t] {
			seen[t] = true
			p.terms = append(p.terms, t)
		}
	}
	if q.AttributeKey != "" && !messageDeltaKeys[q.AttributeKey] {
		return nil, failf("config", op, "attribute_key %q is not a GenAI message attribute", q.AttributeKey)
	}
	if len(q.Role) > maxMessageRoleBytes {
		return nil, failf("config", op, "role exceeds %d bytes", maxMessageRoleBytes)
	}
	p.preds = uniqueAttributes(q.Predicates)
	if len(q.Predicates) > maxQueryPredicates {
		return nil, failf("config", op, "expected at most %d predicates", maxQueryPredicates)
	}
	for _, a := range p.preds {
		if err := validateQueryAttribute(op, a, "resource", "scope", "span"); err != nil {
			return nil, err
		}
	}
	if q.Limit < 0 {
		return nil, fail("config", op, "limit must not be negative")
	}
	if p.window, err = newTimeWindow(q.TimeWindow); err != nil {
		return nil, err
	}
	p.limit = q.Limit
	if p.limit == 0 {
		p.limit = k
	}
	t := tuple.Tuple{messageContinuationKind, mode, int64(len(p.phrase))}
	if mode == MessageMatchPhrase {
		for _, pt := range p.phrase {
			t = append(t, pt.term, int64(pt.offset))
		}
	} else {
		sorted := append([]string(nil), p.terms...)
		sort.Strings(sorted)
		t = append(t, int64(len(sorted)))
		for _, s := range sorted {
			t = append(t, s)
		}
	}
	t = append(t, p.attrKey, p.role, p.firstTurn, queryDigest(messageContinuationKind, p.preds, p.window))
	h := sha256.Sum256(t.Pack())
	p.digest = h[:]
	return p, nil
}

type messageCandidate struct {
	hash      [32]byte
	encoding  string
	role      string
	postings  map[string][]int
	matched   []string
	positions []int
	verified  bool
}

func containsSorted(xs []int, v int) bool {
	i := sort.SearchInts(xs, v)
	return i < len(xs) && xs[i] == v
}

// match applies the plan's mode to a candidate's postings.
func (p *messageQueryPlan) match(c *messageCandidate) bool {
	c.matched, c.positions = nil, nil
	for _, t := range p.terms {
		if _, ok := c.postings[t]; ok {
			c.matched = append(c.matched, t)
		}
	}
	if len(c.matched) == 0 || (p.mode != MessageMatchAny && len(c.matched) != len(p.terms)) {
		return false
	}
	if p.mode == MessageMatchPhrase {
		for _, s := range c.postings[p.phrase[0].term] {
			ok := true
			for _, pt := range p.phrase[1:] {
				if !containsSorted(c.postings[pt.term], s+pt.offset) {
					ok = false
					break
				}
			}
			if ok {
				c.positions = append(c.positions, s)
			}
		}
		return len(c.positions) > 0
	}
	for _, t := range c.matched {
		c.positions = append(c.positions, c.postings[t]...)
	}
	sort.Ints(c.positions)
	return true
}

// matchMessageBlobs reads every query term's postings in one pipelined wave
// and returns the matching messages ordered by hash.
func matchMessageBlobs(tx fdb.ReadTransaction, root subspace.Subspace, tenant int, plan *messageQueryPlan, budget *queryBudget) ([]*messageCandidate, error) {
	prefixes := make([]subspace.Subspace, len(plan.terms))
	pending := make([]fdb.RangeResult, len(plan.terms))
	limit := max(budget.maxCandidates-budget.candidates, 0) + 1
	for i, term := range plan.terms {
		prefixes[i] = messageFTSTerm(root, tenant, term)
		pending[i] = tx.GetRange(prefixes[i], fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})
	}
	byHash := map[[32]byte]*messageCandidate{}
	for i, term := range plan.terms {
		rows, err := pending[i].GetSliceWithError()
		if err != nil {
			return nil, err
		}
		for _, kv := range rows {
			if err := budget.charge(kv); err != nil {
				return nil, err
			}
			k, err := prefixes[i].Unpack(kv.Key)
			if err != nil || len(k) != 1 {
				return nil, fail("corruption", "message posting", "malformed message posting key")
			}
			hb, ok := k[0].([]byte)
			if !ok || len(hb) != 32 {
				return nil, fail("corruption", "message posting", "malformed message digest in posting key")
			}
			enc, role, pos, err := decodeMessagePosting(kv.Value)
			if err != nil {
				return nil, err
			}
			var h [32]byte
			copy(h[:], hb)
			c := byHash[h]
			if c == nil {
				c = &messageCandidate{hash: h, encoding: enc, role: role, postings: map[string][]int{}}
				byHash[h] = c
			} else if c.encoding != enc || c.role != role {
				return nil, fail("corruption", "message posting", "postings of one message disagree on encoding or role")
			}
			c.postings[term] = pos
		}
	}
	out := make([]*messageCandidate, 0, len(byHash))
	for _, c := range byHash {
		if (plan.role == "" || c.role == plan.role) && plan.match(c) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].hash[:], out[j].hash[:]) < 0 })
	return out, nil
}

// verifyMessageCandidates checks each message's metadata and digest-verified
// blob, re-tokenizes it and requires its postings for every query term to be
// exactly the recomputed ones. Reads for all candidates are issued first.
func verifyMessageCandidates(tx fdb.ReadTransaction, root subspace.Subspace, tenant int, plan *messageQueryPlan, cands []*messageCandidate, budget *queryBudget) error {
	const op = "message verify"
	metaKeys := make([]fdb.Key, len(cands))
	metas := make([]fdb.FutureByteSlice, len(cands))
	blobs := make([]fdb.RangeResult, len(cands))
	limit := messageChunkCount(maxMessageBytes) + 1
	for i, c := range cands {
		metaKeys[i] = messageMetaKey(root, tenant, c.hash)
		metas[i] = tx.Get(metaKeys[i])
		blobs[i] = tx.GetRange(messageBlobSpace(root, tenant, c.hash), fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})
	}
	for i, c := range cands {
		meta, err := metas[i].Get()
		if err != nil {
			return err
		}
		if err := budget.charge(fdb.KeyValue{Key: metaKeys[i], Value: meta}); err != nil {
			return err
		}
		if len(meta) == 0 {
			return failf("corruption", op, "message %x has postings but no metadata", c.hash[:8])
		}
		enc, role, n, err := decodeMessageMeta(meta)
		if err != nil {
			return err
		}
		if enc != c.encoding || role != c.role {
			return failf("corruption", op, "message %x metadata disagrees with its postings", c.hash[:8])
		}
		rows, err := blobs[i].GetSliceWithError()
		if err != nil {
			return err
		}
		for _, kv := range rows {
			if err := budget.charge(kv); err != nil {
				return err
			}
		}
		raw, err := decodeMessageBlob(messageBlobSpace(root, tenant, c.hash), tenant, c.hash, c.encoding, rows)
		if err != nil {
			return err
		}
		gotRole, text, err := extractMessageEncodingText(c.encoding, raw)
		if err != nil {
			return err
		}
		tokens, err := TokenizeMessageText(text)
		if err != nil {
			return err
		}
		if gotRole != c.role || len(tokens) != n {
			return failf("corruption", op, "message %x role or token count disagrees with its index", c.hash[:8])
		}
		for _, t := range plan.terms {
			want, has := tokens[t]
			got, posted := c.postings[t]
			if has != posted || !slices.Equal(want, got) {
				return failf("corruption", op, "message %x posting for a query term disagrees with its text", c.hash[:8])
			}
		}
		c.verified = true
	}
	return nil
}

type messageRefStream struct {
	cand       *messageCandidate
	prefix     subspace.Subspace
	plen       int
	begin, end fdb.Key
	rows       []fdb.KeyValue
	done       bool
}

func (s *messageRefStream) read(tx fdb.ReadTransaction, n int) fdb.RangeResult {
	return tx.GetRange(fdb.KeyRange{Begin: s.begin, End: s.end}, fdb.RangeOptions{Limit: n, Mode: fdb.StreamingModeWantAll})
}

func (s *messageRefStream) accept(r fdb.RangeResult, n int, budget *queryBudget) error {
	rows, err := r.GetSliceWithError()
	if err != nil {
		return err
	}
	for _, kv := range rows {
		if err := budget.charge(kv); err != nil {
			return err
		}
	}
	s.rows = rows
	if len(rows) < n {
		s.done = true
	} else {
		s.begin = append(append(fdb.Key(nil), rows[len(rows)-1].Key...), 0)
	}
	return nil
}

// messageRefHeap merges streams by (sort suffix, message hash).
type messageRefHeap []*messageRefStream

func (h messageRefHeap) Len() int { return len(h) }
func (h messageRefHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if c := bytes.Compare(a.rows[0].Key[a.plen:], b.rows[0].Key[b.plen:]); c != 0 {
		return c < 0
	}
	return bytes.Compare(a.cand.hash[:], b.cand.hash[:]) < 0
}
func (h messageRefHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *messageRefHeap) Push(x any)   { *h = append(*h, x.(*messageRefStream)) }
func (h *messageRefHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type messageResume struct {
	suffix tuple.Tuple
	hash   [32]byte
}

// parseMessageResume validates an authenticated continuation's sort key.
func parseMessageResume(order string, raw []byte) (*messageResume, error) {
	bad := fail("corruption", "continuation", "malformed message continuation sort key")
	t, err := tuple.Unpack(raw)
	if err != nil {
		return nil, bad
	}
	n := 4
	if order == "time" {
		n = 5
	}
	if len(t) != n {
		return nil, bad
	}
	hb, ok := t[n-1].([]byte)
	attr, aok := t[n-2].(string)
	span, sok := t[n-3].(string)
	if !ok || !aok || !sok || len(hb) != 32 || !messageDeltaKeys[attr] || !validIdentity(span, 8) {
		return nil, bad
	}
	if order == "time" {
		start, tok := t[0].(int64)
		rev, rok := t[1].([]byte)
		if !tok || !rok || start > 0 || !validIdentity(restoreTrace(rev), 16) {
			return nil, bad
		}
	} else if trace, tok := t[0].(string); !tok || !validIdentity(trace, 16) {
		return nil, bad
	}
	r := &messageResume{suffix: t[:n-1]}
	copy(r.hash[:], hb)
	return r, nil
}

func intersectTimeWindows(a, b timeWindow) timeWindow {
	if !a.set {
		return b
	}
	if !b.set {
		return a
	}
	w := timeWindow{set: true, lo: max(a.lo, b.lo), hi: min(a.hi, b.hi)}
	w.empty = a.empty || b.empty || w.hi <= w.lo
	return w
}

type messagePendingHit struct {
	cand   *messageCandidate
	ref    *messageSpanRef
	suffix tuple.Tuple
}

type messageProbeSpan struct {
	trace, span string
	start       int64
}

// probeMessagePredicates checks every predicate on every distinct span of
// the batch in one pipelined round of point probes.
func probeMessagePredicates(tx fdb.ReadTransaction, pm *postingMaintainer, tenant int, preds []Attribute, batch []messagePendingHit, budget *queryBudget) ([]bool, error) {
	index := map[messageProbeSpan]int{}
	var spans []messageProbeSpan
	for _, ph := range batch {
		k := messageProbeSpan{ph.ref.trace, ph.ref.span, ph.ref.start}
		if _, ok := index[k]; !ok {
			index[k] = len(spans)
			spans = append(spans, k)
		}
	}
	terms := make([]subspace.Subspace, len(preds))
	for j, a := range preds {
		terms[j] = termPrefix(pm.secondary, tenant, a, pm.width)
	}
	probes := make([][]fdb.RangeResult, len(spans))
	for i, s := range spans {
		suffix := postingSuffix(Span{Trace: s.trace, ID: s.span, Start: s.start}, pm.order)
		probes[i] = make([]fdb.RangeResult, len(preds))
		for j, term := range terms {
			r, err := fdb.PrefixRange(term.Pack(suffix))
			if err != nil {
				return nil, err
			}
			probes[i][j] = tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
		}
	}
	ok := make([]bool, len(spans))
	for i, s := range spans {
		ok[i] = true
		for j, term := range terms {
			rows, err := probes[i][j].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			found := false
			for _, kv := range rows {
				if err := budget.charge(kv); err != nil {
					return nil, err
				}
				m, a, err := decodeSpanPosting(pm, term, kv)
				if err != nil {
					return nil, err
				}
				if m.Trace != s.trace || m.ID != s.span {
					return nil, fail("corruption", "message predicate probe", "probe returned another span identity")
				}
				// A start that disagrees is a span re-exported since its
				// references were written: it does not satisfy this reference.
				found = found || (a == preds[j] && m.Start == s.start)
			}
			ok[i] = ok[i] && found
		}
	}
	out := make([]bool, len(batch))
	for i, ph := range batch {
		out[i] = ok[index[messageProbeSpan{ph.ref.trace, ph.ref.span, ph.ref.start}]]
	}
	return out, nil
}

func messageHit(ph messagePendingHit) MessageSpanHit {
	r, c := ph.ref, ph.cand
	return MessageSpanHit{
		Trace:         r.trace,
		Span:          r.span,
		Start:         r.start,
		AttributeKey:  r.attrKey,
		Role:          c.role,
		MessageHash:   hex.EncodeToString(c.hash[:]),
		TurnOrdinals:  append([]int(nil), r.ordinals...),
		PrefixChain:   hex.EncodeToString(r.prefix[:]),
		ManifestChain: hex.EncodeToString(r.chain[:]),
		MatchedTerms:  append([]string(nil), c.matched...),
		Positions:     append([]int(nil), c.positions...),
	}
}

// runMessageQuery emits verified hits in (sort suffix, hash) order after
// resume, stopping after limit hits (0: unbounded). w further restricts start.
func (b *FDBBackend) runMessageQuery(ctx context.Context, tx fdb.ReadTransaction, pm *postingMaintainer, plan *messageQueryPlan, resume *messageResume, w timeWindow, limit int, budget *queryBudget, emit func(MessageSpanHit, []byte)) error {
	const op = "message full-text query"
	root, tenant, order := b.root, b.cfg.Tenant, b.cfg.Order
	w = intersectTimeWindows(plan.window, w)
	if w.empty {
		return nil
	}
	cands, err := matchMessageBlobs(tx, root, tenant, plan, budget)
	if err != nil || len(cands) == 0 {
		return err
	}
	var streams []*messageRefStream
	for _, c := range cands {
		prefix := messageSpanRefSpace(root, tenant, c.hash)
		bk, ek := prefix.FDBRangeKeys()
		begin, end := bk.FDBKey(), ek.FDBKey()
		if w.set && order == "time" {
			begin, end = w.descendingRange(prefix)
		}
		if resume != nil {
			after := prefix.Pack(resume.suffix)
			if bytes.Compare(c.hash[:], resume.hash[:]) <= 0 {
				after = append(after, 0)
			}
			if bytes.Compare(after, begin) > 0 {
				begin = after
			}
		}
		if bytes.Compare(begin, end) < 0 {
			streams = append(streams, &messageRefStream{cand: c, prefix: prefix, plen: len(prefix.Bytes()), begin: begin, end: end})
		}
	}
	chunk := msgRefScanRows
	if limit > 0 {
		chunk = min(max(limit+1, 8), msgRefScanRows)
	}
	first := make([]fdb.RangeResult, len(streams))
	for i, s := range streams {
		first[i] = s.read(tx, chunk)
	}
	h := make(messageRefHeap, 0, len(streams))
	for i, s := range streams {
		if err := s.accept(first[i], chunk, budget); err != nil {
			return err
		}
		if len(s.rows) > 0 {
			h = append(h, s)
		}
	}
	heap.Init(&h)
	var batch []messagePendingHit
	accepted := 0
	flush := func() (bool, error) {
		if err := canceled(ctx, op); err != nil {
			return false, err
		}
		var need []*messageCandidate
		queued := map[*messageCandidate]bool{}
		for _, ph := range batch {
			if !ph.cand.verified && !queued[ph.cand] {
				queued[ph.cand] = true
				need = append(need, ph.cand)
			}
		}
		if err := verifyMessageCandidates(tx, root, tenant, plan, need, budget); err != nil {
			return false, err
		}
		var ok []bool
		if len(plan.preds) > 0 {
			var err error
			if ok, err = probeMessagePredicates(tx, pm, tenant, plan.preds, batch, budget); err != nil {
				return false, err
			}
		}
		defer func() { batch = batch[:0] }()
		for i, ph := range batch {
			if ok != nil && !ok[i] {
				continue
			}
			emit(messageHit(ph), append(append(tuple.Tuple(nil), ph.suffix...), ph.cand.hash[:]).Pack())
			accepted++
			if limit > 0 && accepted == limit {
				return true, nil
			}
		}
		return false, nil
	}
	for h.Len() > 0 {
		s := h[0]
		kv := s.rows[0]
		s.rows = s.rows[1:]
		if len(s.rows) == 0 && !s.done {
			if err := s.accept(s.read(tx, chunk), chunk, budget); err != nil {
				return err
			}
		}
		if len(s.rows) == 0 {
			heap.Pop(&h)
		} else {
			heap.Fix(&h, 0)
		}
		ref, suffix, err := decodeMessageSpanRef(s.prefix, s.cand.hash, order, kv)
		if err != nil {
			return err
		}
		if ref.encoding != s.cand.encoding {
			return fail("corruption", "message span reference", "reference encoding disagrees with the message postings")
		}
		if (plan.attrKey != "" && ref.attrKey != plan.attrKey) || (plan.firstTurn && ref.ordinals[len(ref.ordinals)-1] != ref.count-1) || !w.contains(ref.start) {
			continue
		}
		batch = append(batch, messagePendingHit{s.cand, ref, suffix})
		capacity := sameSpanProbeBatch
		if limit > 0 {
			capacity = min(capacity, limit-accepted)
		}
		if len(batch) >= capacity {
			full, err := flush()
			if err != nil || full {
				return err
			}
		}
	}
	if len(batch) > 0 {
		_, err := flush()
		return err
	}
	return nil
}

// QueryMessageFullText returns one verified snapshot page of message hits,
// resuming after a signed continuation at a new read version.
func (b *FDBBackend) QueryMessageFullText(ctx context.Context, q MessageFullTextQuery) (MessageFullTextPage, error) {
	const op = "message full-text query"
	b.lastWork = QueryResult{}
	page := MessageFullTextPage{Hits: []MessageSpanHit{}, Order: b.cfg.Order}
	plan, err := newMessageQueryPlan(op, q, b.cfg.K)
	if err != nil {
		return page, err
	}
	var token *continuation
	if len(q.Continuation) > 0 {
		if token, err = parseContinuationKind(q.Continuation, messageContinuationKind); err != nil {
			return page, err
		}
	}
	var secret, lastKey []byte
	budget := b.newBudget(op)
	_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		page = MessageFullTextPage{Hits: []MessageSpanHit{}, Order: b.cfg.Order}
		budget = b.newBudget(op)
		lastKey = nil
		pm, err := b.openPostings(rc, op)
		if err != nil {
			return nil, err
		}
		if secret, err = b.readContinuationSecret(rc); err != nil {
			return nil, err
		}
		var resume *messageResume
		if token != nil {
			if err := token.authenticate(secret, b, plan.digest); err != nil {
				return nil, err
			}
			if resume, err = parseMessageResume(b.cfg.Order, token.last); err != nil {
				return nil, err
			}
		}
		if page.ReadVersion, err = rc.GetReadVersion(); err != nil {
			return nil, err
		}
		return nil, b.runMessageQuery(ctx, rc.ReadTransaction(false), pm, plan, resume, timeWindow{}, plan.limit, budget, func(h MessageSpanHit, key []byte) {
			page.Hits = append(page.Hits, h)
			lastKey = key
		})
	})
	page.Candidates, page.Bytes = budget.candidates, budget.bytes
	b.lastWork = QueryResult{Candidates: budget.candidates, Bytes: budget.bytes}
	if err != nil || len(page.Hits) < plan.limit {
		return page, err
	}
	if len(secret) == 0 {
		if secret, err = b.ensureContinuationSecret(ctx); err != nil {
			return page, err
		}
	}
	page.Continuation = signContinuationKind(secret, messageContinuationKind, b.cfg.Run, b.cfg.Tenant, b.cfg.Order, plan.digest, lastKey)
	return page, nil
}

// QueryMessageFullText searches message text; the exporter must index it.
func (e *Exporter) QueryMessageFullText(ctx context.Context, q MessageFullTextQuery) (MessageFullTextPage, error) {
	const op = "message full-text query"
	empty := MessageFullTextPage{Hits: []MessageSpanHit{}, Order: e.cfg.Order}
	if !e.cfg.EnableMessageFullText {
		return empty, fail("config", op, "enable_message_full_text is off; message text is not indexed")
	}
	if err := e.begin(op); err != nil {
		return empty, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.QueryMessageFullText(ctx, q)
}
