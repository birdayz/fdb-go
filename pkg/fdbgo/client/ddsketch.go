package client

import (
	"math"
	"math/rand/v2"
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
const numSketchStripes = 16

type latencySketchStripe struct {
	mu             sync.Mutex
	buckets        map[int]uint64 // signed bucket index -> count; lazily created
	populationSize uint64
	zeroPopulation uint64 // samples <= ddSketchEPS (count as 0, per C++)
	sum            float64
	minValue       float64
	maxValue       float64
	_              [8]byte // pad to 64B cache line
}

type latencySketch struct {
	stripes [numSketchStripes]latencySketchStripe
}

func (s *latencySketch) lockAll() {
	for i := range s.stripes {
		s.stripes[i].mu.Lock()
	}
}

func (s *latencySketch) unlockAll() {
	for i := len(s.stripes) - 1; i >= 0; i-- {
		s.stripes[i].mu.Unlock()
	}
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
	var idx int
	isZero := v <= ddSketchEPS
	if !isZero {
		idx = bucketIndex(v)
	}
	st := &s.stripes[rand.Uint64()&(numSketchStripes-1)]
	st.mu.Lock()
	if st.populationSize == 0 {
		st.minValue, st.maxValue = v, v
	}
	if isZero {
		st.zeroPopulation++
	} else {
		if st.buckets == nil {
			st.buckets = make(map[int]uint64, 32)
		}
		st.buckets[idx]++
	}
	st.populationSize++
	st.sum += v
	if v > st.maxValue {
		st.maxValue = v
	}
	if v < st.minValue {
		st.minValue = v
	}
	st.mu.Unlock()
}

// stats returns a point-in-time snapshot. Sorts the occupied bucket indices once
// and answers all requested percentiles from that single sorted slice (vs the
// C++ per-call vector walk) — snapshot is not the hot path.
func (s *latencySketch) stats() LatencyStats {
	var (
		populationSize uint64
		zeroPopulation uint64
		sum            float64
		minValue       float64
		maxValue       float64
		buckets        map[int]uint64
	)
	for i := range s.stripes {
		st := &s.stripes[i]
		st.mu.Lock()
		if st.populationSize > 0 {
			if populationSize == 0 {
				minValue, maxValue = st.minValue, st.maxValue
			} else {
				if st.minValue < minValue {
					minValue = st.minValue
				}
				if st.maxValue > maxValue {
					maxValue = st.maxValue
				}
			}
			populationSize += st.populationSize
			zeroPopulation += st.zeroPopulation
			sum += st.sum
			if len(st.buckets) > 0 {
				if buckets == nil {
					buckets = make(map[int]uint64, len(st.buckets))
				}
				for k, c := range st.buckets {
					buckets[k] += c
				}
			}
		}
		st.mu.Unlock()
	}
	st := LatencyStats{Count: int64(populationSize)}
	if populationSize == 0 {
		return st
	}
	st.Sum = sum
	st.Mean = sum / float64(populationSize)
	st.Min = minValue
	st.Max = maxValue

	idxs := make([]int, 0, len(buckets))
	for k := range buckets {
		idxs = append(idxs, k)
	}
	sort.Ints(idxs)

	st.Median = percentileMerged(0.50, idxs, buckets, populationSize, zeroPopulation, maxValue)
	st.P90 = percentileMerged(0.90, idxs, buckets, populationSize, zeroPopulation, maxValue)
	st.P99 = percentileMerged(0.99, idxs, buckets, populationSize, zeroPopulation, maxValue)
	return st
}

// percentileMerged ports DDSketchBase::percentile (DDSketch.h:123) over the
// pre-sorted occupied indices.
func percentileMerged(p float64, idxs []int, buckets map[int]uint64, populationSize, zeroPopulation uint64, maxValue float64) float64 {
	if populationSize == 0 {
		return 0
	}
	// C++ truncates p*(pop-1) to uint64 (the 0-indexed target rank).
	target := uint64(p * float64(populationSize-1))
	if target < zeroPopulation {
		return 0
	}
	if p <= 0.5 { // count up from the smallest bucket (incl. the zero population)
		count := zeroPopulation
		for _, i := range idxs {
			if target < count+buckets[i] {
				return bucketValue(i)
			}
			count += buckets[i]
		}
	} else { // count down from the largest bucket
		var count uint64
		for j := len(idxs) - 1; j >= 0; j-- {
			i := idxs[j]
			if target+count+buckets[i] >= populationSize {
				return bucketValue(i)
			}
			count += buckets[i]
		}
	}
	// Unreachable for a non-empty sketch (target >= zeroPopulation is always in
	// some bucket); fall back to max rather than the C++ -1 sentinel.
	return maxValue
}
