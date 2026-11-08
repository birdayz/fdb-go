package partition

import (
	"errors"
	"math"
	"testing"
)

func TestCoordinate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ value, width, want int64 }{
		{-11, 10, -2},
		{-10, 10, -1},
		{-1, 10, -1},
		{0, 10, 0},
		{9, 10, 0},
		{10, 10, 1},
		{math.MinInt64, 1, math.MinInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MinInt64, 10, -922337203685477581},
		{math.MaxInt64, 10, 922337203685477580},
		{math.MinInt64, math.MaxInt64, -2},
	} {
		got, err := Coordinate(tc.value, tc.width)
		if err != nil || got != tc.want {
			t.Fatalf("Coordinate(%d,%d) = %d,%v; want %d", tc.value, tc.width, got, err, tc.want)
		}
	}
	for _, width := range []int64{0, -1, math.MinInt64} {
		_, err := Coordinate(0, width)
		var typed *RoutingError
		if !errors.As(err, &typed) || typed.Width != width {
			t.Fatalf("invalid width %d: %v", width, err)
		}
	}
	for _, value := range []any{nil, int32(1), float64(1), "1"} {
		_, err := Route(value, 10)
		var typed *RoutingError
		if !errors.As(err, &typed) {
			t.Fatalf("invalid source %T: %v", value, err)
		}
	}
	if got, err := Route(int64(-1), 10); err != nil || got != -1 {
		t.Fatalf("Route = %d, %v", got, err)
	}
}
