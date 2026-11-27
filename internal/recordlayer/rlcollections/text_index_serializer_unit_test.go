package rlcollections

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
			encoded := appendVarInt(nil, tc.val)
			decoded, next, canonical, err := readVarInt(encoded, 0)
			if err != nil {
				t.Fatalf("readVarInt(%x): %v", encoded, err)
			}
			if next != len(encoded) || !canonical {
				t.Fatalf("readVarInt(%x): next=%d, canonical=%v", encoded, next, canonical)
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
			encoded := appendVarInt(nil, tc.val)
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
			encoded := appendPositionList(nil, tc.list, size)
			got := []int{}
			next, count, canonical, err := walkPositionList(encoded, 0, &got)
			if err != nil {
				t.Fatalf("walkPositionList: %v", err)
			}
			if next != len(encoded) || count != len(tc.list) || !canonical {
				t.Fatalf("walkPositionList: next=%d, count=%d, canonical=%v", next, count, canonical)
			}
			skipNext, skipCount, skipCanonical, err := walkPositionList(encoded, 0, nil)
			if err != nil || skipNext != next || skipCount != count || skipCanonical != canonical {
				t.Fatalf("walkPositionList without output: next=%d, count=%d, canonical=%v, err=%v", skipNext, skipCount, skipCanonical, err)
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
			keyLen, pos, canonical, err := readVarInt(data, 0)
			if err != nil {
				t.Fatalf("reading key length: %v", err)
			}
			if !canonical || keyLen < 0 || keyLen > len(data)-pos {
				t.Fatalf("invalid key length %d at %d (canonical=%v)", keyLen, pos, canonical)
			}
			parsedKey, err := tuple.Unpack(data[pos : pos+keyLen])
			if err != nil {
				t.Fatalf("unpacking key: %v", err)
			}
			if !reflect.DeepEqual(parsedKey, tc.key) {
				t.Fatalf("key mismatch: got %v, want %v", parsedKey, tc.key)
			}
			positions := []int{}
			next, count, canonical, err := walkPositionList(data, pos+keyLen, &positions)
			if err != nil {
				t.Fatalf("reading positions: %v", err)
			}
			if next != len(data) || count != len(tc.positions) || !canonical {
				t.Fatalf("reading positions: next=%d, count=%d, canonical=%v", next, count, canonical)
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
			got := appendVarInt(nil, tc.val)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("appendVarInt(%d): got %x, want %x", tc.val, got, tc.want)
			}
			decoded, next, canonical, err := readVarInt(tc.want, 0)
			if err != nil || decoded != tc.val || next != len(tc.want) || !canonical {
				t.Fatalf("readVarInt(%x): value=%d, next=%d, canonical=%v, err=%v", tc.want, decoded, next, canonical, err)
			}
		})
	}
}

func sequentialInts(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}
