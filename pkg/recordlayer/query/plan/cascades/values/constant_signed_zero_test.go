package values

import (
	"math"
	"testing"
)

// TestConstantValue_SignedZeroEqualsIsHashConsistent pins RFC-189 A2 (finding
// 5): scalar ConstantValue equality used Go's `==`, which calls -0.0 == +0.0
// true, while writeSemanticHash's %v renders "-0" vs "0" — an
// equal-but-different-hash memo violation (a duplicate/miss on interning). The
// fix compares scalar floats bitwise (math.FloatNNbits), matching Java's
// Double.equals(doubleToLongBits): signed zeros are UNEQUAL, so the memo
// invariant "equal ⟹ same hash" is restored, and identical-bit NaNs stay equal
// and hash-coherent.
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
		// bitwise reports them unequal.
		if EqualsWithoutChildren(negZero, posZero) {
			t.Fatal("-0.0 and +0.0 double constants must be UNEQUAL (bitwise, Java Double.equals parity)")
		}
		requireHashInvariant(t, negZero, posZero)
	})

	t.Run("float32_signed_zero_unequal", func(t *testing.T) {
		t.Parallel()
		negZero := &ConstantValue{Value: float32(math.Copysign(0, -1)), Typ: NullableFloat}
		posZero := &ConstantValue{Value: float32(0.0), Typ: NullableFloat}
		if EqualsWithoutChildren(negZero, posZero) {
			t.Fatal("-0.0 and +0.0 float32 constants must be UNEQUAL (bitwise)")
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

	t.Run("identical_bit_nan_equal_and_hash_coherent", func(t *testing.T) {
		t.Parallel()
		// math.NaN() yields a fixed bit pattern, so these are identical-bit NaNs.
		a := &ConstantValue{Value: math.NaN(), Typ: NullableDouble}
		b := &ConstantValue{Value: math.NaN(), Typ: NullableDouble}
		if !EqualsWithoutChildren(a, b) {
			t.Fatal("identical-bit NaN constants must be equal")
		}
		if SemanticHashCode(a) != SemanticHashCode(b) {
			t.Fatal("equal (identical-bit) NaN constants must hash equal")
		}
	})
}
