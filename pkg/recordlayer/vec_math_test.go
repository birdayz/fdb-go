package recordlayer

import (
	"math"
	"testing"
)

func TestDotIdenticalVectors(t *testing.T) {
	t.Parallel()
	a := []float64{1, 2, 3}
	got := dot(a, a)
	want := 14.0 // 1+4+9
	if got != want {
		t.Errorf("dot(a, a) = %v, want %v", got, want)
	}
}

func TestDotOrthogonalVectors(t *testing.T) {
	t.Parallel()
	a := []float64{1, 0, 0}
	b := []float64{0, 1, 0}
	got := dot(a, b)
	if got != 0 {
		t.Errorf("dot(orthogonal) = %v, want 0", got)
	}
}

func TestDotMismatchedLengths(t *testing.T) {
	t.Parallel()
	// Shorter b: only first 2 elements used.
	a := []float64{1, 2, 3, 4}
	b := []float64{5, 6}
	got := dot(a, b)
	want := 17.0 // 1*5 + 2*6
	if got != want {
		t.Errorf("dot(len4, len2) = %v, want %v", got, want)
	}

	// Shorter a.
	got2 := dot(b, a)
	if got2 != want {
		t.Errorf("dot(len2, len4) = %v, want %v", got2, want)
	}
}

func TestDotEmpty(t *testing.T) {
	t.Parallel()
	if got := dot(nil, nil); got != 0 {
		t.Errorf("dot(nil, nil) = %v, want 0", got)
	}
	if got := dot([]float64{}, []float64{1, 2}); got != 0 {
		t.Errorf("dot(empty, nonempty) = %v, want 0", got)
	}
	if got := dot([]float64{1, 2}, nil); got != 0 {
		t.Errorf("dot(nonempty, nil) = %v, want 0", got)
	}
}

func TestDotSingleElement(t *testing.T) {
	t.Parallel()
	got := dot([]float64{7}, []float64{3})
	if got != 21 {
		t.Errorf("dot([7],[3]) = %v, want 21", got)
	}
}

func TestDotNegativeValues(t *testing.T) {
	t.Parallel()
	a := []float64{-1, -2, -3}
	b := []float64{4, -5, 6}
	got := dot(a, b)
	want := -1*4 + -2*-5 + -3*6.0 // -4 + 10 - 18 = -12
	if got != want {
		t.Errorf("dot(neg, mixed) = %v, want %v", got, want)
	}
}

func TestDotLargeValues(t *testing.T) {
	t.Parallel()
	a := []float64{1e15, 1e15}
	b := []float64{1e15, 1e15}
	got := dot(a, b)
	want := 2e30
	if got != want {
		t.Errorf("dot(large) = %v, want %v", got, want)
	}
}

func TestL2NormUnitVector(t *testing.T) {
	t.Parallel()
	for i := 0; i < 3; i++ {
		v := make([]float64, 3)
		v[i] = 1.0
		got := l2Norm(v)
		if got != 1.0 {
			t.Errorf("l2Norm(unit axis %d) = %v, want 1", i, got)
		}
	}
}

func TestL2NormZeroVector(t *testing.T) {
	t.Parallel()
	if got := l2Norm([]float64{0, 0, 0}); got != 0 {
		t.Errorf("l2Norm(zero) = %v, want 0", got)
	}
	if got := l2Norm(nil); got != 0 {
		t.Errorf("l2Norm(nil) = %v, want 0", got)
	}
}

func TestL2NormKnownValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    []float64
		want float64
	}{
		{"3-4-5", []float64{3, 4}, 5},
		{"single", []float64{7}, 7},
		{"negative", []float64{-3, -4}, 5},
		{"1,1,1,1", []float64{1, 1, 1, 1}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := l2Norm(tt.v)
			if math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("l2Norm(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestL2NormSingleNegative(t *testing.T) {
	t.Parallel()
	got := l2Norm([]float64{-5})
	if got != 5 {
		t.Errorf("l2Norm([-5]) = %v, want 5", got)
	}
}

// --- Benchmarks ---

func BenchmarkDotSmall(b *testing.B) {
	a := make([]float64, 8)
	bv := make([]float64, 8)
	for i := range a {
		a[i] = float64(i)
		bv[i] = float64(i * 2)
	}
	b.ResetTimer()
	for b.Loop() {
		dot(a, bv)
	}
}

func BenchmarkDotLarge(b *testing.B) {
	const n = 4096
	a := make([]float64, n)
	bv := make([]float64, n)
	for i := range a {
		a[i] = float64(i)
		bv[i] = float64(i * 2)
	}
	b.ResetTimer()
	for b.Loop() {
		dot(a, bv)
	}
}

func BenchmarkL2NormSmall(b *testing.B) {
	v := make([]float64, 8)
	for i := range v {
		v[i] = float64(i + 1)
	}
	b.ResetTimer()
	for b.Loop() {
		l2Norm(v)
	}
}

func BenchmarkL2NormLarge(b *testing.B) {
	const n = 4096
	v := make([]float64, n)
	for i := range v {
		v[i] = float64(i + 1)
	}
	b.ResetTimer()
	for b.Loop() {
		l2Norm(v)
	}
}
