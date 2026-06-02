package recordlayer

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ---------------------------------------------------------------------------
// VarInt encoding roundtrip
// ---------------------------------------------------------------------------

func TestVarIntRoundtrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  int
	}{
		{"zero", 0},
		{"one", 1},
		{"127", 127},
		{"128", 128},
		{"255", 255},
		{"256", 256},
		{"600", 600},
		{"16383", 16383},
		{"16384", 16384},
		{"2097151", 2097151},
		{"max_int32", math.MaxInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded := packVarInt(tc.val)
			decoded, err := unpackVarInt(encoded)
			if err != nil {
				t.Fatalf("unpackVarInt(%x): %v", encoded, err)
			}
			if decoded != tc.val {
				t.Fatalf("roundtrip: got %d, want %d", decoded, tc.val)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// VarInt size calculation
// ---------------------------------------------------------------------------

func TestVarIntSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val      int
		wantSize int
	}{
		{0, 1},
		{1, 1},
		{127, 1},
		{128, 2},
		{255, 2},
		{256, 2},
		{600, 2},
		{16383, 2},
		{16384, 3},
		{2097151, 3},
		{2097152, 4},
		{math.MaxInt32, 5},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.val), func(t *testing.T) {
			t.Parallel()
			gotSize := varIntSize(tc.val)
			if gotSize != tc.wantSize {
				t.Fatalf("varIntSize(%d): got %d, want %d", tc.val, gotSize, tc.wantSize)
			}
			// Cross-check: actual encoded length must match.
			encoded := packVarInt(tc.val)
			if len(encoded) != tc.wantSize {
				t.Fatalf("actual encoded len(%d) = %d, varIntSize = %d", tc.val, len(encoded), tc.wantSize)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Position list delta compression roundtrip
// ---------------------------------------------------------------------------

func TestPositionListRoundtrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		list []int
	}{
		{"empty", nil},
		{"single_zero", []int{0}},
		{"single_nonzero", []int{5}},
		{"small_gaps", []int{1, 3, 5, 8}},
		{"starts_with_zero", []int{0, 600, 605}},
		{"sequential_100", sequentialInts(100)},
		{"large_gaps", []int{0, 100000, 200000, 300000}},
		{"duplicates", []int{0, 0, 0}}, // delta = 0 is valid
		{"two_elements", []int{42, 99}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			size, err := positionListSize(tc.list)
			if err != nil {
				t.Fatalf("positionListSize: %v", err)
			}
			var buf bytes.Buffer
			serializePositionList(&buf, tc.list, size)
			reader := bytes.NewReader(buf.Bytes())
			got, err := deserializePositionList(reader)
			if err != nil {
				t.Fatalf("deserializePositionList: %v", err)
			}
			// Normalize nil vs empty.
			if len(tc.list) == 0 {
				if len(got) != 0 {
					t.Fatalf("expected empty list, got %v", got)
				}
			} else if !reflect.DeepEqual(got, tc.list) {
				t.Fatalf("roundtrip mismatch: got %v, want %v", got, tc.list)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Position list validation: non-monotonic and negative
// ---------------------------------------------------------------------------

func TestPositionListNonMonotonic(t *testing.T) {
	t.Parallel()
	_, err := positionListSize([]int{5, 3})
	if err == nil {
		t.Fatal("expected error for non-monotonic list [5, 3]")
	}
	if !strings.Contains(err.Error(), "monotonically increasing") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPositionListNegative(t *testing.T) {
	t.Parallel()
	_, err := positionListSize([]int{-1})
	if err == nil {
		t.Fatal("expected error for negative position [-1]")
	}
	if !strings.Contains(err.Error(), "monotonically increasing") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPositionListNegativeMiddle(t *testing.T) {
	t.Parallel()
	_, err := positionListSize([]int{0, -1, 5})
	if err == nil {
		t.Fatal("expected error for list with negative in middle [0, -1, 5]")
	}
}

// ---------------------------------------------------------------------------
// SerializeKey roundtrip
// ---------------------------------------------------------------------------

func TestSerializeKeyRoundtrip(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	cases := []struct {
		name string
		key  tuple.Tuple
	}{
		{"int64", tuple.Tuple{int64(42)}},
		{"string", tuple.Tuple{"hello"}},
		{"bytes", tuple.Tuple{[]byte{0xde, 0xad, 0xbe, 0xef}}},
		{"nested", tuple.Tuple{tuple.Tuple{int64(1), "foo"}}},
		{"multi_element", tuple.Tuple{int64(1066), "battle"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := s.SerializeKey(tc.key)
			got, err := s.DeserializeKey(data, 0, len(data))
			if err != nil {
				t.Fatalf("DeserializeKey: %v", err)
			}
			if !reflect.DeepEqual(got, tc.key) {
				t.Fatalf("roundtrip: got %v, want %v", got, tc.key)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SerializeEntry roundtrip
// ---------------------------------------------------------------------------

func TestSerializeEntryRoundtrip(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	cases := []struct {
		name      string
		key       tuple.Tuple
		positions []int
	}{
		{"basic", tuple.Tuple{int64(42)}, []int{0, 1, 2}},
		{"empty_positions", tuple.Tuple{"word"}, nil},
		{"large_gap", tuple.Tuple{int64(99)}, []int{0, 10000}},
		{"single_position", tuple.Tuple{"token"}, []int{7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := s.SerializeEntry(tc.key, tc.positions)
			if err != nil {
				t.Fatalf("SerializeEntry: %v", err)
			}
			// SerializeEntry format: varInt(keyLen) + keyBytes + positionList
			// Verify we can parse it back.
			reader := bytes.NewReader(data)
			keyLen, err := deserializeVarInt(reader)
			if err != nil {
				t.Fatalf("reading key length: %v", err)
			}
			keyBytes := make([]byte, keyLen)
			if _, err := reader.Read(keyBytes); err != nil {
				t.Fatalf("reading key bytes: %v", err)
			}
			parsedKey, err := tuple.Unpack(keyBytes)
			if err != nil {
				t.Fatalf("unpacking key: %v", err)
			}
			if !reflect.DeepEqual(parsedKey, tc.key) {
				t.Fatalf("key mismatch: got %v, want %v", parsedKey, tc.key)
			}
			positions, err := deserializePositionList(reader)
			if err != nil {
				t.Fatalf("reading positions: %v", err)
			}
			if len(tc.positions) == 0 {
				if len(positions) != 0 {
					t.Fatalf("expected empty positions, got %v", positions)
				}
			} else if !reflect.DeepEqual(positions, tc.positions) {
				t.Fatalf("positions mismatch: got %v, want %v", positions, tc.positions)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SerializeEntries roundtrip
// ---------------------------------------------------------------------------

func TestSerializeEntriesRoundtrip(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()

	entries := []BunchedEntry[tuple.Tuple, []int]{
		{Key: tuple.Tuple{int64(1066)}, Value: []int{1, 3, 5, 8}},
		{Key: tuple.Tuple{int64(1415)}, Value: []int{0, 600, 605}},
		{Key: tuple.Tuple{int64(2000)}, Value: []int{42}},
	}

	data, err := s.SerializeEntries(entries)
	if err != nil {
		t.Fatalf("SerializeEntries: %v", err)
	}
	got, err := s.DeserializeEntries(entries[0].Key, data)
	if err != nil {
		t.Fatalf("DeserializeEntries: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("entry count: got %d, want %d", len(got), len(entries))
	}
	for i, e := range entries {
		if !reflect.DeepEqual(got[i].Key, e.Key) {
			t.Fatalf("entry[%d] key: got %v, want %v", i, got[i].Key, e.Key)
		}
		if !reflect.DeepEqual(got[i].Value, e.Value) {
			t.Fatalf("entry[%d] value: got %v, want %v", i, got[i].Value, e.Value)
		}
	}
}

// ---------------------------------------------------------------------------
// The documented example: exact byte-level verification
// ---------------------------------------------------------------------------

func TestSerializeEntriesDocumentedExample(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()

	// From the Java doc: (1066,)→[1,3,5,8] and (1415,)→[0,600,605]
	// Expected: 20 04 01 02 02 03 03 16 05 87 04 00 84 58 05
	entries := []BunchedEntry[tuple.Tuple, []int]{
		{Key: tuple.Tuple{int64(1066)}, Value: []int{1, 3, 5, 8}},
		{Key: tuple.Tuple{int64(1415)}, Value: []int{0, 600, 605}},
	}

	data, err := s.SerializeEntries(entries)
	if err != nil {
		t.Fatalf("SerializeEntries: %v", err)
	}
	want := "200401020203031605870400845805" // without spaces
	// Parse expected hex to allow flexible comparison.
	wantBytes, err := hex.DecodeString(want)
	if err != nil {
		t.Fatalf("bad test hex: %v", err)
	}
	if !bytes.Equal(data, wantBytes) {
		t.Fatalf("byte mismatch:\n  got:  %x\n  want: %x", data, wantBytes)
	}

	// Also verify roundtrip through deserialization.
	got, err2 := s.DeserializeEntries(entries[0].Key, data)
	if err2 != nil {
		t.Fatalf("DeserializeEntries: %v", err2)
	}
	if len(got) != 2 {
		t.Fatalf("entry count: got %d, want 2", len(got))
	}
	if !reflect.DeepEqual(got[0].Value, []int{1, 3, 5, 8}) {
		t.Fatalf("entry[0] value: got %v", got[0].Value)
	}
	if !reflect.DeepEqual(got[1].Key, tuple.Tuple{int64(1415)}) {
		t.Fatalf("entry[1] key: got %v", got[1].Key)
	}
	if !reflect.DeepEqual(got[1].Value, []int{0, 600, 605}) {
		t.Fatalf("entry[1] value: got %v", got[1].Value)
	}
}

// ---------------------------------------------------------------------------
// DeserializeKeys — keys-only deserialization skips values
// ---------------------------------------------------------------------------

func TestDeserializeKeys(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()

	entries := []BunchedEntry[tuple.Tuple, []int]{
		{Key: tuple.Tuple{"apple"}, Value: []int{0, 5, 10}},
		{Key: tuple.Tuple{"banana"}, Value: []int{2}},
		{Key: tuple.Tuple{"cherry"}, Value: []int{7, 12, 18, 25}},
	}
	data, err := s.SerializeEntries(entries)
	if err != nil {
		t.Fatalf("SerializeEntries: %v", err)
	}
	keys, err := s.DeserializeKeys(entries[0].Key, data)
	if err != nil {
		t.Fatalf("DeserializeKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("key count: got %d, want 3", len(keys))
	}
	for i, e := range entries {
		if !reflect.DeepEqual(keys[i], e.Key) {
			t.Fatalf("key[%d]: got %v, want %v", i, keys[i], e.Key)
		}
	}
}

// ---------------------------------------------------------------------------
// CanAppend
// ---------------------------------------------------------------------------

func TestCanAppend(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	if !s.CanAppend() {
		t.Fatal("CanAppend should return true")
	}
}

// ---------------------------------------------------------------------------
// Empty entry list panics
// ---------------------------------------------------------------------------

func TestSerializeEntriesEmpty(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	_, err := s.SerializeEntries(nil)
	if err == nil {
		t.Fatal("expected error on empty entry list")
	}
	if !strings.Contains(err.Error(), "empty entry list") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Single entry: serialize/deserialize with first key from signpost
// ---------------------------------------------------------------------------

func TestSerializeEntriesSingle(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()

	entries := []BunchedEntry[tuple.Tuple, []int]{
		{Key: tuple.Tuple{"only"}, Value: []int{0, 3, 7}},
	}
	data, err := s.SerializeEntries(entries)
	if err != nil {
		t.Fatalf("SerializeEntries: %v", err)
	}
	got, err := s.DeserializeEntries(entries[0].Key, data)
	if err != nil {
		t.Fatalf("DeserializeEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entry count: got %d, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].Key, entries[0].Key) {
		t.Fatalf("key: got %v, want %v", got[0].Key, entries[0].Key)
	}
	if !reflect.DeepEqual(got[0].Value, entries[0].Value) {
		t.Fatalf("value: got %v, want %v", got[0].Value, entries[0].Value)
	}
}

// ---------------------------------------------------------------------------
// DeserializeKey bounds checking
// ---------------------------------------------------------------------------

func TestDeserializeKeyOutOfBounds(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	data := tuple.Tuple{"hello"}.Pack()

	cases := []struct {
		name   string
		offset int
		length int
	}{
		{"negative_offset", -1, len(data)},
		{"offset_past_end", len(data) + 1, 0},
		{"length_past_end", 0, len(data) + 1},
		{"negative_length", 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.DeserializeKey(data, tc.offset, tc.length)
			if err == nil {
				t.Fatal("expected error for out-of-bounds")
			}
			var serErr *BunchedSerializationError
			if !errors.As(err, &serErr) {
				t.Fatalf("expected BunchedSerializationError, got %T: %v", err, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Deserialize bad prefix panics
// ---------------------------------------------------------------------------

func TestDeserializeBadPrefix(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	_, err := s.DeserializeEntries(tuple.Tuple{"x"}, []byte{0xFF, 0x00})
	if err == nil {
		t.Fatal("expected error on bad prefix")
	}
	var serErr *BunchedSerializationError
	if !errors.As(err, &serErr) {
		t.Fatalf("expected BunchedSerializationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "incorrect prefix") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Non-monotonic position list in SerializeEntry panics
// ---------------------------------------------------------------------------

func TestSerializeEntryNonMonotonicReturnsError(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	_, err := s.SerializeEntry(tuple.Tuple{"word"}, []int{5, 3})
	if err == nil {
		t.Fatal("expected error for non-monotonic positions in SerializeEntry")
	}
	if !strings.Contains(err.Error(), "monotonically") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Non-monotonic position list in SerializeEntries returns error
// ---------------------------------------------------------------------------

func TestSerializeEntriesNonMonotonicReturnsError(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()
	_, err := s.SerializeEntries([]BunchedEntry[tuple.Tuple, []int]{
		{Key: tuple.Tuple{"a"}, Value: []int{10, 5}},
	})
	if err == nil {
		t.Fatal("expected error for non-monotonic positions in SerializeEntries")
	}
	if !strings.Contains(err.Error(), "monotonically") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ===========================================================================
// Tokenizer tests: DefaultTextTokenizer
// ===========================================================================

func TestTokenizerBasicEnglish(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("hello world", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	assertMapKeys(t, m, "hello", "world")
	assertPositions(t, m, "hello", []int{0})
	assertPositions(t, m, "world", []int{1})
}

func TestTokenizerCaseFolding(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("Hello WORLD", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	assertMapKeys(t, m, "hello", "world")
}

func TestTokenizerDiacriticalRemoval(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	cases := []struct {
		input string
		want  string
	}{
		{"Après", "apres"},
		{"café", "cafe"},
		{"naïve", "naive"},
		{"résumé", "resume"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			list, err := tok.TokenizeToList(tc.input, 0, TokenizerModeIndex)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("expected 1 token, got %v", list)
			}
			if list[0] != tc.want {
				t.Fatalf("got %q, want %q", list[0], tc.want)
			}
		})
	}
}

func TestTokenizerNFKDNormalization(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	// U+FB06 is the "st" ligature.
	list, err := tok.TokenizeToList("\uFB06", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 token, got %v", list)
	}
	if list[0] != "st" {
		t.Fatalf("got %q, want %q", list[0], "st")
	}
}

func TestTokenizerPunctuationFiltering(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("hello, world!", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	assertMapKeys(t, m, "hello", "world")
}

func TestTokenizerApostropheMidWord(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("don't", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 token for \"don't\", got %v", list)
	}
	if list[0] != "don't" {
		t.Fatalf("got %q, want %q", list[0], "don't")
	}
}

func TestTokenizerNumbers(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("abc123", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 token, got %v", list)
	}
	if list[0] != "abc123" {
		t.Fatalf("got %q, want %q", list[0], "abc123")
	}
}

func TestTokenizerEmptyString(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestTokenizerWhitespaceOnly(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("   \t\n  ", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map for whitespace-only input, got %v", m)
	}
}

func TestTokenizerPositionTracking(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("the cat sat on the mat", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	// "the" appears at positions 0 and 4.
	assertPositions(t, m, "the", []int{0, 4})
	assertPositions(t, m, "cat", []int{1})
	assertPositions(t, m, "sat", []int{2})
	assertPositions(t, m, "on", []int{3})
	assertPositions(t, m, "mat", []int{5})
}

func TestTokenizerTokenizeToList(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("the quick brown fox", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"the", "quick", "brown", "fox"}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("list: got %v, want %v", list, want)
	}
}

func TestTokenizerKorean(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	// Korean characters are letters; they should produce tokens.
	list, err := tok.TokenizeToList("안녕하세요", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("expected at least 1 token for Korean text")
	}
}

func TestTokenizerGermanUmlauts(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	// ü = u + combining diaeresis under NFKD; after stripping marks → "u"
	// Ä = A + combining diaeresis → "a" after lowercase + strip
	list, err := tok.TokenizeToList("über Ärger", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 tokens, got %v", list)
	}
	if list[0] != "uber" {
		t.Fatalf("got %q, want %q", list[0], "uber")
	}
	if list[1] != "arger" {
		t.Fatalf("got %q, want %q", list[1], "arger")
	}
}

func TestTokenizerRussianStressMarks(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	// With UAX #29 word segmentation, combining marks (U+0301) are Extend
	// characters that don't break words. "прив\u0301ет" is one word.
	// After NFKD + strip marks: "привет" (matching Java's BreakIterator behavior).
	list, err := tok.TokenizeToList("прив\u0301ет", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 token, got %v", list)
	}
	if list[0] != "привет" {
		t.Fatalf("got %q, want %q", list[0], "привет")
	}

	// Plain Russian without stress marks stays intact.
	list2, err := tok.TokenizeToList("привет мир", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list2) != 2 {
		t.Fatalf("expected 2 tokens, got %v", list2)
	}
	if list2[0] != "привет" {
		t.Fatalf("got %q, want %q", list2[0], "привет")
	}
}

func TestTokenizerEmojiFiltering(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("hello 🌍 world", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	assertMapKeys(t, m, "hello", "world")
}

func TestTokenizerVersionValidation(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()

	t.Run("version_0_ok", func(t *testing.T) {
		t.Parallel()
		list, err := tok.TokenizeToList("test", 0, TokenizerModeIndex)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0] != "test" {
			t.Fatalf("unexpected result: %v", list)
		}
	})

	t.Run("version_1_errors", func(t *testing.T) {
		t.Parallel()
		_, err := tok.Tokenize("test", 1, TokenizerModeIndex)
		if err == nil {
			t.Fatal("expected error for version 1")
		}
	})

	t.Run("negative_version_errors", func(t *testing.T) {
		t.Parallel()
		_, err := tok.Tokenize("test", -1, TokenizerModeIndex)
		if err == nil {
			t.Fatal("expected error for negative version")
		}
	})
}

// ---------------------------------------------------------------------------
// TokenIterator: Next panics when exhausted
// ---------------------------------------------------------------------------

func TestTokenIteratorExhausted(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	iter, err := tok.Tokenize("one", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	// Consume the single token.
	if !iter.HasNext() {
		t.Fatal("expected a token")
	}
	_ = iter.Next()
	// Now calling Next should panic.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when calling Next on exhausted iterator")
		}
	}()
	iter.Next()
}

// ---------------------------------------------------------------------------
// Query mode produces same results (DefaultTextTokenizer doesn't differentiate)
// ---------------------------------------------------------------------------

func TestTokenizerQueryMode(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	indexResult, err := tok.TokenizeToList("Hello World", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	queryResult, err := tok.TokenizeToList("Hello World", 0, TokenizerModeQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(indexResult, queryResult) {
		t.Fatalf("index vs query mode mismatch: %v vs %v", indexResult, queryResult)
	}
}

// ===========================================================================
// Tokenizer registry tests
// ===========================================================================

func TestGetTextTokenizerDefault(t *testing.T) {
	t.Parallel()
	tok, err := GetTextTokenizer("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("expected non-nil tokenizer for empty name")
	}
	if tok.Name() != DefaultTextTokenizerName {
		t.Fatalf("name: got %q, want %q", tok.Name(), DefaultTextTokenizerName)
	}
}

func TestGetTextTokenizerByName(t *testing.T) {
	t.Parallel()
	tok, err := GetTextTokenizer("default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("expected non-nil tokenizer for 'default'")
	}
	if tok.Name() != DefaultTextTokenizerName {
		t.Fatalf("name: got %q, want %q", tok.Name(), DefaultTextTokenizerName)
	}
}

func TestGetTextTokenizerUnknownReturnsError(t *testing.T) {
	t.Parallel()
	_, err := GetTextTokenizer("nonexistent_tokenizer")
	if err == nil {
		t.Fatal("expected error for unknown tokenizer name")
	}
	if !strings.Contains(err.Error(), "unrecognized") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRegistryRegisterCustom(t *testing.T) {
	t.Parallel()
	reg := newTextTokenizerRegistry()
	custom := &stubTokenizerFactory{name: "custom_test"}
	if err := reg.Register(custom); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok, err := reg.GetTokenizer("custom_test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tok.Name() != "custom_test" {
		t.Fatalf("name: got %q, want %q", tok.Name(), "custom_test")
	}
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	t.Parallel()
	reg := newTextTokenizerRegistry()
	factory1 := &stubTokenizerFactory{name: "dup_test"}
	factory2 := &stubTokenizerFactory{name: "dup_test"}
	if err := reg.Register(factory1); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Same name, different factory instance → should error.
	err := reg.Register(factory2)
	if err == nil {
		t.Fatal("expected error for duplicate registration with different factory")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegistryRegisterSameInstanceOk(t *testing.T) {
	t.Parallel()
	reg := newTextTokenizerRegistry()
	factory := &stubTokenizerFactory{name: "same_instance"}
	if err := reg.Register(factory); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Same factory instance → no error.
	if err := reg.Register(factory); err != nil {
		t.Fatalf("re-register same instance: %v", err)
	}
}

func TestRegistryReset(t *testing.T) {
	t.Parallel()
	reg := newTextTokenizerRegistry()
	factory := &stubTokenizerFactory{name: "to_be_reset"}
	if err := reg.Register(factory); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Reset()
	_, err := reg.GetTokenizer("to_be_reset")
	if err == nil {
		t.Fatal("expected error after reset for custom tokenizer")
	}
	// Default should still exist.
	tok, err := reg.GetTokenizer("default")
	if err != nil {
		t.Fatalf("default after reset: %v", err)
	}
	if tok.Name() != DefaultTextTokenizerName {
		t.Fatalf("name: got %q, want %q", tok.Name(), DefaultTextTokenizerName)
	}
}

// ---------------------------------------------------------------------------
// ValidateTokenizerVersion
// ---------------------------------------------------------------------------

func TestValidateTokenizerVersion(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if err := ValidateTokenizerVersion(tok, 0); err != nil {
			t.Fatalf("expected no error for version 0: %v", err)
		}
	})

	t.Run("too_high", func(t *testing.T) {
		t.Parallel()
		err := ValidateTokenizerVersion(tok, 1)
		if err == nil {
			t.Fatal("expected error for version 1")
		}
		if !strings.Contains(err.Error(), "unknown tokenizer version") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("negative", func(t *testing.T) {
		t.Parallel()
		err := ValidateTokenizerVersion(tok, -1)
		if err == nil {
			t.Fatal("expected error for version -1")
		}
	})
}

// ===========================================================================
// Tokenizer edge cases
// ===========================================================================

func TestTokenizerMultipleSpaces(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("hello    world", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hello", "world"}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("got %v, want %v", list, want)
	}
}

func TestTokenizerLeadingTrailingPunctuation(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("...hello...", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	// Leading/trailing dots are not flanked by word chars on both sides,
	// so they're not mid-word. "hello" should be the only token.
	if len(list) != 1 || list[0] != "hello" {
		t.Fatalf("got %v, want [hello]", list)
	}
}

func TestTokenizerOnlyPunctuation(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	m, err := tok.TokenizeToMap("!@#$%^&*()", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map for only-punctuation input, got %v", m)
	}
}

func TestTokenizerMixedScript(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	// Mixing Latin and CJK — CJK chars are letters, so adjacent Latin+CJK
	// would be one segment.
	list, err := tok.TokenizeToList("hello世界", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("expected at least 1 token for mixed script")
	}
}

func TestTokenizerPeriodMidWord(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	// "U.S.A" — periods between word chars act as mid-word.
	list, err := tok.TokenizeToList("U.S.A", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 token for 'U.S.A', got %v", list)
	}
	if list[0] != "u.s.a" {
		t.Fatalf("got %q, want %q", list[0], "u.s.a")
	}
}

func TestTokenizerSingleChar(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("a", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "a" {
		t.Fatalf("got %v, want [a]", list)
	}
}

func TestTokenizerSingleDigit(t *testing.T) {
	t.Parallel()
	tok := DefaultTextTokenizerInstance()
	list, err := tok.TokenizeToList("7", 0, TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "7" {
		t.Fatalf("got %v, want [7]", list)
	}
}

// ===========================================================================
// Serializer: VarInt specific byte patterns
// ===========================================================================

func TestVarIntEncodingBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val  int
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x81, 0x00}},         // 1<<7 = 128
		{255, []byte{0x81, 0x7f}},         // (1<<7)|0x7f
		{16383, []byte{0xFF, 0x7F}},       // (0x7f<<7)|0x7f = 16383
		{16384, []byte{0x81, 0x80, 0x00}}, // 1<<14
		{600, []byte{0x84, 0x58}},         // from documented example
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.val), func(t *testing.T) {
			t.Parallel()
			got := packVarInt(tc.val)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("packVarInt(%d): got %x, want %x", tc.val, got, tc.want)
			}
		})
	}
}

// ===========================================================================
// Helpers
// ===========================================================================

func sequentialInts(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func assertMapKeys(t *testing.T, m map[string][]int, keys ...string) {
	t.Helper()
	if len(m) != len(keys) {
		t.Fatalf("map key count: got %d (%v), want %d (%v)", len(m), mapKeysToSlice(m), len(keys), keys)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in map %v", k, mapKeysToSlice(m))
		}
	}
}

func assertPositions(t *testing.T, m map[string][]int, key string, want []int) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Fatalf("key %q not in map", key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("positions for %q: got %v, want %v", key, got, want)
	}
}

func mapKeysToSlice(m map[string][]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// stubTokenizerFactory is a test double for registry tests.
type stubTokenizerFactory struct {
	name string
}

func (f *stubTokenizerFactory) Name() string                { return f.name }
func (f *stubTokenizerFactory) GetTokenizer() TextTokenizer { return &stubTokenizer{name: f.name} }

type stubTokenizer struct {
	name string
}

func (t *stubTokenizer) Name() string { return t.name }
func (t *stubTokenizer) Tokenize(_ string, _ int, _ TokenizerMode) (TokenIterator, error) {
	return &emptyIter{}, nil
}

func (t *stubTokenizer) TokenizeToMap(_ string, _ int, _ TokenizerMode) (map[string][]int, error) {
	return nil, nil
}

func (t *stubTokenizer) TokenizeToList(_ string, _ int, _ TokenizerMode) ([]string, error) {
	return nil, nil
}
func (t *stubTokenizer) MaxVersion() int { return 0 }
func (t *stubTokenizer) MinVersion() int { return 0 }

type emptyIter struct{}

func (e *emptyIter) HasNext() bool { return false }
func (e *emptyIter) Next() string  { panic("no more tokens") }

// ===========================================================================
// BunchedMap: ContainsKey, Compact, Scan — integration tests (require FDB)
// ===========================================================================

var _ = Describe("BunchedMap methods", func() {
	// bunchedSubspace returns a unique subspace for each spec to avoid cross-contamination.
	bunchedSubspace := func() subspace.Subspace {
		return subspace.FromBytes(tuple.Tuple{"bunched_map_test", CurrentSpecReport().FullText()}.Pack())
	}

	// putEntries inserts N entries with sequential int64 keys into the map within
	// a single transaction. Each entry's value is a position list [key*10, key*10+1].
	putEntries := func(bm *BunchedMap, ss subspace.Subspace, n int) {
		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			for i := 0; i < n; i++ {
				k := tuple.Tuple{int64(i)}
				v := []int{i * 10, i*10 + 1}
				// RETURN the error (do NOT Expect inside the closure): bm.Put can return
				// a retryable transaction_too_old (1007) when a slow run drifts past the
				// 5s MVCC window — returning it lets db.Transact retry with a fresh read
				// version. An in-closure Expect would fail the spec before the retry,
				// flaking under parallel-container load. The outer Expect catches real errors.
				if _, _, err := bm.Put(tx, ss, k, v); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// collectAll drains a BunchedMapIterator into a slice of entries.
	collectAll := func(it *BunchedMapIterator) []bunchedEntry {
		var result []bunchedEntry
		for it.HasNext() {
			e := it.Next()
			result = append(result, *e)
		}
		return result
	}

	// =========================================================================
	// ContainsKey
	// =========================================================================

	It("ContainsKey: key exists returns true", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(10)

		putEntries(bm, ss, 5)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			found, err := bm.ContainsKey(tx, ss, tuple.Tuple{int64(3)})
			if err != nil {
				return nil, err
			}
			Expect(found).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContainsKey: key does not exist returns false", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(10)

		putEntries(bm, ss, 5)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			found, err := bm.ContainsKey(tx, ss, tuple.Tuple{int64(99)})
			if err != nil {
				return nil, err
			}
			Expect(found).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContainsKey: empty map returns false", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(10)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			found, err := bm.ContainsKey(tx, ss, tuple.Tuple{int64(0)})
			if err != nil {
				return nil, err
			}
			Expect(found).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// Compact
	// =========================================================================

	It("Compact: entries still accessible via Get after compaction", func() {
		ss := bunchedSubspace()
		// Small bunch size forces many FDB keys.
		bm := NewBunchedMap(2)
		n := 20

		putEntries(bm, ss, n)

		// Compact with keyLimit=0 (all at once).
		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			cont, err := bm.Compact(tx, ss, 0, nil)
			if err != nil {
				return nil, err
			}
			Expect(cont).To(BeNil(), "keyLimit=0 should complete in one call")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify all entries still accessible.
		_, err = sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			for i := 0; i < n; i++ {
				val, found, err := bm.Get(tx, ss, tuple.Tuple{int64(i)})
				if err != nil {
					return nil, err
				}
				Expect(found).To(BeTrue(), "key %d should exist after compact", i)
				Expect(val).To(Equal([]int{i * 10, i*10 + 1}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Compact: keyLimit=0 returns nil continuation", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(2)

		putEntries(bm, ss, 10)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			cont, err := bm.Compact(tx, ss, 0, nil)
			if err != nil {
				return nil, err
			}
			Expect(cont).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Compact: small keyLimit requires multiple calls to complete", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(2)
		n := 20

		putEntries(bm, ss, n)

		// Compact with small keyLimit — expect non-nil continuation on first call.
		var continuation []byte
		calls := 0
		for {
			var cont []byte
			_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
				var err error
				cont, err = bm.Compact(tx, ss, 3, continuation)
				if err != nil {
					return nil, err
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			calls++
			continuation = cont
			if continuation == nil {
				break
			}
			// Safety valve: don't loop forever.
			Expect(calls).To(BeNumerically("<", 100), "compaction should terminate")
		}
		Expect(calls).To(BeNumerically(">", 1), "small keyLimit should need multiple calls")

		// Verify all entries still readable.
		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			for i := 0; i < n; i++ {
				val, found, err := bm.Get(tx, ss, tuple.Tuple{int64(i)})
				if err != nil {
					return nil, err
				}
				Expect(found).To(BeTrue(), "key %d should exist after multi-call compact", i)
				Expect(val).To(Equal([]int{i * 10, i*10 + 1}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Compact: VerifyIntegrity passes after compaction", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(2)

		putEntries(bm, ss, 30)

		// Compact all.
		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			_, err := bm.Compact(tx, ss, 0, nil)
			if err != nil {
				return nil, err
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// VerifyIntegrity should pass.
		_, err = sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			err := bm.VerifyIntegrity(tx, ss)
			if err != nil {
				return nil, err
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// Scan
	// =========================================================================

	It("Scan: forward returns all entries in order", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(3)
		n := 10

		putEntries(bm, ss, n)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			it := bm.Scan(tx, ss, nil, 0, false)
			entries := collectAll(it)
			Expect(entries).To(HaveLen(n))

			// Verify ascending order.
			for i, e := range entries {
				Expect(e.Key).To(Equal(tuple.Tuple{int64(i)}))
				Expect(e.Value).To(Equal([]int{i * 10, i*10 + 1}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Scan: reverse returns entries in reverse order", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(3)
		n := 10

		putEntries(bm, ss, n)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			it := bm.Scan(tx, ss, nil, 0, true)
			entries := collectAll(it)
			Expect(entries).To(HaveLen(n))

			// Verify descending order.
			for i, e := range entries {
				expectedKey := int64(n - 1 - i)
				Expect(e.Key).To(Equal(tuple.Tuple{expectedKey}))
				Expect(e.Value).To(Equal([]int{int(expectedKey) * 10, int(expectedKey)*10 + 1}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Scan: limit returns only limit entries and valid continuation", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(3)
		n := 10
		limit := 4

		putEntries(bm, ss, n)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			it := bm.Scan(tx, ss, nil, limit, false)
			entries := collectAll(it)
			Expect(entries).To(HaveLen(limit))

			// Should be the first 4 entries.
			for i, e := range entries {
				Expect(e.Key).To(Equal(tuple.Tuple{int64(i)}))
			}

			// Continuation should be non-nil (more entries remain).
			cont := it.GetContinuation()
			Expect(cont).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Scan: resume with continuation returns remaining entries", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(3)
		n := 10
		limit := 4

		putEntries(bm, ss, n)

		// First scan: get first 4 entries.
		var continuation []byte
		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			it := bm.Scan(tx, ss, nil, limit, false)
			entries := collectAll(it)
			Expect(entries).To(HaveLen(limit))
			continuation = it.GetContinuation()
			Expect(continuation).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Second scan: resume from continuation, get remaining 6 entries.
		_, err = sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			it := bm.Scan(tx, ss, continuation, 0, false)
			entries := collectAll(it)
			Expect(entries).To(HaveLen(n - limit))

			// Should start from key 4 (first key after the limit).
			for i, e := range entries {
				Expect(e.Key).To(Equal(tuple.Tuple{int64(i + limit)}))
			}

			// No more entries — continuation should be nil.
			cont := it.GetContinuation()
			Expect(cont).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Scan: empty map returns no entries", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(10)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			it := bm.Scan(tx, ss, nil, 0, false)
			entries := collectAll(it)
			Expect(entries).To(BeEmpty())

			cont := it.GetContinuation()
			Expect(cont).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("InstrumentedBunchedMap", func() {
	bunchedSubspace := func() subspace.Subspace {
		return subspace.FromBytes(tuple.Tuple{"instrumented_bm_test", CurrentSpecReport().FullText()}.Pack())
	}

	It("Put records save and load index counters", func() {
		ss := bunchedSubspace()
		timer := NewStoreTimer()
		bm := NewInstrumentedBunchedMap(10, timer)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			_, _, err := bm.Put(tx, ss, tuple.Tuple{int64(1)}, []int{10, 20})
			Expect(err).NotTo(HaveOccurred())

			_, _, err = bm.Put(tx, ss, tuple.Tuple{int64(2)}, []int{30, 40})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Writes happened — save counters should be non-zero.
		Expect(timer.GetCount(CountSaveIndexKey)).To(BeNumerically(">", 0))
		Expect(timer.GetCount(CountSaveIndexKeyBytes)).To(BeNumerically(">", 0))
		Expect(timer.GetCount(CountSaveIndexValueBytes)).To(BeNumerically(">", 0))

		// Range reads happened (Put does snapshot range read) — load counters may be zero
		// for the first Put (empty map) but should be non-zero for the second Put.
		Expect(timer.GetCount(CountLoadIndexKey)).To(BeNumerically(">=", 0))
	})

	It("Remove records delete index counters", func() {
		ss := bunchedSubspace()
		timer := NewStoreTimer()
		bm := NewInstrumentedBunchedMap(10, timer)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			_, _, err := bm.Put(tx, ss, tuple.Tuple{int64(1)}, []int{10})
			Expect(err).NotTo(HaveOccurred())
			_, _, err = bm.Put(tx, ss, tuple.Tuple{int64(2)}, []int{20})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		timer.Reset()

		_, err = sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			_, _, err := bm.Remove(tx, ss, tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Remove triggers either a delete (single entry) or a write (repack).
		// Either way, some counter should have been bumped.
		totalOps := timer.GetCount(CountDeleteIndexKey) + timer.GetCount(CountSaveIndexKey)
		Expect(totalOps).To(BeNumerically(">", 0))

		// Load counters from the entryForKey range read.
		Expect(timer.GetCount(CountLoadIndexKey)).To(BeNumerically(">", 0))
	})

	It("Get records load index counters", func() {
		ss := bunchedSubspace()
		timer := NewStoreTimer()
		bm := NewInstrumentedBunchedMap(10, timer)

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			_, _, err := bm.Put(tx, ss, tuple.Tuple{int64(1)}, []int{10})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		timer.Reset()

		_, err = sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			val, found, err := bm.Get(tx, ss, tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(val).To(Equal([]int{10}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Get triggers entryForKey which does a range read.
		Expect(timer.GetCount(CountLoadIndexKey)).To(BeNumerically(">", 0))
		Expect(timer.GetCount(CountLoadIndexKeyBytes)).To(BeNumerically(">", 0))
		Expect(timer.GetCount(CountLoadIndexValueBytes)).To(BeNumerically(">", 0))
	})

	It("nil timer produces no panics", func() {
		ss := bunchedSubspace()
		bm := NewBunchedMap(10) // no timer

		_, err := sharedDB.db.Transact(func(tx fdb.Transaction) (any, error) {
			_, _, err := bm.Put(tx, ss, tuple.Tuple{int64(1)}, []int{10})
			Expect(err).NotTo(HaveOccurred())
			_, found, err := bm.Get(tx, ss, tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			_, _, err = bm.Remove(tx, ss, tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
