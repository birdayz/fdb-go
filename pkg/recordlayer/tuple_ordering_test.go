package recordlayer

import (
	"bytes"
	"math"
	"math/big"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// ---------------------------------------------------------------------------
// invertBytes / uninvertBytes round-trip
// ---------------------------------------------------------------------------

func TestInvertBytesRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"single_zero", []byte{0x00}},
		{"single_ff", []byte{0xFF}},
		{"single_middle", []byte{0x42}},
		{"all_zeros", bytes.Repeat([]byte{0x00}, 16)},
		{"all_ff", bytes.Repeat([]byte{0xFF}, 16)},
		{"ascending", []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}},
		{"descending", []byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB}},
		{"alternating", []byte{0xAA, 0x55, 0xAA, 0x55}},
		{"one_byte", []byte{0x80}},
		{"seven_bytes", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}},
		{"eight_bytes", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}},
		{"long", bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inverted := invertBytes(tc.input)
			restored, err := uninvertBytes(inverted)
			if err != nil {
				t.Fatalf("uninvertBytes failed: %v", err)
			}
			if !bytes.Equal(restored, tc.input) {
				t.Fatalf("round-trip mismatch: input=%x, restored=%x", tc.input, restored)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// invertBytes output properties
// ---------------------------------------------------------------------------

func TestInvertBytesOutputProperties(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{
		{},
		{0x00},
		{0x42},
		{0xFF},
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		bytes.Repeat([]byte{0xAB}, 100),
	}

	for _, input := range inputs {
		inverted := invertBytes(input)

		// Check expected output length: (inputLen*8+6)/7 + 1
		expectedLen := (len(input)*8+6)/7 + 1
		if len(inverted) != expectedLen {
			t.Errorf("input len %d: expected output len %d, got %d",
				len(input), expectedLen, len(inverted))
		}

		// All non-terminal bytes must have high bit 0
		for i := 0; i < len(inverted)-1; i++ {
			if (inverted[i] & 0x80) != 0 {
				t.Errorf("input len %d: non-terminal byte %d has high bit set: 0x%02X",
					len(input), i, inverted[i])
			}
		}

		// Terminal byte must have high bit 1
		if len(inverted) > 0 {
			last := inverted[len(inverted)-1]
			if (last & 0x80) == 0 {
				t.Errorf("input len %d: terminal byte has high bit clear: 0x%02X",
					len(input), last)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// invertBytes byte ordering (reversal)
// ---------------------------------------------------------------------------

func TestInvertBytesReverseOrdering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b []byte // a < b lexicographically
	}{
		{"same_len_simple", []byte{0x01}, []byte{0x02}},
		{"same_len_multi", []byte{0x10, 0x20}, []byte{0x10, 0x30}},
		{"prefix_shorter_first", []byte{0x10}, []byte{0x10, 0x00}},
		{"prefix_shorter_second", []byte{0x05, 0x00}, []byte{0x05, 0x00, 0x01}},
		{"zero_vs_one", []byte{0x00}, []byte{0x01}},
		{"ff_boundary", []byte{0xFE}, []byte{0xFF}},
		{"empty_vs_nonempty", []byte{}, []byte{0x00}},
		{"all_zeros_diff_len", []byte{0x00, 0x00}, []byte{0x00, 0x00, 0x00}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Verify precondition: a < b
			if bytes.Compare(tc.a, tc.b) >= 0 {
				t.Fatalf("precondition violated: a=%x should be < b=%x", tc.a, tc.b)
			}
			invertA := invertBytes(tc.a)
			invertB := invertBytes(tc.b)
			// After inversion: invertA > invertB
			if bytes.Compare(invertA, invertB) <= 0 {
				t.Fatalf("inversion did not reverse order: invertA=%x, invertB=%x (a=%x, b=%x)",
					invertA, invertB, tc.a, tc.b)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// uninvertBytes error cases
// ---------------------------------------------------------------------------

func TestUninvertBytesErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"no_high_bit_terminal", []byte{0x00, 0x01, 0x02}},
		{"single_data_byte", []byte{0x7F}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := uninvertBytes(tc.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// packNullsLast / unpackNullsLast round-trip
// ---------------------------------------------------------------------------

func TestPackNullsLastRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		t    tuple.Tuple
	}{
		{"empty", tuple.Tuple{}},
		{"no_nulls_int", tuple.Tuple{int64(42), int64(-1), int64(0)}},
		{"no_nulls_string", tuple.Tuple{"hello", "world"}},
		{"no_nulls_bytes", tuple.Tuple{[]byte{0x01, 0x02}, []byte{0xFF}}},
		{"only_nulls", tuple.Tuple{nil, nil, nil}},
		{"mixed_null_first", tuple.Tuple{nil, int64(1), "foo"}},
		{"mixed_null_middle", tuple.Tuple{int64(1), nil, "foo"}},
		{"mixed_null_last", tuple.Tuple{int64(1), "foo", nil}},
		{"mixed_multiple", tuple.Tuple{nil, int64(42), nil, "hello", nil}},
		{"single_nil", tuple.Tuple{nil}},
		{"single_int", tuple.Tuple{int64(100)}},
		{"single_string", tuple.Tuple{"abc"}},
		{"bool_values", tuple.Tuple{true, false}},
		{"float64", tuple.Tuple{3.14}},
		{"float32", tuple.Tuple{float32(2.71)}},
		{"big_int", tuple.Tuple{int64(math.MaxInt64), int64(math.MinInt64)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			packed := packNullsLast(tc.t)
			unpacked, err := unpackNullsLast(packed)
			if err != nil {
				t.Fatalf("unpackNullsLast failed: %v", err)
			}
			if len(unpacked) != len(tc.t) {
				t.Fatalf("length mismatch: expected %d, got %d", len(tc.t), len(unpacked))
			}
			for i := range tc.t {
				if !tupleElemEqual(tc.t[i], unpacked[i]) {
					t.Errorf("element %d: expected %v (%T), got %v (%T)",
						i, tc.t[i], tc.t[i], unpacked[i], unpacked[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// packNullsLast null encoding check: 0xFE byte
// ---------------------------------------------------------------------------

func TestPackNullsLastNullEncoding(t *testing.T) {
	t.Parallel()

	// A tuple with only a single nil should encode as a single 0xFE byte.
	packed := packNullsLast(tuple.Tuple{nil})
	if len(packed) != 1 || packed[0] != 0xFE {
		t.Fatalf("single nil: expected [0xFE], got %x", packed)
	}

	// Multiple nils = multiple 0xFE bytes.
	packed3 := packNullsLast(tuple.Tuple{nil, nil, nil})
	if len(packed3) != 3 {
		t.Fatalf("three nils: expected 3 bytes, got %d", len(packed3))
	}
	for i, b := range packed3 {
		if b != 0xFE {
			t.Fatalf("three nils byte %d: expected 0xFE, got 0x%02X", i, b)
		}
	}

	// 0xFE should sort after all standard tuple type codes (max standard = 0x33 for versionstamp).
	// The highest standard FDB tuple type code in the spec is well below 0xFE.
	standardTypeCodes := []byte{
		0x00,       // null
		0x01,       // byte string
		0x02,       // unicode string
		0x05,       // nested tuple
		0x0C, 0x13, // negative ints
		0x14,       // zero int
		0x15, 0x1C, // positive ints
		0x20, // float32
		0x21, // float64
		0x26, // false
		0x27, // true
		0x30, // UUID
		0x33, // versionstamp
	}
	for _, tc := range standardTypeCodes {
		if nullLastByte <= tc {
			t.Errorf("nullLastByte 0x%02X does not sort after type code 0x%02X", nullLastByte, tc)
		}
	}
}

// ---------------------------------------------------------------------------
// tupleOrderingPack / tupleOrderingUnpack round-trip for all 4 directions
// ---------------------------------------------------------------------------

func TestTupleOrderingPackUnpackRoundTrip(t *testing.T) {
	t.Parallel()

	directions := []struct {
		name string
		dir  OrderDirection
	}{
		{"ASC_NULLS_FIRST", OrderAscNullsFirst},
		{"ASC_NULLS_LAST", OrderAscNullsLast},
		{"DESC_NULLS_FIRST", OrderDescNullsFirst},
		{"DESC_NULLS_LAST", OrderDescNullsLast},
	}

	tuples := []struct {
		name string
		t    tuple.Tuple
	}{
		{"empty", tuple.Tuple{}},
		{"int_only", tuple.Tuple{int64(42)}},
		{"string_only", tuple.Tuple{"hello"}},
		{"bytes_only", tuple.Tuple{[]byte{0xDE, 0xAD}}},
		{"mixed", tuple.Tuple{int64(1), "abc", []byte{0xFF}}},
		{"with_nil", tuple.Tuple{nil, int64(10)}},
		{"nil_only", tuple.Tuple{nil}},
		{"multi_nil", tuple.Tuple{nil, nil}},
		{"negative", tuple.Tuple{int64(-999)}},
		{"zero", tuple.Tuple{int64(0)}},
		{"bool", tuple.Tuple{true, false}},
		{"float64", tuple.Tuple{float64(3.14159)}},
		{"large_int", tuple.Tuple{int64(math.MaxInt64)}},
		{"min_int", tuple.Tuple{int64(math.MinInt64)}},
		{"nil_middle", tuple.Tuple{int64(1), nil, int64(2)}},
	}

	for _, dir := range directions {
		for _, tc := range tuples {
			t.Run(dir.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				packed := tupleOrderingPack(tc.t, dir.dir)
				unpacked, err := tupleOrderingUnpack(packed, dir.dir)
				if err != nil {
					t.Fatalf("unpack failed: %v", err)
				}
				if len(unpacked) != len(tc.t) {
					t.Fatalf("length mismatch: expected %d, got %d", len(tc.t), len(unpacked))
				}
				for i := range tc.t {
					if !tupleElemEqual(tc.t[i], unpacked[i]) {
						t.Errorf("element %d: expected %v (%T), got %v (%T)",
							i, tc.t[i], tc.t[i], unpacked[i], unpacked[i])
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// ASC_NULLS_FIRST matches standard tuple.Pack for non-null
// ---------------------------------------------------------------------------

func TestAscNullsFirstMatchesStandardPack(t *testing.T) {
	t.Parallel()

	tuples := []tuple.Tuple{
		{int64(42)},
		{"hello"},
		{[]byte{0x01}},
		{int64(1), "abc"},
		{int64(-1)},
		{int64(0)},
		{true},
		{float64(2.71828)},
	}

	for _, tup := range tuples {
		standard := tup.Pack()
		ordered := tupleOrderingPack(tup, OrderAscNullsFirst)
		if !bytes.Equal(standard, ordered) {
			t.Errorf("ASC_NULLS_FIRST should match standard Pack for %v: standard=%x, ordered=%x",
				tup, standard, ordered)
		}
	}
}

// ---------------------------------------------------------------------------
// Ordering correctness: ASC vs DESC, NULLS_FIRST vs NULLS_LAST
// ---------------------------------------------------------------------------

func TestOrderingCorrectnessASC(t *testing.T) {
	t.Parallel()

	// For ASC: smaller values -> smaller bytes
	cases := []struct {
		name   string
		small  tuple.Tuple
		big    tuple.Tuple
		dir    OrderDirection
		expect int // -1 means small<big in bytes, +1 means small>big
	}{
		// ASC: small < big
		{"asc_int", tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)}, OrderAscNullsFirst, -1},
		{"asc_string", tuple.Tuple{"aaa"}, tuple.Tuple{"bbb"}, OrderAscNullsFirst, -1},
		{"asc_bytes", tuple.Tuple{[]byte{0x01}}, tuple.Tuple{[]byte{0x02}}, OrderAscNullsFirst, -1},
		{"asc_neg_int", tuple.Tuple{int64(-10)}, tuple.Tuple{int64(-1)}, OrderAscNullsFirst, -1},

		// DESC: small > big (reversed)
		{"desc_int", tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)}, OrderDescNullsLast, 1},
		{"desc_string", tuple.Tuple{"aaa"}, tuple.Tuple{"bbb"}, OrderDescNullsLast, 1},
		{"desc_neg_int", tuple.Tuple{int64(-10)}, tuple.Tuple{int64(-1)}, OrderDescNullsLast, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			packedSmall := tupleOrderingPack(tc.small, tc.dir)
			packedBig := tupleOrderingPack(tc.big, tc.dir)
			cmp := bytes.Compare(packedSmall, packedBig)
			if tc.expect < 0 && cmp >= 0 {
				t.Fatalf("expected small < big in bytes, got cmp=%d (small=%x, big=%x)",
					cmp, packedSmall, packedBig)
			}
			if tc.expect > 0 && cmp <= 0 {
				t.Fatalf("expected small > big in bytes (DESC), got cmp=%d (small=%x, big=%x)",
					cmp, packedSmall, packedBig)
			}
		})
	}
}

func TestOrderingCorrectnessNulls(t *testing.T) {
	t.Parallel()

	// NULLS_FIRST: null sorts before non-null
	packedNull := tupleOrderingPack(tuple.Tuple{nil}, OrderAscNullsFirst)
	packedVal := tupleOrderingPack(tuple.Tuple{int64(0)}, OrderAscNullsFirst)
	if bytes.Compare(packedNull, packedVal) >= 0 {
		t.Errorf("NULLS_FIRST: null should sort before value: null=%x, val=%x",
			packedNull, packedVal)
	}

	// NULLS_LAST: null sorts after non-null
	packedNullLast := tupleOrderingPack(tuple.Tuple{nil}, OrderAscNullsLast)
	packedValLast := tupleOrderingPack(tuple.Tuple{int64(0)}, OrderAscNullsLast)
	if bytes.Compare(packedNullLast, packedValLast) <= 0 {
		t.Errorf("NULLS_LAST: null should sort after value: null=%x, val=%x",
			packedNullLast, packedValLast)
	}

	// DESC_NULLS_FIRST: null sorts before non-null values (in DESC byte space)
	packedNullDescFirst := tupleOrderingPack(tuple.Tuple{nil}, OrderDescNullsFirst)
	packedValDescFirst := tupleOrderingPack(tuple.Tuple{int64(0)}, OrderDescNullsFirst)
	if bytes.Compare(packedNullDescFirst, packedValDescFirst) >= 0 {
		t.Errorf("DESC_NULLS_FIRST: null should sort before value: null=%x, val=%x",
			packedNullDescFirst, packedValDescFirst)
	}

	// DESC_NULLS_LAST: null sorts after non-null values (in DESC byte space)
	packedNullDescLast := tupleOrderingPack(tuple.Tuple{nil}, OrderDescNullsLast)
	packedValDescLast := tupleOrderingPack(tuple.Tuple{int64(0)}, OrderDescNullsLast)
	if bytes.Compare(packedNullDescLast, packedValDescLast) <= 0 {
		t.Errorf("DESC_NULLS_LAST: null should sort after value: null=%x, val=%x",
			packedNullDescLast, packedValDescLast)
	}
}

// Verify null vs multiple non-null values in ASC_NULLS_LAST
func TestNullsLastSortsAfterAllTypes(t *testing.T) {
	t.Parallel()

	nonNullTuples := []tuple.Tuple{
		{int64(0)},
		{int64(-1)},
		{int64(math.MaxInt64)},
		{""},
		{"zzz"},
		{[]byte{}},
		{[]byte{0xFF}},
		{true},
		{false},
		{float64(0)},
		{float64(math.MaxFloat64)},
	}

	packedNull := tupleOrderingPack(tuple.Tuple{nil}, OrderAscNullsLast)
	for _, tup := range nonNullTuples {
		packed := tupleOrderingPack(tup, OrderAscNullsLast)
		if bytes.Compare(packedNull, packed) <= 0 {
			t.Errorf("ASC_NULLS_LAST: null should sort after %v: null=%x, val=%x",
				tup, packedNull, packed)
		}
	}
}

// ---------------------------------------------------------------------------
// tupleElementEndPos
// ---------------------------------------------------------------------------

func TestTupleElementEndPos(t *testing.T) {
	t.Parallel()

	// Helper: pack a single-element tuple and verify tupleElementEndPos finds
	// the correct boundary.
	testSingleElement := func(t *testing.T, name string, elem any) {
		t.Helper()
		packed := tuple.Tuple{elem}.Pack()
		endPos, err := tupleElementEndPos(packed, 0)
		if err != nil {
			t.Fatalf("%s: tupleElementEndPos error: %v", name, err)
		}
		if endPos != len(packed) {
			t.Fatalf("%s: expected endPos=%d, got %d (packed=%x)", name, len(packed), endPos, packed)
		}
	}

	t.Run("null", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "null", nil)
	})

	t.Run("byte_string_empty", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "byte_string_empty", []byte{})
	})

	t.Run("byte_string_data", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "byte_string_data", []byte{0x01, 0x02, 0x03})
	})

	t.Run("byte_string_with_null", func(t *testing.T) {
		t.Parallel()
		// Byte string containing null bytes (escaped as 0x00 0xFF)
		testSingleElement(t, "byte_string_with_null", []byte{0x00, 0x01, 0x00})
	})

	t.Run("unicode_string_empty", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "unicode_string_empty", "")
	})

	t.Run("unicode_string_data", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "unicode_string_data", "hello world")
	})

	t.Run("unicode_string_utf8", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "unicode_string_utf8", "日本語テスト")
	})

	t.Run("int_zero", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_zero", int64(0))
	})

	t.Run("int_positive_small", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_positive_small", int64(1))
	})

	t.Run("int_positive_1byte", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_positive_1byte", int64(255))
	})

	t.Run("int_positive_2bytes", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_positive_2bytes", int64(256))
	})

	t.Run("int_positive_4bytes", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_positive_4bytes", int64(1<<24+1))
	})

	t.Run("int_positive_8bytes", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_positive_8bytes", int64(math.MaxInt64))
	})

	t.Run("int_negative_small", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_negative_small", int64(-1))
	})

	t.Run("int_negative_1byte", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_negative_1byte", int64(-255))
	})

	t.Run("int_negative_2bytes", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_negative_2bytes", int64(-256))
	})

	t.Run("int_negative_8bytes", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "int_negative_8bytes", int64(math.MinInt64))
	})

	t.Run("float32", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "float32", float32(3.14))
	})

	t.Run("float32_zero", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "float32_zero", float32(0))
	})

	t.Run("float32_negative", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "float32_negative", float32(-1.5))
	})

	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "float64", float64(2.718281828))
	})

	t.Run("float64_zero", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "float64_zero", float64(0))
	})

	t.Run("float64_negative", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "float64_negative", float64(-99.99))
	})

	t.Run("bool_true", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "bool_true", true)
	})

	t.Run("bool_false", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "bool_false", false)
	})

	t.Run("uuid", func(t *testing.T) {
		t.Parallel()
		u := tuple.UUID{
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
		}
		testSingleElement(t, "uuid", u)
	})

	t.Run("versionstamp", func(t *testing.T) {
		t.Parallel()
		vs := tuple.Versionstamp{
			TransactionVersion: [10]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A},
			UserVersion:        42,
		}
		testSingleElement(t, "versionstamp", vs)
	})

	t.Run("nested_tuple_empty", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "nested_tuple_empty", tuple.Tuple{})
	})

	t.Run("nested_tuple_with_values", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "nested_tuple_with_values", tuple.Tuple{int64(1), "two"})
	})

	t.Run("nested_tuple_with_null", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "nested_tuple_with_null", tuple.Tuple{nil, int64(1)})
	})

	t.Run("double_nested", func(t *testing.T) {
		t.Parallel()
		testSingleElement(t, "double_nested", tuple.Tuple{tuple.Tuple{int64(42)}})
	})
}

// Test tupleElementEndPos with multi-element tuple (parsing from middle).
func TestTupleElementEndPosMultiElement(t *testing.T) {
	t.Parallel()

	// Pack a multi-element tuple, then parse element by element.
	tup := tuple.Tuple{int64(42), "hello", []byte{0xDE, 0xAD}, nil, true}
	packed := tup.Pack()

	pos := 0
	for i := 0; i < len(tup); i++ {
		if pos >= len(packed) {
			t.Fatalf("ran out of bytes at element %d", i)
		}
		endPos, err := tupleElementEndPos(packed, pos)
		if err != nil {
			t.Fatalf("element %d at pos %d: error: %v", i, pos, err)
		}
		if endPos <= pos {
			t.Fatalf("element %d: endPos %d <= pos %d", i, endPos, pos)
		}
		// Verify this single element unpacks correctly.
		single, err := tuple.Unpack(packed[pos:endPos])
		if err != nil {
			t.Fatalf("element %d: unpack error: %v", i, err)
		}
		if len(single) != 1 {
			t.Fatalf("element %d: expected 1 element, got %d", i, len(single))
		}
		pos = endPos
	}
	if pos != len(packed) {
		t.Fatalf("did not consume all bytes: pos=%d, len=%d", pos, len(packed))
	}
}

// Test tupleElementEndPos error case: position beyond data.
func TestTupleElementEndPosBeyondData(t *testing.T) {
	t.Parallel()
	_, err := tupleElementEndPos([]byte{0x14}, 5)
	if err == nil {
		t.Fatal("expected error for position beyond data")
	}
}

// Test tupleElementEndPos error case: unknown type code.
func TestTupleElementEndPosUnknownTypeCode(t *testing.T) {
	t.Parallel()
	_, err := tupleElementEndPos([]byte{0xAA}, 0)
	if err == nil {
		t.Fatal("expected error for unknown type code 0xAA")
	}
}

// Test tupleElementEndPos error case: unterminated byte string.
func TestTupleElementEndPosUnterminatedString(t *testing.T) {
	t.Parallel()
	// Type code 0x01 (byte string) followed by data but no 0x00 terminator.
	_, err := tupleElementEndPos([]byte{0x01, 0x42, 0x43}, 0)
	if err == nil {
		t.Fatal("expected error for unterminated byte string")
	}
}

// ---------------------------------------------------------------------------
// packNullsLast / unpackNullsLast with nested tuples
// ---------------------------------------------------------------------------

func TestPackNullsLastNestedTuple(t *testing.T) {
	t.Parallel()

	// Nested tuple with nil elements inside
	inner := tuple.Tuple{int64(1), nil, "hello"}
	original := tuple.Tuple{inner, int64(42)}

	packed := packNullsLast(original)
	unpacked, err := unpackNullsLast(packed)
	if err != nil {
		t.Fatalf("unpackNullsLast: %v", err)
	}
	if len(unpacked) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(unpacked))
	}
	// Second element should be int64(42)
	if unpacked[1] != int64(42) {
		t.Errorf("expected 42, got %v", unpacked[1])
	}
}

func TestPackNullsLastAllNulls(t *testing.T) {
	t.Parallel()

	original := tuple.Tuple{nil, nil, nil}
	packed := packNullsLast(original)

	// Each null should be a single 0xFE byte
	if len(packed) != 3 {
		t.Fatalf("expected 3 bytes for 3 nulls, got %d: %x", len(packed), packed)
	}
	for i, b := range packed {
		if b != 0xFE {
			t.Errorf("byte %d: expected 0xFE, got 0x%02X", i, b)
		}
	}

	unpacked, err := unpackNullsLast(packed)
	if err != nil {
		t.Fatalf("unpackNullsLast: %v", err)
	}
	if len(unpacked) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(unpacked))
	}
	for i, v := range unpacked {
		if v != nil {
			t.Errorf("element %d: expected nil, got %v", i, v)
		}
	}
}

func TestPackNullsLastMixedTypesRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []tuple.Tuple{
		{nil, int64(0), nil, "", nil},
		{int64(-1), nil, true, nil, []byte{0x00}},
		{float64(3.14), nil, "café", nil},
	}

	for i, original := range cases {
		packed := packNullsLast(original)
		unpacked, err := unpackNullsLast(packed)
		if err != nil {
			t.Fatalf("case %d: unpackNullsLast: %v", i, err)
		}
		if len(unpacked) != len(original) {
			t.Fatalf("case %d: expected %d elements, got %d", i, len(original), len(unpacked))
		}
		for j := range original {
			if original[j] == nil {
				if unpacked[j] != nil {
					t.Errorf("case %d element %d: expected nil, got %v", i, j, unpacked[j])
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// invertBytes edge cases
// ---------------------------------------------------------------------------

func TestInvertBytesLargeInput(t *testing.T) {
	t.Parallel()

	// 1000-byte input to verify no overflow in the 7-bit packing
	input := make([]byte, 1000)
	for i := range input {
		input[i] = byte(i % 256)
	}
	inverted := invertBytes(input)
	result, err := uninvertBytes(inverted)
	if err != nil {
		t.Fatalf("uninvertBytes: %v", err)
	}
	if !bytes.Equal(input, result) {
		t.Fatal("round-trip failed for 1000-byte input")
	}
}

// ---------------------------------------------------------------------------
// orderDirectionFromName
// ---------------------------------------------------------------------------

func TestOrderDirectionFromName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		expected OrderDirection
		ok       bool
	}{
		{"order_asc_nulls_first", OrderAscNullsFirst, true},
		{"order_asc_nulls_last", OrderAscNullsLast, true},
		{"order_desc_nulls_first", OrderDescNullsFirst, true},
		{"order_desc_nulls_last", OrderDescNullsLast, true},
		{"unknown", OrderDirection{}, false},
		{"", OrderDirection{}, false},
		{"ORDER_ASC_NULLS_FIRST", OrderDirection{}, false}, // case-sensitive
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, ok := orderDirectionFromName(tc.name)
			if ok != tc.ok {
				t.Fatalf("expected ok=%v, got %v", tc.ok, ok)
			}
			if ok && dir != tc.expected {
				t.Fatalf("expected %+v, got %+v", tc.expected, dir)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Order function evaluator via FunctionExpr
// ---------------------------------------------------------------------------

func TestOrderFunctionEvaluatorAscNullsFirst(t *testing.T) {
	t.Parallel()

	expr := FunctionExpr(OrderFuncAscNullsFirst, Field("price"))
	// Simulate evaluation: the function evaluator receives pre-evaluated arguments.
	// We call the evaluator directly since Field evaluation requires a proto message.
	evaluator := makeOrderEvaluator(OrderAscNullsFirst)
	results, err := evaluator(nil, nil, [][]any{{int64(42)}})
	if err != nil {
		t.Fatalf("evaluator error: %v", err)
	}
	if len(results) != 1 || len(results[0]) != 1 {
		t.Fatalf("expected 1 result with 1 element, got %v", results)
	}
	packed, ok := results[0][0].([]byte)
	if !ok {
		t.Fatalf("expected []byte result, got %T", results[0][0])
	}
	// Verify it matches standard tuple pack for ASC_NULLS_FIRST with non-null.
	expected := tuple.Tuple{int64(42)}.Pack()
	if !bytes.Equal(packed, expected) {
		t.Fatalf("ASC_NULLS_FIRST(42): expected %x, got %x", expected, packed)
	}
	_ = expr // confirm type is correct
}

func TestOrderFunctionEvaluatorDescNullsLast(t *testing.T) {
	t.Parallel()

	evaluator := makeOrderEvaluator(OrderDescNullsLast)
	results, err := evaluator(nil, nil, [][]any{{"hello"}})
	if err != nil {
		t.Fatalf("evaluator error: %v", err)
	}
	packed := results[0][0].([]byte)
	// DESC: should be inverted. Verify by unpacking.
	unpacked, err := tupleOrderingUnpack(packed, OrderDescNullsLast)
	if err != nil {
		t.Fatalf("unpack error: %v", err)
	}
	if len(unpacked) != 1 || unpacked[0] != "hello" {
		t.Fatalf("expected [hello], got %v", unpacked)
	}
}

func TestOrderFunctionEvaluatorAscNullsLastWithNil(t *testing.T) {
	t.Parallel()

	evaluator := makeOrderEvaluator(OrderAscNullsLast)
	results, err := evaluator(nil, nil, [][]any{{nil}})
	if err != nil {
		t.Fatalf("evaluator error: %v", err)
	}
	packed := results[0][0].([]byte)
	// Should be 0xFE (nulls-last encoding).
	if len(packed) != 1 || packed[0] != 0xFE {
		t.Fatalf("expected [0xFE] for nil with ASC_NULLS_LAST, got %x", packed)
	}
}

func TestOrderFunctionEvaluatorMultipleArguments(t *testing.T) {
	t.Parallel()

	evaluator := makeOrderEvaluator(OrderDescNullsFirst)
	// Multiple argument groups (fan-out scenario).
	results, err := evaluator(nil, nil, [][]any{
		{int64(1)},
		{int64(2)},
		{int64(3)},
	})
	if err != nil {
		t.Fatalf("evaluator error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// Verify DESC ordering: packed(1) > packed(2) > packed(3) in byte comparison.
	p1 := results[0][0].([]byte)
	p2 := results[1][0].([]byte)
	p3 := results[2][0].([]byte)
	if bytes.Compare(p1, p2) <= 0 {
		t.Errorf("DESC: packed(1) should be > packed(2): %x vs %x", p1, p2)
	}
	if bytes.Compare(p2, p3) <= 0 {
		t.Errorf("DESC: packed(2) should be > packed(3): %x vs %x", p2, p3)
	}
}

func TestOrderFunctionEvaluatorRoundTrip(t *testing.T) {
	t.Parallel()

	directions := []struct {
		name string
		dir  OrderDirection
	}{
		{"asc_nulls_first", OrderAscNullsFirst},
		{"asc_nulls_last", OrderAscNullsLast},
		{"desc_nulls_first", OrderDescNullsFirst},
		{"desc_nulls_last", OrderDescNullsLast},
	}

	values := []struct {
		name string
		args []any
	}{
		{"int", []any{int64(42)}},
		{"string", []any{"test"}},
		{"nil", []any{nil}},
		{"multi", []any{int64(1), "two"}},
		{"negative", []any{int64(-100)}},
	}

	for _, dir := range directions {
		for _, val := range values {
			t.Run(dir.name+"/"+val.name, func(t *testing.T) {
				t.Parallel()
				evaluator := makeOrderEvaluator(dir.dir)
				results, err := evaluator(nil, nil, [][]any{val.args})
				if err != nil {
					t.Fatalf("evaluator error: %v", err)
				}
				packed := results[0][0].([]byte)
				unpacked, err := tupleOrderingUnpack(packed, dir.dir)
				if err != nil {
					t.Fatalf("unpack error: %v", err)
				}
				if len(unpacked) != len(val.args) {
					t.Fatalf("length mismatch: expected %d, got %d", len(val.args), len(unpacked))
				}
				for i := range val.args {
					if !tupleElemEqual(val.args[i], unpacked[i]) {
						t.Errorf("element %d: expected %v (%T), got %v (%T)",
							i, val.args[i], val.args[i], unpacked[i], unpacked[i])
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Proto round-trip for FunctionKeyExpression
// ---------------------------------------------------------------------------

func TestOrderFunctionProtoRoundTrip(t *testing.T) {
	t.Parallel()

	funcNames := []string{
		OrderFuncAscNullsFirst,
		OrderFuncAscNullsLast,
		OrderFuncDescNullsFirst,
		OrderFuncDescNullsLast,
	}

	for _, name := range funcNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			original := FunctionExpr(name, Field("price"))
			proto := original.ToKeyExpression()

			// Verify proto structure.
			if proto.Function == nil {
				t.Fatal("proto.Function is nil")
			}
			if proto.Function.GetName() != name {
				t.Fatalf("expected name %q, got %q", name, proto.Function.GetName())
			}
			if proto.Function.Arguments == nil {
				t.Fatal("proto.Function.Arguments is nil")
			}

			// Deserialize back.
			restored, err := KeyExpressionFromProto(proto)
			if err != nil {
				t.Fatalf("KeyExpressionFromProto error: %v", err)
			}
			funcExpr, ok := restored.(*FunctionKeyExpression)
			if !ok {
				t.Fatalf("expected *FunctionKeyExpression, got %T", restored)
			}
			if funcExpr.Name() != name {
				t.Fatalf("restored name: expected %q, got %q", name, funcExpr.Name())
			}
			// Verify arguments survived.
			argField, ok := funcExpr.Arguments().(*FieldKeyExpression)
			if !ok {
				t.Fatalf("expected *FieldKeyExpression arguments, got %T", funcExpr.Arguments())
			}
			if argField.fieldName != "price" {
				t.Fatalf("expected field name 'price', got %q", argField.fieldName)
			}
		})
	}
}

// Test proto round-trip with composite arguments (Concat).
func TestOrderFunctionProtoRoundTripComposite(t *testing.T) {
	t.Parallel()

	original := FunctionExpr(OrderFuncDescNullsLast, Concat(Field("a"), Field("b")))
	proto := original.ToKeyExpression()

	restored, err := KeyExpressionFromProto(proto)
	if err != nil {
		t.Fatalf("KeyExpressionFromProto error: %v", err)
	}
	funcExpr := restored.(*FunctionKeyExpression)
	if funcExpr.Name() != OrderFuncDescNullsLast {
		t.Fatalf("expected name %q, got %q", OrderFuncDescNullsLast, funcExpr.Name())
	}
}

// ---------------------------------------------------------------------------
// OrderFuncExpr convenience
// ---------------------------------------------------------------------------

func TestOrderFuncExpr(t *testing.T) {
	t.Parallel()

	expr := OrderFuncExpr(OrderFuncAscNullsFirst, Field("x"))
	if expr.Name() != OrderFuncAscNullsFirst {
		t.Fatalf("expected %q, got %q", OrderFuncAscNullsFirst, expr.Name())
	}
}

// ---------------------------------------------------------------------------
// Edge: empty tuple ordering pack/unpack
// ---------------------------------------------------------------------------

func TestTupleOrderingPackEmptyTuple(t *testing.T) {
	t.Parallel()

	for _, dir := range []OrderDirection{
		OrderAscNullsFirst, OrderAscNullsLast,
		OrderDescNullsFirst, OrderDescNullsLast,
	} {
		packed := tupleOrderingPack(tuple.Tuple{}, dir)
		unpacked, err := tupleOrderingUnpack(packed, dir)
		if err != nil {
			t.Fatalf("dir=%+v: unpack empty tuple error: %v", dir, err)
		}
		if len(unpacked) != 0 {
			t.Fatalf("dir=%+v: expected empty tuple, got %v", dir, unpacked)
		}
	}
}

// ---------------------------------------------------------------------------
// Stability: same input always produces same output
// ---------------------------------------------------------------------------

func TestTupleOrderingPackDeterministic(t *testing.T) {
	t.Parallel()

	tup := tuple.Tuple{int64(42), "abc", nil, []byte{0xFF}}
	for _, dir := range []OrderDirection{
		OrderAscNullsFirst, OrderAscNullsLast,
		OrderDescNullsFirst, OrderDescNullsLast,
	} {
		first := tupleOrderingPack(tup, dir)
		for i := 0; i < 100; i++ {
			again := tupleOrderingPack(tup, dir)
			if !bytes.Equal(first, again) {
				t.Fatalf("non-deterministic packing on iteration %d for dir=%+v", i, dir)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// DESC ordering: verify int sequence sorts in reverse byte order
// ---------------------------------------------------------------------------

func TestDescIntSequenceOrdering(t *testing.T) {
	t.Parallel()

	values := []int64{-100, -1, 0, 1, 50, 100, 1000, math.MaxInt64}
	packed := make([][]byte, len(values))
	for i, v := range values {
		packed[i] = tupleOrderingPack(tuple.Tuple{v}, OrderDescNullsLast)
	}
	// In DESC: larger values should have smaller bytes.
	for i := 0; i < len(packed)-1; i++ {
		if bytes.Compare(packed[i], packed[i+1]) <= 0 {
			t.Errorf("DESC order violation at index %d: packed(%d)=%x should be > packed(%d)=%x",
				i, values[i], packed[i], values[i+1], packed[i+1])
		}
	}
}

// ---------------------------------------------------------------------------
// ASC ordering: verify string sequence sorts in forward byte order
// ---------------------------------------------------------------------------

func TestAscStringSequenceOrdering(t *testing.T) {
	t.Parallel()

	values := []string{"", "a", "aa", "ab", "b", "z", "zz"}
	packed := make([][]byte, len(values))
	for i, v := range values {
		packed[i] = tupleOrderingPack(tuple.Tuple{v}, OrderAscNullsFirst)
	}
	for i := 0; i < len(packed)-1; i++ {
		if bytes.Compare(packed[i], packed[i+1]) >= 0 {
			t.Errorf("ASC order violation at index %d: packed(%q)=%x should be < packed(%q)=%x",
				i, values[i], packed[i], values[i+1], packed[i+1])
		}
	}
}

// ---------------------------------------------------------------------------
// gen.Function proto constant check
// ---------------------------------------------------------------------------

func TestOrderFunctionConstants(t *testing.T) {
	t.Parallel()

	// Verify the function name constants are what we expect.
	if OrderFuncAscNullsFirst != "order_asc_nulls_first" {
		t.Fatalf("unexpected constant: %s", OrderFuncAscNullsFirst)
	}
	if OrderFuncAscNullsLast != "order_asc_nulls_last" {
		t.Fatalf("unexpected constant: %s", OrderFuncAscNullsLast)
	}
	if OrderFuncDescNullsFirst != "order_desc_nulls_first" {
		t.Fatalf("unexpected constant: %s", OrderFuncDescNullsFirst)
	}
	if OrderFuncDescNullsLast != "order_desc_nulls_last" {
		t.Fatalf("unexpected constant: %s", OrderFuncDescNullsLast)
	}
}

// ---------------------------------------------------------------------------
// Gen proto Function message fields populated correctly
// ---------------------------------------------------------------------------

func TestOrderFunctionProtoFieldsPopulated(t *testing.T) {
	t.Parallel()

	expr := FunctionExpr(OrderFuncDescNullsLast, Field("price"))
	p := expr.ToKeyExpression()

	// The Function message should have Name set.
	fn := p.GetFunction()
	if fn == nil {
		t.Fatal("Function field is nil")
	}
	if fn.Name == nil {
		t.Fatal("Function.Name is nil (should be pointer to string)")
	}
	if *fn.Name != OrderFuncDescNullsLast {
		t.Fatalf("expected %q, got %q", OrderFuncDescNullsLast, *fn.Name)
	}

	// Arguments should be a KeyExpression with Field set.
	args := fn.GetArguments()
	if args == nil {
		t.Fatal("Arguments is nil")
	}
	if args.GetField() == nil {
		t.Fatal("Arguments.Field is nil")
	}

	// Suppress unused import warning for gen.
	_ = (*gen.Function)(nil)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// tupleElemEqual compares two tuple elements for equality, handling type
// normalization (int64 from tuple.Unpack, nil, []byte, etc.).
func tupleElemEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch av := a.(type) {
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float32:
		bv, ok := b.(float32)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case tuple.UUID:
		bv, ok := b.(tuple.UUID)
		return ok && av == bv
	case tuple.Versionstamp:
		bv, ok := b.(tuple.Versionstamp)
		return ok && av == bv
	default:
		// For nested tuples and other complex types, fall back to
		// string comparison (good enough for test assertions).
		return false
	}
}

// TestTupleElementEndPosBigInt verifies arbitrary precision integer handling.
func TestTupleElementEndPosBigInt(t *testing.T) {
	t.Parallel()

	t.Run("bigint_positive", func(t *testing.T) {
		t.Parallel()
		bigVal := new(big.Int).Lsh(big.NewInt(1), 128) // 2^128 — uses type code 0x1D
		packed := tuple.Tuple{bigVal}.Pack()
		end, err := tupleElementEndPos(packed, 0)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if end != len(packed) {
			t.Errorf("got %d, want %d", end, len(packed))
		}
	})

	t.Run("bigint_negative", func(t *testing.T) {
		t.Parallel()
		bigVal := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 128)) // -2^128 — uses type code 0x0B
		packed := tuple.Tuple{bigVal}.Pack()
		end, err := tupleElementEndPos(packed, 0)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if end != len(packed) {
			t.Errorf("got %d, want %d", end, len(packed))
		}
	})
}
