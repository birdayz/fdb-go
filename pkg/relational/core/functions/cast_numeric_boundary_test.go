package functions

import (
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestCastNumericBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input         any
		integer, long int64
	}{
		{float64(0.49999999999999994), 0, 0},
		{float64(-0.5000000000000001), -1, -1},
		{float64(-0.5), 0, 0},
		{float64(4503599627370497), 1, 4503599627370497},
		{float64(2147483647.5), -2147483648, 2147483648},
		{float64(-2147483648.6), 2147483647, -2147483649},
		{float64(1e20), -1, 9223372036854775807},
		{float64(-1e20), 0, -9223372036854775808},
		{float64(9223372036854774784), -1024, 9223372036854774784},
		{float32(1e20), 2147483647, 2147483647},
		{float32(-1e20), -2147483648, -2147483648},
		{float32(0.49999999999999994), 1, 1},
		{math.Copysign(0, -1), 0, 0},
	} {
		for _, target := range []string{"INTEGER", "INT", "BIGINT", "LONG"} {
			want := tc.long
			if target == "INTEGER" || target == "INT" {
				want = tc.integer
			}
			got, err := CastValue(tc.input, target)
			if err != nil || got != want {
				t.Errorf("%T(%v)→%s: %T(%v)/%v, want int64(%d)", tc.input, tc.input, target, got, got, err, want)
			}
		}
	}
	for _, input := range []any{math.NaN(), math.Inf(1), math.Inf(-1), float32(math.NaN()), float32(math.Inf(1))} {
		for _, target := range []string{"INTEGER", "BIGINT"} {
			got, err := CastValue(input, target)
			var invalid *api.Error
			if got != nil || !errors.As(err, &invalid) || invalid.Code != api.ErrCodeInvalidCast {
				t.Errorf("nonfinite %T→%s: %v/%v", input, target, got, err)
			}
		}
	}
}
