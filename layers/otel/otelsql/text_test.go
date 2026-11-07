package otelsql

import (
	"math/rand"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// sameTokensAsMessageTokenizer requires the default tokenizer over
// IndexableText(text) to give otel_message_v1's tokens of text, position for
// position, with OverlongTokenPlaceholder where otel_message_v1 has "".
func sameTokensAsMessageTokenizer(t *testing.T, text string) (string, int) {
	t.Helper()
	out, err := IndexableText(text)
	if err != nil {
		t.Fatalf("%.80q: %v", text, err)
	}
	old, err := MessageTokenizer{}.TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	got, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(out, 0, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(old) {
		t.Fatalf("%.80q: %d tokens, otel_message_v1 has %d", text, len(got), len(old))
	}
	replaced := 0
	for i := range got {
		want := old[i]
		if want == "" {
			want = OverlongTokenPlaceholder
			replaced++
		}
		if got[i] != want {
			t.Fatalf("%.80q: token %d = %.40q, want %.40q", text, i, got[i], want)
		}
	}
	return out, replaced
}

func TestIndexableText(t *testing.T) {
	t.Parallel()
	for _, s := range []string{OverlongTokenPlaceholder, strings.ToUpper(OverlongTokenPlaceholder), " " + OverlongTokenPlaceholder + " "} {
		toks, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(s, 0, recordlayer.TokenizerModeIndex)
		if err != nil || len(toks) != 1 || toks[0] != OverlongTokenPlaceholder {
			t.Fatalf("placeholder %q tokenizes to %q (%v)", s, toks, err)
		}
	}
	// Text without an overlong token is stored as is.
	plain := "Crème BRÛLÉE, crème! " + strings.Repeat("b", MaxIndexedTokenBytes) + " " + strings.Repeat("e\u0301", MaxIndexedTokenBytes)
	if out, n := sameTokensAsMessageTokenizer(t, plain); out != plain || n != 0 {
		t.Fatalf("text without overlong tokens rewritten (%d replaced)", n)
	}
	long := strings.Repeat("x", MaxIndexedTokenBytes+1)
	for _, tc := range []struct {
		name, text string
		replaced   int
	}{
		{"ascii", "alpha " + long + " beta gamma", 1},
		{"edges", long + " " + long, 2},
		{"glued punctuation", "(" + long + ")," + long + "." + "x", 2},
		{"katakana after letters", "abc" + strings.Repeat("カ", 400) + "def", 1},
		{"digits joined to letters", "123" + long + " 456", 1},
		{"underscore joins", "a_" + long + "_b c", 1},
		{"apostrophe joins", "x'" + long + " y", 1},
		// NFKD expands U+FDFA eighteen-fold: a short raw word, an overlong token.
		{"NFKD expansion", "w " + strings.Repeat("\uFDFA", 60) + " z", 1},
		{"long raw, short token", strings.Repeat("e\u0301", 700) + " q", 0},
		{"newlines", "a\n" + long + "\r\nb", 1},
		{"emoji neighbours", "👍" + long + "👍 ok", 1},
	} {
		out, n := sameTokensAsMessageTokenizer(t, tc.text)
		if n != tc.replaced {
			t.Errorf("%s: %d replaced, want %d", tc.name, n, tc.replaced)
		}
		if n > 0 && !strings.Contains(out, OverlongTokenPlaceholder) {
			t.Errorf("%s: no placeholder in %.80q", tc.name, out)
		}
	}
	// Positions are kept: "alpha" and "beta" stay two apart, so the phrase
	// "alpha beta" does not match across the overlong token.
	out, err := IndexableText("alpha " + long + " beta gamma")
	if err != nil {
		t.Fatal(err)
	}
	pos, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToMap(out, 0, recordlayer.TokenizerModeIndex)
	if err != nil || pos["alpha"][0] != 0 || pos["beta"][0] != 2 || pos[OverlongTokenPlaceholder][0] != 1 {
		t.Fatalf("positions %v (%v)", pos, err)
	}

	// Random texts over adversarial pieces keep the property.
	pieces := []string{
		"a", "Word", "123", "_", "'", ".", ",", " ", "  ", "\n", "-", "カタカナ", "漢字", "é", "e\u0301", "👍", "\u200d", "\uFDFA", "ς", "Σ",
		long, strings.Repeat("カ", 350), strings.Repeat("9", MaxIndexedTokenBytes+3), strings.Repeat("\uFDFA", 40), strings.Repeat("ab", 520),
	}
	r := rand.New(rand.NewSource(1))
	for range 400 {
		var b strings.Builder
		for range 1 + r.Intn(12) {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		sameTokensAsMessageTokenizer(t, b.String())
	}

	// BuildMessageCompiled stores the indexable text.
	m, err := BuildMessageCompiled(MessagePart{Writer: 1, Seq: 1, Text: &MessageText{Role: "user", Content: "alpha " + long + " beta"}})
	if err != nil || m.GetCONTENT() != "alpha  "+OverlongTokenPlaceholder+"  beta" {
		t.Fatalf("content %.80q (%v)", m.GetCONTENT(), err)
	}
}
