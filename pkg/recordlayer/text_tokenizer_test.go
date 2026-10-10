package recordlayer

// Unit tests for text_tokenizer.go that are NOT already covered by
// text_index_unit_test.go.  That file already has thorough
// DefaultTextTokenizer.Tokenize/TokenizeToList/TokenizeToMap and registry
// tests; this file adds:
//   - hasLetterOrDigit (direct)
//   - stripMarks (direct)
//   - wordSegments (direct)
//   - DefaultTextTokenizer metadata (Name/MinVersion/MaxVersion)
//   - DefaultTextTokenizerFactory
//   - DefaultTextTokenizerInstance singleton identity
//   - ValidateTokenizerVersion returns *MetaDataError
//   - TokenizeToList / TokenizeToMap also reject bad versions (not just Tokenize)
//   - TokenIterator.HasNext == false on empty input
//   - GlobalTextTokenizerRegistry singleton identity
//   - GetTextTokenizer errors are *MetaDataError
//   - Registry concurrency (Register + GetTokenizer in parallel)

import (
	"errors"
	"reflect"
	"sync"
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

// ---- wordSegments -----------------------------------------------------------

func TestWordSegments(t *testing.T) {
	t.Parallel()

	t.Run("empty string returns nil", func(t *testing.T) {
		t.Parallel()
		if got := wordSegments(""); got != nil {
			t.Errorf("wordSegments(\"\") = %v, want nil", got)
		}
	})

	t.Run("segments concatenate to original", func(t *testing.T) {
		t.Parallel()
		text := "hello world"
		segs := wordSegments(text)
		if len(segs) == 0 {
			t.Fatal("expected non-empty segments for 'hello world'")
		}
		joined := ""
		for _, s := range segs {
			joined += s
		}
		if joined != text {
			t.Errorf("concatenated segments %q != original %q", joined, text)
		}
	})

	t.Run("word segments contain the words", func(t *testing.T) {
		t.Parallel()
		segs := wordSegments("hello world")
		found := make(map[string]bool)
		for _, s := range segs {
			found[s] = true
		}
		for _, w := range []string{"hello", "world"} {
			if !found[w] {
				t.Errorf("expected segment %q in %v", w, segs)
			}
		}
	})

	t.Run("punctuation gets its own segment", func(t *testing.T) {
		t.Parallel()
		segs := wordSegments("a,b")
		if len(segs) < 3 {
			t.Errorf("expected >=3 segments for 'a,b', got %d: %v", len(segs), segs)
		}
	})
}

// ---- DefaultTextTokenizer metadata ------------------------------------------

func TestDefaultTextTokenizerMetadata(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()

	t.Run("Name", func(t *testing.T) {
		t.Parallel()
		if got := tok.Name(); got != DefaultTextTokenizerName {
			t.Errorf("Name() = %q, want %q", got, DefaultTextTokenizerName)
		}
	})

	t.Run("MinVersion equals GlobalMinVersion", func(t *testing.T) {
		t.Parallel()
		if got := tok.MinVersion(); got != GlobalMinVersion {
			t.Errorf("MinVersion() = %d, want %d", got, GlobalMinVersion)
		}
	})

	t.Run("MaxVersion equals MinVersion", func(t *testing.T) {
		t.Parallel()
		if got := tok.MaxVersion(); got != tok.MinVersion() {
			t.Errorf("MaxVersion() = %d, want MinVersion() = %d", got, tok.MinVersion())
		}
	})
}

// ---- DefaultTextTokenizerInstance singleton ---------------------------------

func TestDefaultTextTokenizerInstanceSingleton(t *testing.T) {
	t.Parallel()
	a := DefaultTextTokenizerInstance()
	b := DefaultTextTokenizerInstance()
	if a != b {
		t.Error("DefaultTextTokenizerInstance() returned different pointers")
	}
}

// ---- DefaultTextTokenizerFactory --------------------------------------------

func TestDefaultTextTokenizerFactory(t *testing.T) {
	t.Parallel()
	f := &DefaultTextTokenizerFactory{}

	t.Run("Name returns default", func(t *testing.T) {
		t.Parallel()
		if got := f.Name(); got != DefaultTextTokenizerName {
			t.Errorf("Name() = %q, want %q", got, DefaultTextTokenizerName)
		}
	})

	t.Run("GetTokenizer returns non-nil DefaultTextTokenizer", func(t *testing.T) {
		t.Parallel()
		tok := f.GetTokenizer()
		if tok == nil {
			t.Fatal("GetTokenizer() returned nil")
		}
		if _, ok := tok.(*DefaultTextTokenizer); !ok {
			t.Errorf("GetTokenizer() = %T, want *DefaultTextTokenizer", tok)
		}
	})
}

// ---- ValidateTokenizerVersion returns *MetaDataError -----------------------

func TestValidateTokenizerVersionErrorType(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()

	t.Run("version above max is MetaDataError", func(t *testing.T) {
		t.Parallel()
		err := ValidateTokenizerVersion(tok, 1)
		if err == nil {
			t.Fatal("expected error for version 1, got nil")
		}
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) {
			t.Fatalf("expected *MetaDataError, got %T: %v", err, err)
		}
		if mdErr.Message == "" {
			t.Error("MetaDataError.Message is empty")
		}
	})

	t.Run("version below min is MetaDataError", func(t *testing.T) {
		t.Parallel()
		err := ValidateTokenizerVersion(tok, -1)
		if err == nil {
			t.Fatal("expected error for version -1, got nil")
		}
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) {
			t.Fatalf("expected *MetaDataError, got %T: %v", err, err)
		}
	})
}

// ---- TokenizeToList / TokenizeToMap also reject bad versions ----------------

func TestDefaultTextTokenizerBadVersionAllMethods(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()

	t.Run("TokenizeToList bad version", func(t *testing.T) {
		t.Parallel()
		_, err := tok.TokenizeToList("hello", 1, TokenizerModeIndex)
		if err == nil {
			t.Fatal("expected error")
		}
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) {
			t.Fatalf("expected *MetaDataError, got %T", err)
		}
	})

	t.Run("TokenizeToMap bad version", func(t *testing.T) {
		t.Parallel()
		_, err := tok.TokenizeToMap("hello", 1, TokenizerModeIndex)
		if err == nil {
			t.Fatal("expected error")
		}
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) {
			t.Fatalf("expected *MetaDataError, got %T", err)
		}
	})
}

// ---- TokenIterator.HasNext on empty input -----------------------------------

func TestTokenIteratorHasNextEmptyInput(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	iter, err := tok.Tokenize("", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if iter.HasNext() {
		t.Error("HasNext() should be false for empty input")
	}
}

// ---- TokenizeToMap position ordering ----------------------------------------

func TestTokenizeToMapPositionOrder(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()

	// Repeated token: positions must be in ascending order of first appearance.
	m, err := tok.TokenizeToMap("a b a c a", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	aPos, ok := m["a"]
	if !ok {
		t.Fatal("expected 'a' in map")
	}
	want := []int{0, 2, 4}
	if !reflect.DeepEqual(aPos, want) {
		t.Errorf("'a' positions = %v, want %v", aPos, want)
	}
}

// ---- GlobalTextTokenizerRegistry singleton ----------------------------------

func TestGlobalTextTokenizerRegistrySingleton(t *testing.T) {
	t.Parallel()

	t.Run("returns non-nil", func(t *testing.T) {
		t.Parallel()
		if r := GlobalTextTokenizerRegistry(); r == nil {
			t.Error("GlobalTextTokenizerRegistry() returned nil")
		}
	})

	t.Run("same instance on repeated calls", func(t *testing.T) {
		t.Parallel()
		a := GlobalTextTokenizerRegistry()
		b := GlobalTextTokenizerRegistry()
		if a != b {
			t.Error("GlobalTextTokenizerRegistry() returned different instances")
		}
	})
}

// ---- GetTextTokenizer errors are *MetaDataError -----------------------------

func TestGetTextTokenizerErrorType(t *testing.T) {
	t.Parallel()
	_, err := GetTextTokenizer("no-such-tokenizer-xyz")
	if err == nil {
		t.Fatal("expected error for unknown tokenizer, got nil")
	}
	var mdErr *MetaDataError
	if !errors.As(err, &mdErr) {
		t.Fatalf("expected *MetaDataError, got %T: %v", err, err)
	}
	if mdErr.Message == "" {
		t.Error("MetaDataError.Message is empty")
	}
}

// ---- Registry concurrency ---------------------------------------------------

func TestTextTokenizerRegistryConcurrency(t *testing.T) {
	t.Parallel()
	// Use a fresh isolated registry so this test cannot interfere with global state.
	reg := newTextTokenizerRegistry()

	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)

	// Shared factory: every goroutine registers the same pointer — all must
	// succeed; concurrent reads must also succeed without a data race.
	shared := &stubTokenizerFactory{name: "concurrent-tok"}

	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_ = reg.Register(shared) // idempotent same-pointer registration
			} else {
				_, _ = reg.GetTokenizer("default")
			}
		}()
	}
	wg.Wait()
	// If we reach here without a data race or panic, locking is correct.
}

// Expected tokens are Java 4.14.2.0 DefaultTextTokenizer's on JDK 21.0.9. Inputs
// whose word segmentation differs are pinned in the conformance suite instead.
func TestDefaultTextTokenizerMatchesJava(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want []string
	}{
		{"ΟΔΟΣ ΣΟΦΟΣ Σ ΣΑ ΑΣ. ΑΣ'Σ", []string{"οδος", "σοφος", "σ", "σα", "ας", "ασ'ς"}},
		{"Ὀδυσσεύς", []string{"οδυσσευς"}},
		{"𐐔𐐯𐑅𐐨𐑉𐐯𐐻 𐐔", nil},
		{"𠀀𠀁 𠀂", nil},
		{"𞤀𞤁𞤂", nil},
		{"한국어 문장입니다", []string{"한국어", "문장입니다"}},
		{"مرحبا بالعالم", []string{"مرحبا", "بالعالم"}},
		{"שלום עולם", []string{"שלום", "עולם"}},
		{"नमस्ते दुनिया", []string{"नमसत", "दनय"}},
		{"হ্যালো", []string{"হযল"}},
		{"தமிழ்", []string{"தமழ"}},
		{"Привет мир ЁЖ", []string{"привет", "мир", "еж"}},
		{"İstanbul ıi I", []string{"istanbul", "ıi", "i"}},
		{"Straße STRASSE ß", []string{"straße", "strasse", "ß"}},
		{"ﬁnance ﬂ ǅ ǆ", []string{"finance", "fl", "dz", "dz"}},
		{"ＡＢＣ１２３ ｶﾀｶﾅ", []string{"abc123", "カタカナ"}},
		{"e.g. U.S.A. 3.14 1,000 1.2.3 a.b", []string{"e.g", "u.s.a", "3.14", "1,000", "1.2.3", "a.b"}},
		{"don't O'Neil l'homme rock'n'roll", []string{"don't", "o'neil", "l'homme", "rock'n'roll"}},
		{"user@example.com http://x.y/z?a=b", []string{"user", "example.com", "http", "x.y", "z", "a", "b"}},
		{"😀 👍🏽 👨\u200d👩\u200d👧 🇩🇪 a😀b", []string{"a", "b"}},
		{"𝐀𝐁𝐂 𝟙𝟚", []string{"abc", "12"}},
		{"Ⅻ ⅻ ℃", []string{"xii", "xii", "°c"}},
		{"café naïve coöperate", []string{"cafe", "naive", "cooperate"}},
		{"עִבְרִית", []string{"עברית"}},
		{"ǰ ŉ ΐ ﬀ", []string{"j", "ʼn", "ι", "ff"}},
		{"ꙮ ᏣᎳᎩ ⴀ Ⴀ", []string{"ꙮ", "ꮳꮃꭹ", "ⴀ", "ⴀ"}},
		{"Α1Σ ΑΣ1 1Σ Σ1 ΑΣΑ ΑΣ\u0301 Α\u0301Σ", []string{"α1ς", "ας1", "1σ", "σ1", "ασα", "ας", "ας"}},
		{"ΑΣ\u00adΣ Α\u00adΣ \u0345Σ", []string{"ασ\u00adς", "α\u00adς", "σ"}},
		{"Σ\u0301 ΣΣ ΣΣΣ ὈΣ ἈΣ", []string{"σ", "σς", "σσς", "ος", "ας"}},
		{"AΣ aΣ ǅΣ ꭜΣ ΑΣ\u200dΣ", []string{"aς", "aς", "dzς", "ꜧς", "ασ\u200dς"}},
		{"a𐐔 𐐔a 𐒠1 1𐒠 𐒠 𝟙", []string{"a𐐼", "𐐼a", "𐒠1", "1𐒠", "1"}},
		{"ΘΕΟΣ, ΚΑΙ ΣΥ; ΛΟΓΟΣ!", []string{"θεος", "και", "συ", "λογος"}},
	}
	for _, tc := range cases {
		got, err := DefaultTextTokenizerInstance().TokenizeToList(tc.text, 0, TokenizerModeIndex)
		if err != nil {
			t.Fatalf("TokenizeToList(%q): %v", tc.text, err)
		}
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("TokenizeToList(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
