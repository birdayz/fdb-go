package embedded

import (
	"database/sql/driver"
	"testing"
)

// TestValuesComparable pins the type-compatibility check used by
// evalComparisonPredicateTri's `22000 cannot compare T1 with T2`
// rejection path. The contract is symmetric (comparable(a,b) iff
// comparable(b,a)) and total: numeric × numeric is always OK,
// same-concrete-type is OK, everything else is not OK.
func TestValuesComparable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b driver.Value
		want bool
	}{
		// Numeric × numeric: all combinations of int64 / float64.
		{"int64+int64", int64(1), int64(2), true},
		{"int64+float64", int64(1), float64(2), true},
		{"float64+int64", float64(1), int64(2), true},
		{"float64+float64", float64(1), float64(2), true},
		// Same concrete type (non-numeric).
		{"string+string", "a", "b", true},
		{"bool+bool", true, false, true},
		{"bytes+bytes", []byte{1}, []byte{2}, true},
		// Cross-type non-numeric: rejected.
		{"string+bool", "a", true, false},
		{"string+bytes", "a", []byte{1}, false},
		{"bool+bytes", true, []byte{1}, false},
		// Numeric × non-numeric: rejected (the SQL 22000 path).
		{"int64+string", int64(1), "a", false},
		{"float64+bool", float64(1), true, false},
		{"int64+bytes", int64(1), []byte{1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := valuesComparable(tc.a, tc.b); got != tc.want {
				t.Errorf("valuesComparable(%T, %T) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Symmetry: order of operands must not matter.
			if got := valuesComparable(tc.b, tc.a); got != tc.want {
				t.Errorf("valuesComparable(%T, %T) [reversed] = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestNullSafeEqual pins the IS NOT DISTINCT FROM truth table:
// two NULLs equal, NULL and non-NULL never equal, two non-NULLs
// fall through to type-strict valuesEqual.
func TestNullSafeEqual(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b driver.Value
		want bool
	}{
		{"nil+nil", nil, nil, true},
		{"nil+int", nil, int64(1), false},
		{"int+nil", int64(1), nil, false},
		{"int+int equal", int64(1), int64(1), true},
		{"int+int unequal", int64(1), int64(2), false},
		{"nil+string", nil, "a", false},
		{"string+string equal", "a", "a", true},
		{"string+string unequal", "a", "b", false},
		// Cross-type non-NULL: equality is false (mirrors `=` rejection).
		{"int+string", int64(1), "1", false},
		// Numeric promotion: int64(1) and float64(1.0) compare equal.
		{"int+float same", int64(1), float64(1), true},
		{"int+float diff", int64(1), float64(2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nullSafeEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("nullSafeEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
