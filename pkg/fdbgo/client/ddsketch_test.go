package client

import (
	"math"
	"sort"
	"testing"
)

// trueQuantile returns the exact value at 0-indexed rank floor(p*(n-1)) of a
// sorted copy — the same target-rank convention as DDSketchBase::percentile.
func trueQuantile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	return s[int(p*float64(len(s)-1))]
}

// TestDDSketch_RelativeErrorGuarantee: every reported quantile is within the
// DDSketch relative-error bound of the true quantile. The sketch's errorGuarantee
// is 0.005, so a bucket spans a factor of γ ≈ 1.01005; the reported bucket
// representative and the true value can additionally sit one bucket apart when
// the rank straddles a boundary, so the practical bound is ~2·(γ−1) ≈ 0.0201. A
// regression to the wrong γ (wider buckets) blows past this immediately.
func TestDDSketch_RelativeErrorGuarantee(t *testing.T) {
	t.Parallel()
	var s latencySketch
	var samples []float64
	for i := 1; i <= 100000; i++ {
		v := float64(i) * 1e-4 // 0.1ms .. 10s, dense
		s.addSample(v)
		samples = append(samples, v)
	}
	st := s.stats()
	if st.Count != int64(len(samples)) {
		t.Fatalf("Count = %d, want %d", st.Count, len(samples))
	}
	for _, c := range []struct {
		p   float64
		got float64
	}{{0.5, st.Median}, {0.9, st.P90}, {0.99, st.P99}} {
		want := trueQuantile(samples, c.p)
		rel := math.Abs(c.got-want) / want
		const bound = 0.0201 // ~2·(γ−1)
		if rel > bound {
			t.Errorf("p%.0f: got %.6f want %.6f rel-err %.4f > %.4f", c.p*100, c.got, want, rel, bound)
		}
	}
	// Mean, Min and Max are exact (not bucketed).
	if rel := math.Abs(st.Max-trueQuantile(samples, 1.0)) / st.Max; rel != 0 {
		t.Errorf("Max not exact: got %.6f want %.6f", st.Max, trueQuantile(samples, 1.0))
	}
	if st.Min != trueQuantile(samples, 0.0) {
		t.Errorf("Min not exact: got %.6f want %.6f", st.Min, trueQuantile(samples, 0.0))
	}
	wantMean := 0.0
	for _, v := range samples {
		wantMean += v
	}
	wantMean /= float64(len(samples))
	if rel := math.Abs(st.Mean-wantMean) / wantMean; rel > 1e-9 {
		t.Errorf("Mean = %.6f, want %.6f", st.Mean, wantMean)
	}
}

// TestDDSketch_Edges pins the empty / single / all-zero / non-finite cases.
func TestDDSketch_Edges(t *testing.T) {
	t.Parallel()

	// Empty.
	var empty latencySketch
	if st := empty.stats(); st.Count != 0 || st.Mean != 0 || st.Median != 0 || st.P99 != 0 || st.Max != 0 {
		t.Errorf("empty sketch stats = %+v, want all zero", st)
	}

	// Single sample: Count 1, Max exact, Mean exact, Median within bound.
	var one latencySketch
	one.addSample(2.5)
	st := one.stats()
	if st.Count != 1 || st.Max != 2.5 || st.Min != 2.5 || st.Mean != 2.5 {
		t.Errorf("single-sample stats = %+v, want count1 min/max/mean 2.5", st)
	}
	if rel := math.Abs(st.Median-2.5) / 2.5; rel > 0.0201 {
		t.Errorf("single-sample median = %.6f, want ~2.5", st.Median)
	}

	// All-zero samples: counted, but every quantile is 0 (zeroPopulation path).
	var zeros latencySketch
	for i := 0; i < 10; i++ {
		zeros.addSample(0)
	}
	st = zeros.stats()
	if st.Count != 10 || st.Median != 0 || st.P99 != 0 || st.Max != 0 || st.Min != 0 || st.Mean != 0 {
		t.Errorf("all-zero stats = %+v, want count10 and zeros", st)
	}

	// Non-finite / negative are dropped, not bucketed into garbage.
	var guard latencySketch
	guard.addSample(-1)
	guard.addSample(math.NaN())
	guard.addSample(math.Inf(1))
	guard.addSample(math.Inf(-1))
	if st := guard.stats(); st.Count != 0 {
		t.Errorf("non-finite/negative samples counted: %+v", st)
	}
}

// TestDDSketch_Monotonic: quantiles are non-decreasing in p (higher p ⇒ higher-or-
// equal bucket index ⇒ non-decreasing representative).
func TestDDSketch_Monotonic(t *testing.T) {
	t.Parallel()
	var s latencySketch
	// A deliberately skewed set (no rand — deterministic).
	for i := 1; i <= 1000; i++ {
		s.addSample(float64(i*i) * 1e-6)
	}
	st := s.stats()
	if !(st.Median <= st.P90 && st.P90 <= st.P99 && st.P99 <= st.Max) {
		t.Errorf("quantiles not monotone: median=%g p90=%g p99=%g max=%g", st.Median, st.P90, st.P99, st.Max)
	}
	if st.Median <= 0 {
		t.Errorf("median should be positive, got %g", st.Median)
	}
}

// FuzzDDSketch: arbitrary sample streams never panic and always yield finite,
// monotone, in-range stats.
func FuzzDDSketch(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{255, 128, 1, 200, 7, 9, 250})
	f.Fuzz(func(t *testing.T, data []byte) {
		var s latencySketch
		for i, b := range data {
			// Spread magnitudes across 1e-3 .. 1e2 without ever producing NaN/Inf.
			s.addSample(float64(b) * math.Pow(10, float64(i%6-3)))
		}
		st := s.stats()
		if st.Count != int64(len(data)) {
			t.Fatalf("Count = %d, want %d", st.Count, len(data))
		}
		for _, v := range []float64{st.Mean, st.Median, st.P90, st.P99, st.Min, st.Max, st.Sum} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("non-finite stat in %+v", st)
			}
		}
		if len(data) > 0 && !(st.Min <= st.Median && st.Median <= st.P90 && st.P90 <= st.P99) {
			t.Fatalf("quantiles not monotone: %+v", st)
		}
	})
}
