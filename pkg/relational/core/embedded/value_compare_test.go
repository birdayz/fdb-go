package embedded

import (
	"math"
	"testing"

	"fdb.dev/pkg/relational/core/functions"
)

func TestIsTruthy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"bool true", true, true},
		{"bool false", false, false},
		{"int64 0", int64(0), false},
		{"int64 1", int64(1), true},
		{"int64 -1", int64(-1), true},
		{"float64 0.0", float64(0), false},
		{"float64 1.5", float64(1.5), true},
		{"float64 -0.0 == 0", math.Copysign(0, -1), false},
		{"float64 NaN is truthy", math.NaN(), true}, // NaN != 0 is true in Go
		{"empty string", "", false},
		{"non-empty string", "x", true},
		{"any other type defaults to truthy", []byte{}, true}, // []byte is not in the type-switch
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := functions.IsTruthy(tc.v); got != tc.want {
				t.Errorf("IsTruthy(%v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}
