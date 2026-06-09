package recordlayer

import (
	"math"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// TestFastUnpackEquivalence verifies fastUnpack produces identical results
// to tuple.Unpack across a wide range of tuple element types.
func TestFastUnpackEquivalence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		tuple tuple.Tuple
	}{
		{"empty", tuple.Tuple{}},
		{"nil", tuple.Tuple{nil}},
		{"zero", tuple.Tuple{int64(0)}},
		{"positive_small", tuple.Tuple{int64(42)}},
		{"positive_large", tuple.Tuple{int64(1 << 40)}},
		{"negative_small", tuple.Tuple{int64(-1)}},
		{"negative_large", tuple.Tuple{int64(-1 << 40)}},
		{"max_int64", tuple.Tuple{int64(math.MaxInt64)}},
		{"min_int64", tuple.Tuple{int64(math.MinInt64)}},
		{"string", tuple.Tuple{"hello"}},
		{"empty_string", tuple.Tuple{""}},
		{"string_with_null", tuple.Tuple{"hel\x00lo"}},
		{"bytes", tuple.Tuple{[]byte{1, 2, 3}}},
		{"bytes_with_null", tuple.Tuple{[]byte{0, 1, 0}}},
		{"true", tuple.Tuple{true}},
		{"false", tuple.Tuple{false}},
		{"float32", tuple.Tuple{float32(3.14)}},
		{"float64", tuple.Tuple{float64(2.718281828)}},
		{"float64_negative", tuple.Tuple{float64(-1.5)}},
		{"float64_zero", tuple.Tuple{float64(0)}},
		{"uuid", tuple.Tuple{tuple.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}},
		{"composite_int_int", tuple.Tuple{int64(1), int64(2)}},
		{"composite_string_int", tuple.Tuple{"order", int64(42)}},
		{"composite_three", tuple.Tuple{int64(1), "hello", int64(-5)}},
		{"nested_simple", tuple.Tuple{tuple.Tuple{int64(1), int64(2)}}},
		{"nested_in_nested", tuple.Tuple{tuple.Tuple{tuple.Tuple{int64(1)}, int64(2)}}},
		{"nested_with_string", tuple.Tuple{tuple.Tuple{"abc", int64(3)}}},
		{"mixed_nested_and_plain", tuple.Tuple{int64(0), tuple.Tuple{int64(1)}, int64(2)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			packed := tc.tuple.Pack()

			got, err := fastUnpack(packed)
			if err != nil {
				t.Fatalf("fastUnpack error: %v", err)
			}
			want, err := tuple.Unpack(packed)
			if err != nil {
				t.Fatalf("tuple.Unpack error: %v", err)
			}

			if len(got) != len(want) {
				t.Fatalf("length mismatch: fastUnpack=%d, tuple.Unpack=%d", len(got), len(want))
			}
			// Re-pack both and compare bytes for exact equivalence
			gotPacked := tuple.Tuple(got).Pack()
			wantPacked := tuple.Tuple(want).Pack()
			if string(gotPacked) != string(wantPacked) {
				t.Errorf("round-trip mismatch:\n  input:     %v\n  fastUnpack: %x\n  Unpack:     %x", tc.tuple, gotPacked, wantPacked)
			}
		})
	}
}

// TestSplitKeySuffix verifies suffix extraction from tuple-encoded keys.
func TestSplitKeySuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		pk         tuple.Tuple
		suffix     int64
		wantSuffix int64
	}{
		{"unsplit", tuple.Tuple{int64(1)}, 0, 0},
		{"version", tuple.Tuple{int64(1)}, -1, -1},
		{"split_1", tuple.Tuple{int64(1)}, 1, 1},
		{"split_2", tuple.Tuple{int64(1)}, 2, 2},
		{"composite_pk_unsplit", tuple.Tuple{int64(1), "order"}, 0, 0},
		{"composite_pk_version", tuple.Tuple{int64(1), "order"}, -1, -1},
		{"large_pk_value", tuple.Tuple{int64(math.MaxInt64)}, 0, 0},
		{"negative_pk", tuple.Tuple{int64(-42)}, 0, 0},
		{"string_pk", tuple.Tuple{"my-record"}, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			full := append(tc.pk, tc.suffix)
			packed := full.Pack()

			gotSuffix, pkEnd, err := splitKeySuffix(packed)
			if err != nil {
				t.Fatalf("splitKeySuffix error: %v", err)
			}
			if gotSuffix != tc.wantSuffix {
				t.Errorf("suffix: got %d, want %d", gotSuffix, tc.wantSuffix)
			}

			// Verify PK portion decodes correctly
			gotPK, err := fastUnpack(packed[:pkEnd])
			if err != nil {
				t.Fatalf("fastUnpack PK error: %v", err)
			}
			wantPK := tuple.Tuple(tc.pk)
			if string(tuple.Tuple(gotPK).Pack()) != string(wantPK.Pack()) {
				t.Errorf("PK mismatch: got %v, want %v", gotPK, wantPK)
			}
		})
	}
}

// TestSplitKeySuffix_EmptyReturnsError pins that splitKeySuffix returns a typed
// error — never panics — on an empty suffix. A record key is always prefix +
// PK-tuple + suffix, but a stray/malformed key under the records subspace (a foreign
// client, corruption, or a scan range that included the bare prefix) yields an empty
// suffix; pre-fix the function's `tupleBytes[lastStart]` index-panicked on it,
// crashing the main record-scan path (key_value_cursor loadNext). Don't-leak-panics:
// malformed stored data must surface as an error.
func TestSplitKeySuffix_EmptyReturnsError(t *testing.T) {
	t.Parallel()
	for _, in := range [][]byte{nil, {}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("splitKeySuffix(%v) panicked instead of erroring: %v", in, r)
				}
			}()
			if _, _, err := splitKeySuffix(in); err == nil {
				t.Fatalf("splitKeySuffix(%v): want error, got nil", in)
			}
		}()
	}
}

// TestTupleSkipNestedInNested is a targeted regression test for the bug where
// tupleSkip would stop at the inner nested tuple's 0x00 terminator instead of
// the outer one, causing the outer tuple to be measured as too short.
// TestTupleSkipNestedWithBytesPayload pins the bug where tupleSkip on a nested
// tuple stopped at the first inner element's *terminator* 0x00 (e.g. a bytes
// element's trailing 0x00) instead of parsing element-by-element. HNSW stores
// node vectors as nested {bytes} tuples whose payloads contain arbitrary 0x00 /
// 0x05 bytes, so a node value's element walk must skip such a nested tuple to
// its true end. Each case asserts tupleSkip returns the full packed length.
func TestTupleSkipNestedWithBytesPayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    tuple.Tuple
	}{
		// Vectors: bytes payloads with 0x00 (escaped as 0x00 0xFF) and 0x05.
		{"nested_bytes_nulls", tuple.Tuple{tuple.Tuple{[]byte{0x01, 0x00, 0x05, 0xff, 0x00, 0x7e, 0x05, 0x05}}}},
		{"nested_bytes_trailing_null", tuple.Tuple{tuple.Tuple{[]byte{0x10, 0x20, 0x00}}}},
		{"nested_bytes_only_null", tuple.Tuple{tuple.Tuple{[]byte{0x00}}}},
		// Int value bytes that collide with the nested type code (0x05).
		{"nested_int_0x05", tuple.Tuple{tuple.Tuple{int64(5)}}},
		{"nested_int_with_null", tuple.Tuple{tuple.Tuple{int64(256)}}}, // 0x16 0x01 0x00
		// The full node-value shape: {nodeKind, {vec}, {pk1, pk2}}.
		{"node_value_shape", tuple.Tuple{
			int64(0),
			tuple.Tuple{[]byte{0x01, 0x00, 0x05, 0xff, 0x00, 0x7e}},
			tuple.Tuple{tuple.Tuple{int64(5)}, tuple.Tuple{int64(2)}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			packed := tc.v.Pack()
			// tupleSkip on the first element must consume exactly that element.
			firstLen := len(tuple.Tuple{tc.v[0]}.Pack())
			if got := tupleSkip(packed); got != firstLen {
				t.Errorf("tupleSkip(%x) = %d, want %d (first element length)", packed, got, firstLen)
			}
			// And walking every element must consume the whole buffer.
			p := 0
			for p < len(packed) {
				n := tupleSkip(packed[p:])
				if n <= 0 {
					t.Fatalf("tupleSkip stalled at offset %d in %x", p, packed)
				}
				p += n
			}
			if p != len(packed) {
				t.Errorf("element walk consumed %d bytes, want %d (%x)", p, len(packed), packed)
			}
		})
	}
}

func TestTupleSkipNestedInNested(t *testing.T) {
	t.Parallel()
	// Pack: (nested(nested(1), 2), 99)
	// The outer nested contains an inner nested — tupleSkip must not
	// confuse the inner 0x00 terminator with the outer one.
	inner := tuple.Tuple{tuple.Tuple{tuple.Tuple{int64(1)}, int64(2)}, int64(99)}
	packed := inner.Pack()

	got, err := fastUnpack(packed)
	if err != nil {
		t.Fatalf("fastUnpack error: %v", err)
	}
	want, err := tuple.Unpack(packed)
	if err != nil {
		t.Fatalf("tuple.Unpack error: %v", err)
	}

	gotPacked := tuple.Tuple(got).Pack()
	wantPacked := tuple.Tuple(want).Pack()
	if string(gotPacked) != string(wantPacked) {
		t.Errorf("nested-in-nested mismatch:\n  fastUnpack: %x\n  Unpack:     %x", gotPacked, wantPacked)
	}

	// Also verify splitKeySuffix works when PK contains nested tuple
	withSuffix := tuple.Tuple{tuple.Tuple{int64(1), int64(2)}, int64(0)}
	packed2 := withSuffix.Pack()
	suffix, pkEnd, err := splitKeySuffix(packed2)
	if err != nil {
		t.Fatalf("splitKeySuffix error: %v", err)
	}
	if suffix != 0 {
		t.Errorf("suffix: got %d, want 0", suffix)
	}
	gotPK, err := fastUnpack(packed2[:pkEnd])
	if err != nil {
		t.Fatalf("fastUnpack PK error: %v", err)
	}
	if len(gotPK) != 1 {
		t.Errorf("PK elements: got %d, want 1", len(gotPK))
	}
}

// TestFastDecodeInt8ByteNegative is a regression test for the sizeLimits[8] bug.
// sizeLimits[8] must be -1 (matching FDB's uint64 overflow), not MaxInt64.
// Without the fix, 8-byte negative integers that go through fastDecodeInt
// (not fastDecodeBigInt) decode to wrong values.
func TestFastDecodeInt8ByteNegative(t *testing.T) {
	t.Parallel()
	// These values require 8 bytes and go through decodeInt (not decodeBigInt):
	// type code 0x0c with first data byte having MSB set.
	values := []int64{
		-(1 << 56),           // smallest 8-byte negative
		-(1 << 62),           // large 8-byte negative
		-(1<<63 - 1),         // MaxInt64 negated = MinInt64 + 1
		-72057594037927936,   // -(2^56) exact boundary
		-4611686018427387904, // -(2^62)
	}
	for _, v := range values {
		packed := tuple.Tuple{v}.Pack()
		got, err := fastUnpack(packed)
		if err != nil {
			t.Fatalf("fastUnpack(%d) error: %v", v, err)
		}
		want, _ := tuple.Unpack(packed)
		// Compare actual decoded values, not just re-packed bytes
		if len(got) != 1 || len(want) != 1 {
			t.Fatalf("length mismatch for %d: got=%d want=%d", v, len(got), len(want))
		}
		gotVal, ok1 := got[0].(int64)
		wantVal, ok2 := want[0].(int64)
		if !ok1 || !ok2 {
			t.Fatalf("type mismatch for %d: got=%T want=%T", v, got[0], want[0])
		}
		if gotVal != wantVal {
			t.Errorf("value mismatch for %d: fastUnpack=%d, tuple.Unpack=%d", v, gotVal, wantVal)
		}
	}
}

// TestFastDecodeIntEdgeCases covers integer edge cases.
func TestFastDecodeIntEdgeCases(t *testing.T) {
	t.Parallel()
	values := []int64{
		0, 1, -1, 127, -128, 255, -255,
		256, -256, 65535, -65535,
		1 << 24, -(1 << 24),
		1 << 32, -(1 << 32),
		1 << 48, -(1 << 48),
		1 << 56, -(1 << 56),
		1 << 62, -(1 << 62),
		math.MaxInt64, math.MinInt64,
	}
	for _, v := range values {
		packed := tuple.Tuple{v}.Pack()
		got, err := fastUnpack(packed)
		if err != nil {
			t.Fatalf("fastUnpack(%d) error: %v", v, err)
		}
		want, _ := tuple.Unpack(packed)
		gotPacked := tuple.Tuple(got).Pack()
		wantPacked := tuple.Tuple(want).Pack()
		if string(gotPacked) != string(wantPacked) {
			t.Errorf("int %d: fastUnpack=%x, Unpack=%x", v, gotPacked, wantPacked)
		}
	}
}

// TestTupleSkip verifies tupleSkip returns correct byte lengths for all tuple type codes.
func TestTupleSkip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		tupleV tuple.Tuple
	}{
		{"nil", tuple.Tuple{nil}},
		{"zero", tuple.Tuple{int64(0)}},
		{"small_pos", tuple.Tuple{int64(42)}},
		{"small_neg", tuple.Tuple{int64(-42)}},
		{"large_pos", tuple.Tuple{int64(1 << 40)}},
		{"large_neg", tuple.Tuple{int64(-1 << 40)}},
		{"max_int64", tuple.Tuple{int64(math.MaxInt64)}},
		{"min_int64", tuple.Tuple{int64(math.MinInt64)}},
		{"string", tuple.Tuple{"hello"}},
		{"empty_string", tuple.Tuple{""}},
		{"bytes", tuple.Tuple{[]byte{1, 2, 3}}},
		{"empty_bytes", tuple.Tuple{[]byte{}}},
		{"float32", tuple.Tuple{float32(3.14)}},
		{"float64", tuple.Tuple{float64(2.718)}},
		{"bool_true", tuple.Tuple{true}},
		{"bool_false", tuple.Tuple{false}},
		{"uuid", tuple.Tuple{tuple.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}},
		{"versionstamp", tuple.Tuple{tuple.Versionstamp{
			TransactionVersion: [10]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
			UserVersion:        42,
		}}},
		// Nested tuples tested separately in TestTupleSkipNestedInNested
		// because Pack escaping of inner 0x00 bytes affects size calculation.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			packed := tc.tupleV.Pack()
			size := tupleSkip(packed)
			if size != len(packed) {
				t.Errorf("tupleSkip(%x) = %d, want %d (full packed length)", packed, size, len(packed))
			}
		})
	}

	// Multi-element: tupleSkip should return just the first element's size.
	t.Run("multi_element_first_only", func(t *testing.T) {
		t.Parallel()
		packed := tuple.Tuple{int64(42), "hello"}.Pack()
		firstPacked := tuple.Tuple{int64(42)}.Pack()
		size := tupleSkip(packed)
		if size != len(firstPacked) {
			t.Errorf("tupleSkip on multi-element: got %d, want %d (first element)", size, len(firstPacked))
		}
	})

	// Truncated input should return -1.
	t.Run("truncated_empty", func(t *testing.T) {
		t.Parallel()
		if tupleSkip(nil) != -1 {
			t.Error("tupleSkip(nil) should return -1")
		}
		if tupleSkip([]byte{}) != -1 {
			t.Error("tupleSkip({}) should return -1")
		}
	})

	t.Run("unknown_type_code", func(t *testing.T) {
		t.Parallel()
		// 0x04 is not a valid type code
		if tupleSkip([]byte{0x04}) != -1 {
			t.Error("tupleSkip(0x04) should return -1 for unknown type code")
		}
	})
}

// TestSplitKeySuffixEdgeCases verifies edge cases for the zero-alloc key suffix splitter.
func TestSplitKeySuffixEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("simple_pk_with_int_suffix", func(t *testing.T) {
		t.Parallel()
		// pk=(42), suffix=0
		packed := tuple.Tuple{int64(42), int64(0)}.Pack()
		suffix, pkEnd, err := splitKeySuffix(packed)
		if err != nil {
			t.Fatal(err)
		}
		if suffix != 0 {
			t.Errorf("suffix: got %d, want 0", suffix)
		}
		// pkEnd should point to where the last element starts
		firstPacked := tuple.Tuple{int64(42)}.Pack()
		if pkEnd != len(firstPacked) {
			t.Errorf("pkEnd: got %d, want %d", pkEnd, len(firstPacked))
		}
	})

	t.Run("string_pk_with_negative_suffix", func(t *testing.T) {
		t.Parallel()
		packed := tuple.Tuple{"order", int64(-1)}.Pack()
		suffix, pkEnd, err := splitKeySuffix(packed)
		if err != nil {
			t.Fatal(err)
		}
		if suffix != -1 {
			t.Errorf("suffix: got %d, want -1", suffix)
		}
		firstPacked := tuple.Tuple{"order"}.Pack()
		if pkEnd != len(firstPacked) {
			t.Errorf("pkEnd: got %d, want %d", pkEnd, len(firstPacked))
		}
	})

	t.Run("non_integer_suffix_errors", func(t *testing.T) {
		t.Parallel()
		// Suffix is a string, not an integer
		packed := tuple.Tuple{int64(1), "not_an_int"}.Pack()
		_, _, err := splitKeySuffix(packed)
		if err == nil {
			t.Error("expected error for non-integer suffix")
		}
	})

	t.Run("single_integer", func(t *testing.T) {
		t.Parallel()
		packed := tuple.Tuple{int64(5)}.Pack()
		suffix, pkEnd, err := splitKeySuffix(packed)
		if err != nil {
			t.Fatal(err)
		}
		if suffix != 5 {
			t.Errorf("suffix: got %d, want 5", suffix)
		}
		if pkEnd != 0 {
			t.Errorf("pkEnd: got %d, want 0", pkEnd)
		}
	})
}
