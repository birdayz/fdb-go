package textcmp

import (
	"errors"
	"reflect"
	"testing"
)

// ---- hasLetterOrDigit -------------------------------------------------------

func TestHasLetterOrDigit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"lowercase letters", "abc", true},
		{"uppercase letters", "ABC", true},
		{"digits only", "123", true},
		{"mixed letter and digit", "a1", true},
		{"single letter", "z", true},
		{"single digit", "0", true},
		{"punctuation only", "...", false},
		{"dashes only", "---", false},
		{"spaces only", "   ", false},
		{"empty string", "", false},
		{"exclamation only", "!", false},
		{"letter after punctuation", "!a", true},
		{"supplementary letter only", "\U00010414", false},
		{"supplementary digit only", "\U000104a0", false},
		{"supplementary letter with BMP letter", "\U00010414a", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasLetterOrDigit(tc.input); got != tc.want {
				t.Errorf("hasLetterOrDigit(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---- stripMarks -------------------------------------------------------------

func TestStripMarks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// NFC "é" is one codepoint (U+00E9, precomposed) — no combining mark → unchanged.
		{"precomposed e-acute unchanged", "é", "é"},
		// NFD / explicit decomposed form: "e" + U+0301 (combining acute) → combining mark stripped.
		{"decomposed e-acute strips mark", "é", "e"},
		// Pure ASCII stays put.
		{"ascii unchanged", "hello", "hello"},
		{"empty", "", ""},
		// Digits and spaces are not category-M, so they pass through.
		{"digits and spaces", "a1 b2", "a1 b2"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripMarks(tc.input); got != tc.want {
				t.Errorf("stripMarks(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---- breakIteratorWrapper ---------------------------------------------------

func TestBreakIteratorWrapperLazyAdvancement(t *testing.T) {
	t.Parallel()
	const text = "FIRST, Café!!!"
	raw, err := DefaultTextTokenizerInstance().Tokenize(text, 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	iter, ok := raw.(*breakIteratorWrapper)
	if !ok {
		t.Fatalf("Tokenize returned %T, want lazy breakIteratorWrapper", raw)
	}
	if iter.remaining != text || iter.state != -1 || iter.hasNext || iter.next != "" {
		t.Fatalf("constructor eagerly advanced: %+v", iter)
	}
	if !iter.HasNext() || iter.next != "first" || iter.remaining != ", Café!!!" {
		t.Fatalf("first HasNext did not stop after one token: %+v", iter)
	}
	cached := *iter
	if !iter.HasNext() || *iter != cached {
		t.Fatalf("repeated HasNext advanced cached token: %+v", iter)
	}
	if got := iter.Next(); got != "first" {
		t.Fatalf("first token = %q, want first", got)
	}
	if iter.remaining != cached.remaining || iter.state != cached.state || iter.hasNext || iter.next != "" {
		t.Fatalf("Next did more than consume the cached token: %+v", iter)
	}
	// Next must also advance without a preceding HasNext, skipping punctuation
	// and normalizing only the requested word, not the trailing punctuation.
	if got := iter.Next(); got != "cafe" || iter.remaining != "!!!" {
		t.Fatalf("second token = %q, iterator=%+v", got, iter)
	}
	if iter.HasNext() || iter.remaining != "" || iter.hasNext || iter.next != "" {
		t.Fatalf("trailing punctuation was not filtered: %+v", iter)
	}
	exhausted := *iter
	if iter.HasNext() || *iter != exhausted {
		t.Fatalf("repeated exhaustion changed iterator: %+v", iter)
	}
}

func TestBreakIteratorWrapperTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace", " \t\n ", nil},
		{"punctuation and emoji", "!🌍,...", nil},
		{"punctuation separates words", "a,b", []string{"a", "b"}},
		{"words and digits", "Hello, world! 7", []string{"hello", "world", "7"}},
		{"midword punctuation", "don't U.S.A hello_world", []string{"don't", "u.s.a", "hello_world"}},
		{"normalization and final sigma", "CAFÉ ﬆ ΟΣ", []string{"cafe", "st", "ος"}},
		{"combining mark within word", "прив\u0301ет", []string{"привет"}},
		{"CJK word boundaries", "世界", []string{"世", "界"}},
		{"supplementary letters and digits filtered", "\U00010414 \U000104a0 abc", []string{"abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			iter := newBreakIteratorWrapper(tc.text)
			var got []string
			for iter.HasNext() {
				cached := *iter
				if !iter.HasNext() || *iter != cached {
					t.Fatal("HasNext changed a cached token")
				}
				got = append(got, iter.Next())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tokens for %q = %q, want %q", tc.text, got, tc.want)
			}
			if iter.remaining != "" || iter.HasNext() {
				t.Fatalf("iterator did not exhaust: %+v", iter)
			}
		})
	}
}

func TestTokenizerErrorsAreMetaDataError(t *testing.T) {
	t.Parallel()
	if _, err := GetTextTokenizer("no-such-tokenizer-xyz"); err == nil {
		t.Fatal("expected error for unknown tokenizer")
	} else {
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) {
			t.Fatalf("expected *MetaDataError, got %T: %v", err, err)
		}
	}
	if err := ValidateTokenizerVersion(DefaultTextTokenizerInstance(), 1); err == nil {
		t.Fatal("expected error for bad version")
	} else {
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) {
			t.Fatalf("expected *MetaDataError, got %T: %v", err, err)
		}
	}
}
