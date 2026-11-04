package otel

// Message text extraction, the message tokenizer and text segmentation.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"github.com/rivo/uniseg"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Text extraction and tokenization
// ---------------------------------------------------------------------------

func normalizeMessageRole(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// messageStructuralKey names map fields whose values are metadata, not text.
func messageStructuralKey(k string) bool { return k == "role" || k == "type" }

// extractMessageText returns a message's normalized top-level role and its
// searchable text: every string value except those under "role" and "type"
// keys, in document order, joined by newlines. Both element encodings of the
// same message yield the same result.
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

// messageTokenizer is the Record Layer default tokenizer except that a token
// longer than maxIndexedTokenBytes becomes "", which the TEXT index skips
// while keeping its position, as the default tokenizers treat stop words.
type messageTokenizer struct{}

func (messageTokenizer) Name() string                            { return messageTokenizerName }
func (messageTokenizer) GetTokenizer() recordlayer.TextTokenizer { return messageTokenizer{} }
func (messageTokenizer) MinVersion() int {
	return recordlayer.DefaultTextTokenizerInstance().MinVersion()
}

func (messageTokenizer) MaxVersion() int {
	return recordlayer.DefaultTextTokenizerInstance().MaxVersion()
}

func (messageTokenizer) Tokenize(text string, version int, mode recordlayer.TokenizerMode) (recordlayer.TokenIterator, error) {
	it, err := recordlayer.DefaultTextTokenizerInstance().Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	return cappedTokens{it}, nil
}

// TokenizeToMap and TokenizeToList follow TextTokenizer's default methods
// (text_tokenizer.go defaultTokenizeToMap/defaultTokenizeToList).
func (t messageTokenizer) TokenizeToMap(text string, version int, mode recordlayer.TokenizerMode) (map[string][]int, error) {
	it, err := t.Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for offset := 0; it.HasNext(); offset++ {
		if tok := it.Next(); tok != "" {
			out[tok] = append(out[tok], offset)
		}
	}
	return out, nil
}

func (t messageTokenizer) TokenizeToList(text string, version int, mode recordlayer.TokenizerMode) ([]string, error) {
	it, err := t.Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	var out []string
	for it.HasNext() {
		out = append(out, it.Next())
	}
	return out, nil
}

type cappedTokens struct{ recordlayer.TokenIterator }

func (c cappedTokens) Next() string {
	if tok := c.TokenIterator.Next(); len(tok) <= maxIndexedTokenBytes {
		return tok
	}
	return ""
}

var messageTokenizerRegistrationError = recordlayer.GlobalTextTokenizerRegistry().Register(messageTokenizer{})

// messageSegment is one indexed slice of a message's text: whole tokens
// offset..offset+n-1 of the message. Postings is its number of distinct
// indexed tokens, the TEXT index entries it adds.
type messageSegment struct {
	text     string
	offset   int
	postings int
}

// segmentMessageText cuts text into segments of at most the given number of
// distinct tokens and messageSegmentBytes bytes, consecutive segments sharing
// messageSegmentOverlap tokens. A segment begins at a token's first byte and
// ends at a token's last byte, and must tokenize to exactly its tokens of the
// whole text; text that cannot be cut so is refused rather than mis-indexed.
// It also returns the number of tokens too long to index.
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
