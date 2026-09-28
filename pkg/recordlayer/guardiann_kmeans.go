package recordlayer

import (
	"container/heap"
	"math"
	"sort"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// Java package com.apple.foundationdb.kmeans: KMeans.fit and
// PartitionEvaluator, over GuardiANN vector references.

type kMeansResult struct {
	centroids    []gVector
	clusterSizes []int
	assignment   []int
	distances    []float64
	objective    float64
}

// kMeansAdapter is KMeans.MetricAdapter.
type kMeansAdapter struct{ metric VectorMetric }

func (a kMeansAdapter) baseObjective(v, c []float64) float64 {
	if a.metric == VectorMetricCosine {
		return javaMetricDistance(v, c, a.metric)
	}
	return l2SquaredSequential(v, c)
}

// javaMetricDistance is MetricDefinition.distance over the scalar backend:
// sequential sums, and cosine without clamping (a zero vector is +Inf).
func javaMetricDistance(a, b []float64, metric VectorMetric) float64 {
	switch metric {
	case VectorMetricEuclidean:
		return math.Sqrt(l2SquaredSequential(a, b))
	case VectorMetricEuclideanSquare:
		return l2SquaredSequential(a, b)
	case VectorMetricCosine:
		na, nb := dotSequential(a, a), dotSequential(b, b)
		if na == 0 || nb == 0 {
			return math.Inf(1)
		}
		return 1 - dotSequential(a, b)/(math.Sqrt(na)*math.Sqrt(nb))
	}
	return vectorDistance(a, b, metric)
}

func (a kMeansAdapter) renormalize(v []float64) {
	if a.metric != VectorMetricCosine {
		return
	}
	norm := math.Sqrt(dotSequential(v, v))
	if norm == 0 {
		return
	}
	scale(v, 1/norm)
}

func (a kMeansAdapter) meaninglessNorm(v []float64) bool {
	return a.metric == VectorMetricCosine && dotSequential(v, v) <= realVectorEPS*realVectorEPS
}

func l2SquaredSequential(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

func dotSequential(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// kMeansFit is KMeans.fit with lambda 0 (GuardiANN's call): k-means++
// initialisation, Lloyd iterations, and the best of maxRestarts+1 runs.
func kMeansFit(random *splittableRandom, metric VectorMetric, vectors [][]float64, k, maxIterations, maxRestarts int) kMeansResult {
	a := kMeansAdapter{metric: metric}
	n := len(vectors)
	dims := len(vectors[0])
	if k == 1 {
		centroid := make([]float64, dims)
		for _, v := range vectors {
			addInto(centroid, v)
		}
		scale(centroid, 1/float64(n))
		if a.meaninglessNorm(centroid) {
			copy(centroid, vectors[farthestVectorIndex(a, vectors, [][]float64{centroid})])
		} else {
			a.renormalize(centroid)
		}
		res := kMeansResult{
			centroids: []gVector{{data: centroid, typ: vectorcodec.TypeDouble}}, clusterSizes: []int{n},
			assignment: make([]int, n), distances: make([]float64, n),
		}
		for i, v := range vectors {
			res.distances[i] = a.baseObjective(v, centroid)
			res.objective += res.distances[i]
		}
		return res
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	next := make([][]float64, k)
	for c := range next {
		next[c] = make([]float64, dims)
	}
	var best *kMeansResult
	for r := 0; r <= maxRestarts; r++ {
		centroids := initKMeansPP(a, random, vectors, k)
		assignment := make([]int, n)
		for i := range assignment {
			assignment[i] = -1
		}
		sizes := make([]int, k)
		for iteration := 0; iteration < maxIterations; iteration++ {
			projected := make([]int, k)
			changed := assignmentStep(a, vectors, centroids, order, assignment, projected)
			copy(sizes, projected)
			if changed == 0 {
				break
			}
			for c := range next {
				for i := range next[c] {
					next[c][i] = 0
				}
			}
			for i, v := range vectors {
				addInto(next[assignment[i]], v)
			}
			for c := 0; c < k; c++ {
				if sizes[c] == 0 {
					copy(next[c], vectors[farthestVectorIndex(a, vectors, centroids)])
					sizes[c] = 1
					continue
				}
				scale(next[c], 1/float64(sizes[c]))
				if a.meaninglessNorm(next[c]) {
					copy(next[c], vectors[farthestVectorIndex(a, vectors, centroids)])
				} else {
					a.renormalize(next[c])
				}
			}
			centroids, next = next, centroids
		}
		projected := make([]int, k)
		assignmentStep(a, vectors, centroids, order, assignment, projected)
		copy(sizes, projected)
		cand := kMeansResult{
			clusterSizes: append([]int(nil), sizes...), assignment: append([]int(nil), assignment...),
			distances: make([]float64, n),
		}
		for i, v := range vectors {
			cand.distances[i] = a.baseObjective(v, centroids[assignment[i]])
			cand.objective += cand.distances[i]
		}
		for _, c := range centroids {
			cand.centroids = append(cand.centroids, gVector{data: append([]float64(nil), c...), typ: vectorcodec.TypeDouble})
		}
		if best == nil || cand.objective < best.objective {
			best = &cand
		}
		if r < maxRestarts {
			// centroids and next alias the working buffers; restart with fresh ones.
			next = make([][]float64, k)
			for c := range next {
				next[c] = make([]float64, dims)
			}
		}
	}
	return *best
}

func addInto(dst, v []float64) {
	for i := range dst {
		dst[i] += v[i]
	}
}

func scale(v []float64, f float64) {
	for i := range v {
		v[i] *= f
	}
}

func assignmentStep(a kMeansAdapter, vectors, centroids [][]float64, order, assignment, projected []int) int {
	changed := 0
	for _, i := range order {
		bestC := 0
		bestScore := a.baseObjective(vectors[i], centroids[0])
		for c := 1; c < len(centroids); c++ {
			if s := a.baseObjective(vectors[i], centroids[c]); s < bestScore {
				bestScore, bestC = s, c
			}
		}
		if assignment[i] != bestC {
			assignment[i] = bestC
			changed++
		}
		projected[bestC]++
	}
	return changed
}

func initKMeansPP(a kMeansAdapter, random *splittableRandom, vectors [][]float64, k int) [][]float64 {
	n := len(vectors)
	centroids := make([][]float64, 0, k)
	latest := append([]float64(nil), vectors[random.nextInt(n)]...)
	centroids = append(centroids, latest)
	weights := make([]float64, n)
	total := 0.0
	for i, v := range vectors {
		weights[i] = a.baseObjective(v, latest)
		total += weights[i]
	}
	for len(centroids) < k {
		var chosen int
		if total == 0 {
			chosen = random.nextInt(n)
		} else {
			pick := random.nextDouble() * total
			cumulative := 0.0
			chosen = n - 1
			for i := 0; i < n; i++ {
				cumulative += weights[i]
				if cumulative >= pick {
					chosen = i
					break
				}
			}
		}
		latest = append([]float64(nil), vectors[chosen]...)
		centroids = append(centroids, latest)
		if len(centroids) < k {
			total = 0
			for i, v := range vectors {
				if d := a.baseObjective(v, latest); d < weights[i] {
					weights[i] = d
				}
				total += weights[i]
			}
		}
	}
	return centroids
}

func farthestVectorIndex(a kMeansAdapter, vectors, centroids [][]float64) int {
	best, bestIdx := -1.0, 0
	for i, v := range vectors {
		m := math.MaxFloat64
		for _, c := range centroids {
			if o := a.baseObjective(v, c); o < m {
				m = o
			}
		}
		if m > best {
			best, bestIdx = m, i
		}
	}
	return bestIdx
}

// Partition evaluation (PartitionEvaluator).

type partitionDecision int

const (
	decisionKeepCurrent partitionDecision = iota
	decisionAcceptCandidate
	decisionInvalidCandidate
)

type partitionParameters struct {
	metric                VectorMetric
	minRelativeSseGain    float64
	minSeparation         float64
	maxLowMarginRate      float64
	minChildFraction      float64
	maxRelativeImbalance  float64
	lowMarginThreshold    float64
	alphaSseGain          float64
	betaSeparationGain    float64
	gammaImbalancePenalty float64
	deltaLowMarginPenalty float64
	minScoreGain          float64
}

func defaultPartitionParameters(metric VectorMetric) partitionParameters {
	return partitionParameters{
		metric: metric, minRelativeSseGain: 0.10, minSeparation: 0.3, maxLowMarginRate: 0.25,
		minChildFraction: 0.015, maxRelativeImbalance: 1.0, lowMarginThreshold: -1.0, alphaSseGain: 1.0,
		betaSeparationGain: 0.5, gammaImbalancePenalty: 1.0, deltaLowMarginPenalty: 0.75, minScoreGain: 0.05,
	}
}

type partition struct {
	centroids   [][]float64
	assignments []int
}

type partitionStats struct {
	k                                                     int
	sse, imbalance, separation, largestFrac, smallestFrac float64
	lowMarginRate                                         float64
}

func (s partitionStats) relativeImbalance() float64 {
	if s.k < 2 {
		return 0
	}
	return s.imbalance * float64(s.k) / (float64(s.k) - 1)
}

type evaluationResult struct {
	decision  partitionDecision
	scoreGain float64
}

func nanToZero(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

// evaluatePartitions is PartitionEvaluator.evaluate.
func evaluatePartitions(currentVectors [][]float64, current partition, candidateVectors [][]float64, candidate partition, p partitionParameters) evaluationResult {
	cs := evaluatePartition(currentVectors, current, p)
	ks := evaluatePartition(candidateVectors, candidate, p)
	relativeSseGain := (cs.sse - ks.sse) / math.Max(cs.sse, 1e-12)
	separationGain := nanToZero(ks.separation) - nanToZero(cs.separation)
	lowMarginPenalty := math.Max(0, nanToZero(ks.lowMarginRate)-nanToZero(cs.lowMarginRate))
	imbalancePenalty := math.Max(0, ks.relativeImbalance()-cs.relativeImbalance())
	scoreGain := p.alphaSseGain*relativeSseGain + p.betaSeparationGain*separationGain -
		p.gammaImbalancePenalty*imbalancePenalty - p.deltaLowMarginPenalty*lowMarginPenalty
	switch {
	case ks.smallestFrac < p.minChildFraction:
		return evaluationResult{decisionInvalidCandidate, scoreGain}
	case ks.relativeImbalance() > p.maxRelativeImbalance:
		return evaluationResult{decisionKeepCurrent, scoreGain}
	case len(candidate.centroids) >= 2 && (math.IsNaN(ks.separation) || ks.separation < p.minSeparation):
		return evaluationResult{decisionKeepCurrent, scoreGain}
	case len(candidate.centroids) >= 2 && ks.lowMarginRate > p.maxLowMarginRate:
		return evaluationResult{decisionKeepCurrent, scoreGain}
	case relativeSseGain < p.minRelativeSseGain:
		return evaluationResult{decisionKeepCurrent, scoreGain}
	case scoreGain < p.minScoreGain:
		return evaluationResult{decisionKeepCurrent, scoreGain}
	}
	return evaluationResult{decisionAcceptCandidate, scoreGain}
}

type floatMinHeap []float64

func (h floatMinHeap) Len() int           { return len(h) }
func (h floatMinHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h floatMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *floatMinHeap) Push(x any)        { *h = append(*h, x.(float64)) }
func (h *floatMinHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func evaluatePartition(vectors [][]float64, p partition, params partitionParameters) partitionStats {
	n, k := len(vectors), len(p.centroids)
	distance := func(a, b []float64) float64 { return javaMetricDistance(a, b, params.metric) }
	needP95 := params.metric != VectorMetricCosine && params.lowMarginThreshold <= 0
	childSizes := make([]int, k)
	childRadii := make([][]float64, k)
	var margins []float64
	sse := 0.0
	p95Size := 0
	var p95 floatMinHeap
	if needP95 {
		p95Size = max(1, int(math.Ceil(0.05*float64(n))))
	}
	for i, v := range vectors {
		own := p.assignments[i]
		ownC := p.centroids[own]
		childSizes[own]++
		if params.metric == VectorMetricCosine {
			sse += 2 * distance(v, ownC)
		} else {
			sse += l2SquaredSequential(v, ownC)
		}
		d := distance(v, ownC)
		childRadii[own] = append(childRadii[own], d)
		if needP95 {
			if p95.Len() < p95Size {
				heap.Push(&p95, d)
			} else if d > p95[0] {
				heap.Pop(&p95)
				heap.Push(&p95, d)
			}
		}
		if k >= 2 {
			margins = append(margins, computeMargin(params.metric, p, v, own))
		}
	}
	overallP95 := math.NaN()
	if p95.Len() > 0 {
		overallP95 = p95[0]
	}
	lowMarginThreshold := params.lowMarginThreshold
	if lowMarginThreshold <= 0 {
		if params.metric == VectorMetricCosine {
			lowMarginThreshold = 0.02
		} else {
			lowMarginThreshold = 0.05 * overallP95
		}
	}
	target := float64(n) / float64(k)
	sumSq := 0.0
	minSize, maxSize := math.MaxInt, math.MinInt
	for _, sz := range childSizes {
		d := float64(sz) - target
		sumSq += d * d
		minSize, maxSize = min(minSize, sz), max(maxSize, sz)
	}
	maxRadius95 := 0.0
	for _, radii := range childRadii {
		if len(radii) > 0 {
			maxRadius95 = math.Max(maxRadius95, percentile(radii, 0.95))
		}
	}
	separation := math.NaN()
	if k >= 2 {
		minDist := math.Inf(1)
		for i := 0; i < k; i++ {
			for j := i + 1; j < k; j++ {
				minDist = math.Min(minDist, distance(p.centroids[i], p.centroids[j]))
			}
		}
		separation = minDist / math.Max(maxRadius95, 1e-12)
	}
	lowRate := 0.0
	if k >= 2 {
		low := 0
		for _, m := range margins {
			if m < lowMarginThreshold {
				low++
			}
		}
		lowRate = float64(low) / float64(n)
	}
	return partitionStats{
		k: k, sse: sse, imbalance: sumSq / (float64(n) * float64(n)), separation: separation,
		largestFrac: float64(maxSize) / float64(n), smallestFrac: float64(minSize) / float64(n), lowMarginRate: lowRate,
	}
}

func computeMargin(metric VectorMetric, p partition, v []float64, own int) float64 {
	if metric == VectorMetricCosine {
		clamped := func(c []float64) float64 { return math.Max(-1, math.Min(1, dotSequential(v, c))) }
		ownS := clamped(p.centroids[own])
		second := math.Inf(-1)
		for j, c := range p.centroids {
			if j != own {
				second = math.Max(second, clamped(c))
			}
		}
		return ownS - second
	}
	ownD := javaMetricDistance(v, p.centroids[own], metric)
	second := math.Inf(1)
	for j, c := range p.centroids {
		if j != own {
			second = math.Min(second, javaMetricDistance(v, c, metric))
		}
	}
	return second - ownD
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	if len(values) == 1 {
		return values[0]
	}
	c := append([]float64(nil), values...)
	sort.Slice(c, func(i, j int) bool { return compareFloat64Java(c[i], c[j]) < 0 })
	rank := p * float64(len(c)-1)
	lo, hi := int(math.Floor(rank)), int(math.Ceil(rank))
	if lo == hi {
		return c[lo]
	}
	w := rank - float64(lo)
	return c[lo]*(1-w) + c[hi]*w
}
