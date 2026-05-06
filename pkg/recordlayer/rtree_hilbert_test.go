package recordlayer

import (
	"math"
	"math/big"
	"testing"
)

// --- shiftCoordinate ---

func TestShiftCoordinateDocumentedMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input int64
		want  uint64
	}{
		{"MinInt64 -> 0", math.MinInt64, 0},
		{"-1 -> MaxInt64", -1, math.MaxInt64},
		{"0 -> 1<<63", 0, 1 << 63},
		{"MaxInt64 -> MaxUint64", math.MaxInt64, math.MaxUint64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shiftCoordinate(tt.input)
			if got != tt.want {
				t.Errorf("shiftCoordinate(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestShiftCoordinateMonotonic(t *testing.T) {
	t.Parallel()
	// shiftCoordinate must be strictly monotonically increasing.
	vals := []int64{
		math.MinInt64, math.MinInt64 + 1,
		-1000, -1, 0, 1, 1000,
		math.MaxInt64 - 1, math.MaxInt64,
	}
	for i := 1; i < len(vals); i++ {
		prev := shiftCoordinate(vals[i-1])
		curr := shiftCoordinate(vals[i])
		if curr <= prev {
			t.Errorf("shiftCoordinate not monotonic: f(%d)=%d >= f(%d)=%d",
				vals[i-1], prev, vals[i], curr)
		}
	}
}

func TestShiftCoordinateNegativeBoundary(t *testing.T) {
	t.Parallel()
	// -1 and 0 are adjacent in the shifted space.
	neg := shiftCoordinate(-1)
	zero := shiftCoordinate(0)
	if zero != neg+1 {
		t.Errorf("shiftCoordinate(0) - shiftCoordinate(-1) = %d, want 1", zero-neg)
	}
}

// --- transposedIndex ---

func TestTransposedIndexDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	coords := []uint64{42, 99}
	orig0, orig1 := coords[0], coords[1]
	_ = transposedIndex(64, coords)
	if coords[0] != orig0 || coords[1] != orig1 {
		t.Error("transposedIndex mutated input slice")
	}
}

func TestTransposedIndexSingleDimension(t *testing.T) {
	t.Parallel()
	// transposedIndex is deterministic and produces distinct outputs for distinct inputs.
	seen := make(map[uint64]uint64)
	for _, v := range []uint64{0, 1, 2, 255, 1 << 63, math.MaxUint64} {
		result := transposedIndex(64, []uint64{v})
		if len(result) != 1 {
			t.Fatalf("transposedIndex(64, [%d]): got %d elements, want 1", v, len(result))
		}
		if prev, ok := seen[result[0]]; ok {
			t.Errorf("collision: transposedIndex(%d) = transposedIndex(%d) = %d", v, prev, result[0])
		}
		seen[result[0]] = v
	}
	// Zero input maps to zero output.
	if r := transposedIndex(64, []uint64{0}); r[0] != 0 {
		t.Errorf("transposedIndex(64, [0]) = [%d], want [0]", r[0])
	}
}

// --- toIndex ---

func TestToIndexZero(t *testing.T) {
	t.Parallel()
	got := toIndex(64, []uint64{0, 0}, 2)
	if got.Sign() != 0 {
		t.Errorf("toIndex(zeros) = %v, want 0", got)
	}
}

func TestToIndexSingleDimension(t *testing.T) {
	t.Parallel()
	// With 1 dimension, toIndex should just reconstruct the value from bits.
	for _, v := range []uint64{0, 1, 42, math.MaxUint64} {
		transposed := []uint64{v}
		got := toIndex(64, transposed, 1)
		want := new(big.Int).SetUint64(v)
		if got.Cmp(want) != 0 {
			t.Errorf("toIndex(64, [%d], 1) = %v, want %v", v, got, want)
		}
	}
}

func TestToIndexBitInterleaving(t *testing.T) {
	t.Parallel()
	// 2 dimensions, 4 bits each, d0=0b1010, d1=0b0101.
	// Interleaved (MSB first, alternating d0,d1):
	// d0_3=1, d1_3=0, d0_2=0, d1_2=1, d0_1=1, d1_1=0, d0_0=0, d1_0=1
	// = 0b10011001 = 153
	got := toIndex(4, []uint64{0b1010, 0b0101}, 2)
	want := big.NewInt(153)
	if got.Cmp(want) != 0 {
		t.Errorf("toIndex interleave = %v, want %v", got, want)
	}
}

// --- hilbertValue ---

func TestHilbertValueEmptyCoords(t *testing.T) {
	t.Parallel()
	got := hilbertValue(nil)
	if got.Sign() != 0 {
		t.Errorf("hilbertValue(nil) = %v, want 0", got)
	}
	got2 := hilbertValue([]int64{})
	if got2.Sign() != 0 {
		t.Errorf("hilbertValue([]) = %v, want 0", got2)
	}
}

func TestHilbertValueSingleDimensionMonotonic(t *testing.T) {
	t.Parallel()
	// In 1D, shift+Gray preserves total order.
	vals := []int64{math.MinInt64, -1000, -1, 0, 1, 1000, math.MaxInt64}
	prev := hilbertValue([]int64{vals[0]})
	for i := 1; i < len(vals); i++ {
		curr := hilbertValue([]int64{vals[i]})
		if curr.Cmp(prev) <= 0 {
			t.Errorf("1D not monotonic: hilbert(%d)=%v >= hilbert(%d)=%v",
				vals[i-1], prev, vals[i], curr)
		}
		prev = curr
	}
}

func TestHilbertValueTwoDimensionsLocality(t *testing.T) {
	t.Parallel()
	// (0,0) should produce a smaller Hilbert value than (1,0).
	h00 := hilbertValue([]int64{0, 0})
	h10 := hilbertValue([]int64{1, 0})
	if h00.Cmp(h10) >= 0 {
		t.Errorf("locality violated: hilbert([0,0])=%v >= hilbert([1,0])=%v", h00, h10)
	}
}

func TestHilbertValueDifferentInputsDifferentOutputs(t *testing.T) {
	t.Parallel()
	points := [][]int64{
		{0, 0},
		{1, 0},
		{0, 1},
		{1, 1},
		{-1, -1},
		{100, 200},
	}
	seen := make(map[string][]int64)
	for _, p := range points {
		h := hilbertValue(p)
		key := h.String()
		if dup, ok := seen[key]; ok {
			t.Errorf("hilbertValue(%v) == hilbertValue(%v) = %s", dup, p, key)
		}
		seen[key] = p
	}
}

func TestHilbertValueOriginIsNotZero(t *testing.T) {
	t.Parallel()
	// (0,0) is in the middle of shifted space, not zero.
	h := hilbertValue([]int64{0, 0})
	if h.Sign() == 0 {
		t.Error("hilbertValue([0,0]) should not be zero (origin is mid-space)")
	}
}

func TestHilbertValueMinInt64AllDims(t *testing.T) {
	t.Parallel()
	// All-MinInt64 maps to all-zero in shifted space.
	// transposedIndex of all zeros is all zeros, toIndex of all zeros is zero.
	h := hilbertValue([]int64{math.MinInt64, math.MinInt64})
	if h.Sign() != 0 {
		t.Errorf("hilbertValue([MinInt64, MinInt64]) = %v, want 0", h)
	}
}

func TestHilbertValueMaxInt64AllDims(t *testing.T) {
	t.Parallel()
	// All-MaxInt64 should be near the top of the Hilbert range.
	h := hilbertValue([]int64{math.MaxInt64, math.MaxInt64})
	if h.Sign() == 0 {
		t.Error("hilbertValue([MaxInt64, MaxInt64]) should not be zero")
	}
	// With 64 bits * 2 dims = 128-bit result space, value should be large.
	if h.BitLen() < 64 {
		t.Errorf("hilbertValue([MaxInt64, MaxInt64]) has only %d bits, expected many more", h.BitLen())
	}
}

func TestHilbertValueThreeDimensions(t *testing.T) {
	t.Parallel()
	// Smoke test: 3D should produce distinct values.
	h1 := hilbertValue([]int64{0, 0, 0})
	h2 := hilbertValue([]int64{1, 0, 0})
	h3 := hilbertValue([]int64{0, 1, 0})
	h4 := hilbertValue([]int64{0, 0, 1})
	vals := []*big.Int{h1, h2, h3, h4}
	for i := 0; i < len(vals); i++ {
		for j := i + 1; j < len(vals); j++ {
			if vals[i].Cmp(vals[j]) == 0 {
				t.Errorf("3D collision: hilbert(point %d) == hilbert(point %d) = %v", i, j, vals[i])
			}
		}
	}
}

func TestHilbertValueSymmetricInputsDiffer(t *testing.T) {
	t.Parallel()
	// (a,b) != (b,a) in general for Hilbert curves.
	h1 := hilbertValue([]int64{3, 7})
	h2 := hilbertValue([]int64{7, 3})
	if h1.Cmp(h2) == 0 {
		t.Errorf("hilbertValue([3,7]) == hilbertValue([7,3]) = %v", h1)
	}
}

// --- Benchmarks ---

func BenchmarkHilbertValue2D(b *testing.B) {
	coords := []int64{42, -17}
	for b.Loop() {
		hilbertValue(coords)
	}
}

func BenchmarkHilbertValue4D(b *testing.B) {
	coords := []int64{42, -17, 1000, -999}
	for b.Loop() {
		hilbertValue(coords)
	}
}

func BenchmarkShiftCoordinate(b *testing.B) {
	for b.Loop() {
		shiftCoordinate(42)
	}
}

func BenchmarkTransposedIndex2D(b *testing.B) {
	coords := []uint64{1 << 63, 1<<63 + 42}
	for b.Loop() {
		transposedIndex(64, coords)
	}
}

func BenchmarkToIndex2D(b *testing.B) {
	transposed := []uint64{123456789, 987654321}
	for b.Loop() {
		toIndex(64, transposed, 2)
	}
}
