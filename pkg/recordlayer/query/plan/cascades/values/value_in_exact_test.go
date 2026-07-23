package values

import "testing"

// TestEqualsAny_ExactIntAbove2Pow53 pins RFC-189 C1 (finding 12a): the
// value-layer IN promoted every numeric pair to float64, so two distinct int64
// above 2^53 compared EQUAL (their float64 images tie). It now compares
// same-family integers exactly (CompareExactInts), matching the predicate-layer
// IN; cross-type int/float still coerces (Java parity).
func TestEqualsAny_ExactIntAbove2Pow53(t *testing.T) {
	t.Parallel()
	if equalsAny(int64(9007199254740993), int64(9007199254740992)) {
		t.Fatal("distinct int64 above 2^53 must NOT be equal (float64 promotion would tie them)")
	}
	if !equalsAny(int64(9007199254740993), int64(9007199254740993)) {
		t.Fatal("identical int64 must be equal")
	}
	// Cross-type int/float still coerces to a common numeric type (Java parity).
	if !equalsAny(int64(5), float64(5.0)) {
		t.Fatal("int64(5) and float64(5.0) must compare equal (cross-type coercion)")
	}
	if equalsAny(int64(5), float64(5.5)) {
		t.Fatal("int64(5) and float64(5.5) must be unequal")
	}
}

// TestEqualsAny_NonComparableNoPanic pins that non-comparable dynamic types
// ([]any) no longer panic under `==` (finding 12a): they compare structurally.
func TestEqualsAny_NonComparableNoPanic(t *testing.T) {
	t.Parallel()
	a := []any{int64(1), int64(2)}
	b := []any{int64(1), int64(2)}
	c := []any{int64(9)}
	if !equalsAny(a, b) { // pre-fix: `a == b` on []any PANICS
		t.Fatal("structurally equal []any must be equal (DeepEqual, no panic)")
	}
	if equalsAny(a, c) {
		t.Fatal("different []any must be unequal")
	}
}

// TestArrayContainsByValue_NonComparableNoPanic pins the array_distinct panic
// guard (finding 12a): a nested-array element no longer panics under `==`.
func TestArrayContainsByValue_NonComparableNoPanic(t *testing.T) {
	t.Parallel()
	arr := []any{[]any{int64(1)}, []any{int64(2)}}
	if !arrayContainsByValue(arr, []any{int64(1)}) { // pre-fix: `e == target` PANICS
		t.Fatal("present nested array must be found (no panic)")
	}
	if arrayContainsByValue(arr, []any{int64(3)}) {
		t.Fatal("absent nested array must not be found")
	}
}
