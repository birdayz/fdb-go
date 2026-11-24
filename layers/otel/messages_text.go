package otel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/recordlayer"
	"github.com/rivo/uniseg"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

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

type messageTokenSpan struct {
	start, end int
}

// boundOverlongWords replaces overlong words with their normalized token (or a
// bounded sentinel) while raw message bytes remain stored separately.
func boundOverlongWords(text string, tok otelsql.MessageTokenizer) (string, error) {
	const op = "message text segments"
	var bounded strings.Builder
	state := -1
	for rest := text; len(rest) > 0; {
		var word string
		word, rest, state = uniseg.FirstWordInString(rest, state)
		ts, err := tok.TokenizeToList(word, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
		if err != nil {
			return "", typedError("config", op, "tokenizer", err)
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
	return bounded.String(), nil
}

// tokenWordSpans collects byte spans of word segments yielding one token,
// matching the default tokenizer's break iterator.
func tokenWordSpans(text string, tok otelsql.MessageTokenizer, want int) ([]messageTokenSpan, error) {
	const op = "message text segments"
	spans := make([]messageTokenSpan, 0, want)
	state, pos := -1, 0
	for rest := text; len(rest) > 0; {
		var word string
		word, rest, state = uniseg.FirstWordInString(rest, state)
		ts, err := tok.TokenizeToList(word, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
		if err != nil {
			return nil, typedError("config", op, "tokenizer", err)
		}
		if len(ts) > 1 {
			return nil, fail("admission", op, "a word segment yields several tokens")
		}
		if len(ts) == 1 {
			spans = append(spans, messageTokenSpan{pos, pos + len(word)})
		}
		pos += len(word)
	}
	if len(spans) != want {
		return nil, fail("admission", op, "word segments disagree with the tokenizer")
	}
	return spans, nil
}

// segmentMessageText bounds searchable projections by distinct tokens and bytes, preserving token positions.
// Segments share messageSegmentOverlap tokens; raw bytes stay separate, and overlong-token count is returned.
func segmentMessageText(text string, distinct int) ([]messageSegment, int, error) {
	const op = "message text segments"
	tok := otelsql.MessageTokenizer{}
	if len(text) > messageSegmentBytes {
		var err error
		if text, err = boundOverlongWords(text, tok); err != nil {
			return nil, 0, err
		}
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
	spans, err := tokenWordSpans(text, tok, len(whole))
	if err != nil {
		return nil, 0, err
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
// Message rows
// ---------------------------------------------------------------------------

// messagePlan is one element's MESSAGES rows, part 0 (the header) first.
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
