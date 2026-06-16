package client

import (
	"math"
	"sort"
	"sync"
)

// latencySketch is a faithful port of C++ DDSketch<double> (fdbrpc/include/
// fdbrpc/DDSketch.h), the type behind DatabaseContext's read/commit/GRV latency
// distributions (DatabaseContext.h:657). It answers mean / median / arbitrary
// percentiles / max over a stream of non-negative samples with a bounded
// relative error, in O(1) memory per occupied bucket.
//
// errorGuarantee is 0.005 to match the default-constructed C++ sketches
// (DDSketch.h:220 default arg; the DatabaseContext members are default
// constructed, NativeAPI.actor.cpp:1585). gamma = (1+eG)/(1-eG); a value v lands
// in bucket index = ceil(log(v)/log(gamma)), whose representative value is
// 2*gamma^index/(1+gamma) — within (gamma^(index-1), gamma^index], so every
// reported quantile is within ±eG of the true value.
//
// Two deliberate divergences from the C++ *fast* DDSketch variant, both
// local-metric-only (these counters never touch the wire) and documented:
//
//  1. Exact math.Log/math.Pow instead of C++'s fastLogger bit-hack +
//     correctingFactor (DDSketch.h:229-237). The bit-hack is a pure speed
//     optimization that adds its own approximation; exact log keeps the same
//     gamma and the same eG guarantee and is strictly more accurate. A sampled
//     op is a network round-trip (ms); a math.Log (ns) is noise.
//  2. Sparse map[int]uint64 buckets instead of C++'s pre-sized 2*offset vector.
//     Sub-second latencies (the common case) map to negative indices, which are
//     natural map keys — no offset bookkeeping. percentile() sorts the occupied
//     indices, but only at snapshot time, never on the sampling hot path.
//
// The zero value is ready to use (the bucket map is created lazily under the
// lock), so a latencySketch can be embedded directly in ClientMetrics with no
// constructor.
type latencySketch struct {
	mu             sync.Mutex
	buckets        map[int]uint64 // signed bucket index -> count; lazily created
	populationSize uint64
	zeroPopulation uint64 // samples <= ddSketchEPS (count as 0, per C++)
	sum            float64
	minValue       float64
	maxValue       float64
}

const (
	// ddSketchErrorGuarantee matches the C++ DDSketch<double> default
	// (DDSketch.h:220) used by the DatabaseContext latency members.
	ddSketchErrorGuarantee = 0.005
	// ddSketchEPS mirrors C++ DDSketchBase::EPS — samples at or below it are
	// counted as zero rather than bucketed (DDSketch.h:206).
	ddSketchEPS = 1e-18
)

var (
	ddSketchGamma    = (1.0 + ddSketchErrorGuarantee) / (1.0 - ddSketchErrorGuarantee)
	ddSketchLogGamma = math.Log(ddSketchGamma)
)

// bucketIndex mirrors DDSketch getIndex: ceil(log(v)/log(gamma)). v must be > EPS.
func bucketIndex(v float64) int {
	return int(math.Ceil(math.Log(v) / ddSketchLogGamma))
}

// bucketValue mirrors DDSketchSlow getValue: 2*gamma^index/(1+gamma), the
// representative value of a bucket.
func bucketValue(index int) float64 {
	return 2.0 * math.Pow(ddSketchGamma, float64(index)) / (1.0 + ddSketchGamma)
}

// addSample ports DDSketchBase::addSample (DDSketch.h:86). Negative samples
// (a clock-skew artifact; latencies are non-negative) are dropped rather than
// bucketed into a bogus index.
func (s *latencySketch) addSample(v float64) {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets == nil {
		s.buckets = make(map[int]uint64)
	}
	if s.populationSize == 0 {
		s.minValue, s.maxValue = v, v
	}
	if v <= ddSketchEPS {
		s.zeroPopulation++
	} else {
		s.buckets[bucketIndex(v)]++
	}
	s.populationSize++
	s.sum += v
	if v > s.maxValue {
		s.maxValue = v
	}
	if v < s.minValue {
		s.minValue = v
	}
}

// stats returns a point-in-time snapshot. Sorts the occupied bucket indices once
// and answers all requested percentiles from that single sorted slice (vs the
// C++ per-call vector walk) — snapshot is not the hot path.
func (s *latencySketch) stats() LatencyStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := LatencyStats{Count: int64(s.populationSize)}
	if s.populationSize == 0 {
		return st
	}
	st.Sum = s.sum
	st.Mean = s.sum / float64(s.populationSize)
	st.Min = s.minValue
	st.Max = s.maxValue

	idxs := make([]int, 0, len(s.buckets))
	for k := range s.buckets {
		idxs = append(idxs, k)
	}
	sort.Ints(idxs)

	st.Median = s.percentileLocked(0.50, idxs)
	st.P90 = s.percentileLocked(0.90, idxs)
	st.P99 = s.percentileLocked(0.99, idxs)
	return st
}

// percentileLocked ports DDSketchBase::percentile (DDSketch.h:123) over the
// pre-sorted occupied indices. Caller holds s.mu and supplies ascending idxs.
func (s *latencySketch) percentileLocked(p float64, idxs []int) float64 {
	if s.populationSize == 0 {
		return 0
	}
	// C++ truncates p*(pop-1) to uint64 (the 0-indexed target rank).
	target := uint64(p * float64(s.populationSize-1))
	if target < s.zeroPopulation {
		return 0
	}
	if p <= 0.5 { // count up from the smallest bucket (incl. the zero population)
		count := s.zeroPopulation
		for _, i := range idxs {
			if target < count+s.buckets[i] {
				return bucketValue(i)
			}
			count += s.buckets[i]
		}
	} else { // count down from the largest bucket
		var count uint64
		for j := len(idxs) - 1; j >= 0; j-- {
			i := idxs[j]
			if target+count+s.buckets[i] >= s.populationSize {
				return bucketValue(i)
			}
			count += s.buckets[i]
		}
	}
	// Unreachable for a non-empty sketch (target >= zeroPopulation is always in
	// some bucket); fall back to max rather than the C++ -1 sentinel.
	return s.maxValue
}
