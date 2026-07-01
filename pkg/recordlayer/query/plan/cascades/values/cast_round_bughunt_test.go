package values

// Regression: CAST(double AS INT/BIGINT) must round per java.lang.Math.round
// (Java 7+): ties toward +infinity, with the floating-point boundary corrected
// so the largest double below 0.5 rounds to 0, not 1 (pre-Java-7 floor(x+0.5)
// bug, JDK-6430675).

import "testing"

func TestBugHunt_CastDoubleToIntJavaRounding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want int64
	}{
		{0.49999999999999994, 0}, // largest double < 0.5 — pre-Java-7 floor(x+0.5) gave 1
		{0.5, 1},
		{2.5, 3},
		{-0.5, 0},  // Java rounds half toward +inf (Go math.Round gives -1)
		{-2.5, -2}, // half toward +inf
		{3.9, 4},
		{-3.9, -4},
		{2.4, 2},
		{-2.6, -3},
		{9007199254740994.0, 9007199254740994},   // |a| >= 2^52: already integral (shift<0 branch)
		{-9007199254740994.0, -9007199254740994}, // negative integral
	}
	for _, tc := range cases {
		cv := NewCastValue(&ConstantValue{Value: tc.in, Typ: TypeFloat}, TypeInt)
		got, err := cv.Evaluate(nil)
		if err != nil {
			t.Fatalf("cast %v: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("CAST(%v AS INT) = %v, want %d", tc.in, got, tc.want)
		}
	}
}
