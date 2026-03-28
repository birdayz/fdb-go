package recordlayer

import (
	"math"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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

// TestTupleSkipNestedInNested is a targeted regression test for the bug where
// tupleSkip would stop at the inner nested tuple's 0x00 terminator instead of
// the outer one, causing the outer tuple to be measured as too short.
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
