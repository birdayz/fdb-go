package otelsql

import (
	"fmt"
	"strings"

	"fdb.dev/internal/unicode15"
	"fdb.dev/internal/unicode15/text/unicode/norm"
	"fdb.dev/pkg/recordlayer"
	"github.com/rivo/uniseg"
)

// OverlongTokenPlaceholder stands, in MESSAGES.content, for every token longer
// than MaxIndexedTokenBytes, so MESSAGES_TEXT (the default tokenizer, which
// Java knows) keeps the token's position without indexing it. The placeholder
// itself is an indexed token: a query for it matches every overlong token.
const OverlongTokenPlaceholder = "oteloverlongtoken"

// UnindexableTextError is text whose overlong tokens IndexableText cannot
// replace without moving another token.
type UnindexableTextError struct {
	Reason string
}

func (e *UnindexableTextError) Error() string {
	return "otelsql: message text: " + e.Reason
}

// IndexableText is text as MESSAGES.content stores it, derived text for the
// index only (the element's raw bytes are the message): each token longer
// than MaxIndexedTokenBytes, at the default tokenizer's word boundaries, is
// replaced by OverlongTokenPlaceholder between spaces. The default tokenizer
// then emits the same tokens at the same positions, the placeholder where
// otel_message_v1 emitted "", and text without an overlong token is returned
// as is.
func IndexableText(text string) (string, error) {
	tok := recordlayer.DefaultTextTokenizerInstance()
	return indexableText(text, func(s string) ([]string, error) {
		return tok.TokenizeToList(s, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
	})
}

// indexableText is IndexableText over tokenize, the default tokenizer's
// index tokens: a test's tokenize reaches the fail-closed checks.
func indexableText(text string, tokenize func(string) ([]string, error)) (string, error) {
	tokens, err := tokenize(text)
	if err != nil {
		return "", err
	}
	want := make([]string, len(tokens))
	overlong := false
	for i, t := range tokens {
		want[i] = t
		if len(t) > MaxIndexedTokenBytes {
			want[i], overlong = OverlongTokenPlaceholder, true
		}
	}
	if !overlong {
		return text, nil
	}
	// The tokenizer's own walk (text_tokenizer.go breakIteratorWrapper): UAX
	// #29 word segments, and a token for each one with a BMP letter or digit
	// once NFKD-normalized.
	var b strings.Builder
	b.Grow(len(text))
	next, state := 0, -1
	for rest := text; len(rest) > 0; {
		var segment string
		segment, rest, state = uniseg.FirstWordInString(rest, state)
		if segment == "" || !hasLetterOrDigit(norm.NFKD.String(segment)) {
			b.WriteString(segment)
			continue
		}
		if next == len(tokens) {
			return "", &UnindexableTextError{Reason: "more word segments than tokens"}
		}
		if len(tokens[next]) > MaxIndexedTokenBytes {
			b.WriteString(" " + OverlongTokenPlaceholder + " ")
		} else {
			b.WriteString(segment)
		}
		next++
	}
	if next != len(tokens) {
		return "", &UnindexableTextError{Reason: "fewer word segments than tokens"}
	}
	out := b.String()
	got, err := tokenize(out)
	if err != nil {
		return "", err
	}
	if len(got) != len(want) {
		return "", &UnindexableTextError{Reason: fmt.Sprintf("%d tokens after replacement, want %d", len(got), len(want))}
	}
	for i := range got {
		if got[i] != want[i] {
			return "", &UnindexableTextError{Reason: fmt.Sprintf("token %d changed by the replacement", i)}
		}
	}
	return out, nil
}

// hasLetterOrDigit is the default tokenizer's token test (text_tokenizer.go).
func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if r <= 0xFFFF && (unicode15.IsLetter(r) || unicode15.IsDigit(r)) {
			return true
		}
	}
	return false
}
