package values

import (
	"math"
	"testing"
)

// TestConstantValue_SignedZeroEqualsIsHashConsistent pins RFC-189 A2 (finding
// 5): scalar ConstantValue equality used Go's `==`, which calls -0.0 == +0.0
// true, while writeSemanticHash's %v renders "-0" vs "0" — an
// equal-but-different-hash memo violation (a duplicate/miss on interning). The
// fix compares scalar floats by Java's canonicalized bit identity
// (Float.floatToIntBits / Double.doubleToLongBits): signed zeros are
// UNEQUAL, restoring the memo invariant "equal ⟹ same hash". Java canonicalizes
// every NaN encoding, so distinct-payload NaNs also stay equal and hash-coherent.
func TestConstantValue_SignedZeroEqualsIsHashConsistent(t *testing.T) {
	t.Parallel()

	// The invariant every memo relies on: equal ⟹ same semantic hash.
	requireHashInvariant := func(t *testing.T, a, b *ConstantValue) {
		t.Helper()
		if EqualsWithoutChildren(a, b) && SemanticHashCode(a) != SemanticHashCode(b) {
			t.Fatalf("memo violation: %v == %v but hashes differ (%d vs %d)",
				a.Value, b.Value, SemanticHashCode(a), SemanticHashCode(b))
		}
	}

	t.Run("double_signed_zero_unequal", func(t *testing.T) {
		t.Parallel()
		negZero := &ConstantValue{Value: math.Copysign(0, -1), Typ: NullableDouble}
		posZero := &ConstantValue{Value: 0.0, Typ: NullableDouble}
		// Sharp RED→GREEN pin: `==` reported these equal (with different hashes);
		// canonical bit identity reports them unequal.
		if EqualsWithoutChildren(negZero, posZero) {
			t.Fatal("-0.0 and +0.0 double constants must be UNEQUAL (Java Double.equals parity)")
		}
		if SemanticHashCode(negZero) == SemanticHashCode(posZero) {
			t.Fatal("-0.0 and +0.0 double constants should occupy distinct semantic-hash buckets")
		}
		requireHashInvariant(t, negZero, posZero)
	})

	t.Run("float32_signed_zero_unequal", func(t *testing.T) {
		t.Parallel()
		negZero := &ConstantValue{Value: float32(math.Copysign(0, -1)), Typ: NullableFloat}
		posZero := &ConstantValue{Value: float32(0.0), Typ: NullableFloat}
		if EqualsWithoutChildren(negZero, posZero) {
			t.Fatal("-0.0 and +0.0 float32 constants must be UNEQUAL (Java Float.equals parity)")
		}
		if SemanticHashCode(negZero) == SemanticHashCode(posZero) {
			t.Fatal("-0.0 and +0.0 float32 constants should occupy distinct semantic-hash buckets")
		}
		requireHashInvariant(t, negZero, posZero)
	})

	t.Run("plus_zero_equal_and_hash_coherent", func(t *testing.T) {
		t.Parallel()
		a := &ConstantValue{Value: 0.0, Typ: NullableDouble}
		b := &ConstantValue{Value: 0.0, Typ: NullableDouble}
		if !EqualsWithoutChildren(a, b) {
			t.Fatal("two +0.0 constants must be equal")
		}
		if SemanticHashCode(a) != SemanticHashCode(b) {
			t.Fatal("equal +0.0 constants must hash equal")
		}
	})

	t.Run("double_nan_payloads_canonical_and_hash_coherent", func(t *testing.T) {
		t.Parallel()
		a := &ConstantValue{Value: math.Float64frombits(0x7ff8000000000001), Typ: NullableDouble}
		b := &ConstantValue{Value: math.Float64frombits(0xfff8000000000002), Typ: NullableDouble}
		if !EqualsWithoutChildren(a, b) {
			t.Fatal("all double NaN encodings must compare equal (Java Double.doubleToLongBits parity)")
		}
		if SemanticHashCode(a) != SemanticHashCode(b) {
			t.Fatal("equal double NaN constants must hash equal")
		}
	})

	t.Run("float32_nan_payloads_canonical_and_hash_coherent", func(t *testing.T) {
		t.Parallel()
		a := &ConstantValue{Value: math.Float32frombits(0x7fc00001), Typ: NullableFloat}
		b := &ConstantValue{Value: math.Float32frombits(0xffc00002), Typ: NullableFloat}
		if !EqualsWithoutChildren(a, b) {
			t.Fatal("all float32 NaN encodings must compare equal (Java Float.floatToIntBits parity)")
		}
		if SemanticHashCode(a) != SemanticHashCode(b) {
			t.Fatal("equal float32 NaN constants must hash equal")
		}
	})
}
