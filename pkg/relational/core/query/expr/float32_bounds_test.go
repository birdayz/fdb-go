package expr

import (
	"math"
	"testing"
)

// TestFloat32NextUpDown pins the sortable-key next/prev transform against
// known adjacent float32 pairs, including the sign-crossing cases a naive
// Float32bits(x)±1 gets backwards (raw IEEE-754 bit patterns order
// increasing-magnitude-away-from-zero for negatives, which is the WRONG
// direction for "next value up" once the sign bit is involved).
func TestFloat32NextUpDown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		x          float32
		wantNextUp float32
	}{
		{"one_to_next", 1.0, math.Float32frombits(math.Float32bits(1.0) + 1)},
		{"zero_to_smallest_positive_subnormal", 0, math.Float32frombits(1)},
		// -0 and +0 are the same real number: NextUp must agree regardless
		// of which zero's bit pattern is asked, not treat them as distinct
		// adjacent code points (see float32NextUp's ±0 canonicalization).
		{"negative_zero_to_smallest_positive_subnormal", float32(math.Copysign(0, -1)), math.Float32frombits(1)},
		{"minus_one_to_next", -1.0, math.Float32frombits(math.Float32bits(-1.0) - 1)},
		// Crossing zero from the negative side: the value just below zero's
		// successor must be 0 exactly (not -0, and not some huge negative
		// bit-pattern artifact from blind ±1 on raw bits).
		{"smallest_negative_subnormal_to_zero", math.Float32frombits(0x80000001), float32(math.Copysign(0, -1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := float32NextUp(tc.x)
			if got != tc.wantNextUp {
				t.Errorf("float32NextUp(%v) = %v (bits %#x), want %v (bits %#x)",
					tc.x, got, math.Float32bits(got), tc.wantNextUp, math.Float32bits(tc.wantNextUp))
			}
			// NextDown must invert NextUp exactly (adjacent representable values).
			back := float32NextDown(got)
			if back != tc.x && !(back == 0 && tc.x == 0) { // ±0 both compare == 0
				t.Errorf("float32NextDown(float32NextUp(%v)) = %v, want %v", tc.x, back, tc.x)
			}
		})
	}
}

// TestFloat32CeilFloor proves the ceil/floor bracketing property directly:
// float64(Ceil(f)) >= f, float64(Floor(f)) <= f, and — the property that
// actually matters for SARG correctness — there is NO float32 strictly
// between Floor(f) and Ceil(f) other than themselves (so the rewritten
// `col >= Ceil(f)` / `col <= Floor(f)` bound is the TIGHTEST possible, not
// merely a safe-but-loose one).
func TestFloat32CeilFloor(t *testing.T) {
	t.Parallel()
	// Values NOT exactly representable in float32 (24-bit mantissa): an
	// integer beyond 2^24, and typical non-terminating-binary fractions.
	notExact := []float64{
		1.1, 6.5, -6.5, 0.1, 1e20, -1e20,
		16777217, // 2^24 + 1 — first integer float32 cannot represent exactly
		-16777217,
		1e40,  // overflows float32 range entirely (rounds to +Inf)
		-1e40, // overflows to -Inf
	}
	for _, f := range notExact {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			ceil := float32Ceil(f)
			floor := float32Floor(f)
			if float64(ceil) < f {
				t.Errorf("float32Ceil(%v) = %v, want >= %v", f, ceil, f)
			}
			if float64(floor) > f {
				t.Errorf("float32Floor(%v) = %v, want <= %v", f, floor, f)
			}
			// Floor must be the value immediately below Ceil (no float32 in between)
			// whenever the two differ, i.e. f itself is not exactly representable.
			if ceil != floor {
				if float32NextDown(ceil) != floor {
					t.Errorf("float32Floor(%v)=%v is not the immediate predecessor of float32Ceil(%v)=%v", f, floor, f, ceil)
				}
			}
		})
	}
	// Values that ARE exactly representable — Ceil and Floor must both
	// return the exact value, unchanged, and be equal to each other.
	exact := []float64{0, 1, -1, 1.5, -1.5, 2.5, 16777216, -16777216, 100}
	for _, f := range exact {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			ceil := float32Ceil(f)
			floor := float32Floor(f)
			if float64(ceil) != f {
				t.Errorf("float32Ceil(%v) = %v, want exactly %v", f, ceil, f)
			}
			if float64(floor) != f {
				t.Errorf("float32Floor(%v) = %v, want exactly %v", f, floor, f)
			}
		})
	}
}

// TestFloat32CeilFloorOverflow pins the beyond-float32-range direction:
// Ceil of a value larger than any finite float32 is +Inf (only +Inf is
// "greater than or equal" to it in the float32 domain); Floor of the same
// value is float32's largest finite value (the greatest float32 that does
// NOT overflow past it). Mirrored for the negative side.
func TestFloat32CeilFloorOverflow(t *testing.T) {
	t.Parallel()
	huge := 1e40
	if got := float32Ceil(huge); got != float32(math.Inf(1)) {
		t.Errorf("float32Ceil(1e40) = %v, want +Inf", got)
	}
	if got := float32Floor(huge); got != math.MaxFloat32 {
		t.Errorf("float32Floor(1e40) = %v, want %v (float32 max)", got, math.MaxFloat32)
	}
	negHuge := -1e40
	if got := float32Floor(negHuge); got != float32(math.Inf(-1)) {
		t.Errorf("float32Floor(-1e40) = %v, want -Inf", got)
	}
	if got := float32Ceil(negHuge); got != -math.MaxFloat32 {
		t.Errorf("float32Ceil(-1e40) = %v, want %v (float32 -max)", got, -float32(math.MaxFloat32))
	}
}
