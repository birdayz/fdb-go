// Portions derived from FoundationDB Record Layer (KMeans.java,
// PartitionEvaluator.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"container/heap"
	"math"
	"sort"

	"fdb.dev/pkg/rabitq"
	"fdb.dev/pkg/recordlayer"
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
type kMeansAdapter struct{ codec *guardiannVectorCodec }

func (a kMeansAdapter) baseObjective(v gVector, c []float64) (float64, error) {
	if a.codec.config.metric == VectorMetricCosine || a.codec.quantizer != nil && v.typ == rabitq.TypeByte {
		d, err := a.codec.distance(v, gVector{data: c, typ: vectorcodec.TypeDouble})
		if a.codec.config.metric != VectorMetricCosine {
			d *= d
		}
		return d, err
	}
	return l2SquaredSequential(v.data, c), nil
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
	if a.codec.config.metric != VectorMetricCosine {
		return
	}
	norm := math.Sqrt(dotSequential(v, v))
	if norm == 0 {
		return
	}
	scale(v, 1/norm)
}

func (a kMeansAdapter) meaninglessNorm(v []float64) bool {
	return a.codec.config.metric == VectorMetricCosine && dotSequential(v, v) <= realVectorEPS*realVectorEPS
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
func kMeansFit(random *splittableRandom, codec *guardiannVectorCodec, vectors []gVector, k, maxIterations, maxRestarts int) (kMeansResult, error) {
	switch {
	case k < 1:
		return kMeansResult{}, &recordlayer.IllegalArgumentError{Message: "k must be >= 1"}
	case len(vectors) < k:
		return kMeansResult{}, &recordlayer.IllegalArgumentError{Message: "vectors.size() must be >= k"}
	case maxIterations < 1:
		return kMeansResult{}, &recordlayer.IllegalArgumentError{Message: "maxIterations must be >= 1"}
	case maxRestarts < 0:
		return kMeansResult{}, &recordlayer.IllegalArgumentError{Message: "maxRestarts must be >= 0"}
	}
	a := kMeansAdapter{codec: codec}
	n := len(vectors)
	dims := len(vectors[0].data)
	if k == 1 {
		centroid := make([]float64, dims)
		for _, v := range vectors {
			addInto(centroid, v.data)
		}
		scale(centroid, 1/float64(n))
		if a.meaninglessNorm(centroid) {
			index, err := farthestVectorIndex(a, vectors, [][]float64{centroid})
			if err != nil {
				return kMeansResult{}, err
			}
			copy(centroid, vectors[index].data)
		} else {
			a.renormalize(centroid)
		}
		res := kMeansResult{
			centroids: []gVector{{data: centroid, typ: vectorcodec.TypeDouble}}, clusterSizes: []int{n},
			assignment: make([]int, n), distances: make([]float64, n),
		}
		for i, v := range vectors {
			var err error
			res.distances[i], err = a.baseObjective(v, centroid)
			if err != nil {
				return kMeansResult{}, err
			}
			res.objective += res.distances[i]
		}
		return res, nil
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
		centroids, err := initKMeansPP(a, random, vectors, k)
		if err != nil {
			return kMeansResult{}, err
		}
		assignment := make([]int, n)
		for i := range assignment {
			assignment[i] = -1
		}
		sizes := make([]int, k)
		for iteration := 0; iteration < maxIterations; iteration++ {
			projected := make([]int, k)
			changed, err := assignmentStep(a, vectors, centroids, order, assignment, projected)
			if err != nil {
				return kMeansResult{}, err
			}
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
				addInto(next[assignment[i]], v.data)
			}
			for c := 0; c < k; c++ {
				if sizes[c] == 0 {
					index, err := farthestVectorIndex(a, vectors, centroids)
					if err != nil {
						return kMeansResult{}, err
					}
					copy(next[c], vectors[index].data)
					sizes[c] = 1
					continue
				}
				scale(next[c], 1/float64(sizes[c]))
				if a.meaninglessNorm(next[c]) {
					index, err := farthestVectorIndex(a, vectors, centroids)
					if err != nil {
						return kMeansResult{}, err
					}
					copy(next[c], vectors[index].data)
				} else {
					a.renormalize(next[c])
				}
			}
			centroids, next = next, centroids
		}
		projected := make([]int, k)
		if _, err = assignmentStep(a, vectors, centroids, order, assignment, projected); err != nil {
			return kMeansResult{}, err
		}
		copy(sizes, projected)
		cand := kMeansResult{
			clusterSizes: append([]int(nil), sizes...), assignment: append([]int(nil), assignment...),
			distances: make([]float64, n),
		}
		for i, v := range vectors {
			cand.distances[i], err = a.baseObjective(v, centroids[assignment[i]])
			if err != nil {
				return kMeansResult{}, err
			}
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
	return *best, nil
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

func assignmentStep(a kMeansAdapter, vectors []gVector, centroids [][]float64, order, assignment, projected []int) (int, error) {
	changed := 0
	for _, i := range order {
		bestC := 0
		bestScore, err := a.baseObjective(vectors[i], centroids[0])
		if err != nil {
			return 0, err
		}
		for c := 1; c < len(centroids); c++ {
			s, err := a.baseObjective(vectors[i], centroids[c])
			if err != nil {
				return 0, err
			}
			if s < bestScore {
				bestScore, bestC = s, c
			}
		}
		if assignment[i] != bestC {
			assignment[i] = bestC
			changed++
		}
		projected[bestC]++
	}
	return changed, nil
}

func initKMeansPP(a kMeansAdapter, random *splittableRandom, vectors []gVector, k int) ([][]float64, error) {
	n := len(vectors)
	centroids := make([][]float64, 0, k)
	latest := append([]float64(nil), vectors[random.nextInt(n)].data...)
	centroids = append(centroids, latest)
	weights := make([]float64, n)
	total := 0.0
	for i, v := range vectors {
		var err error
		weights[i], err = a.baseObjective(v, latest)
		if err != nil {
			return nil, err
		}
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
		latest = append([]float64(nil), vectors[chosen].data...)
		centroids = append(centroids, latest)
		if len(centroids) < k {
			total = 0
			for i, v := range vectors {
				d, err := a.baseObjective(v, latest)
				if err != nil {
					return nil, err
				}
				if d < weights[i] {
					weights[i] = d
				}
				total += weights[i]
			}
		}
	}
	return centroids, nil
}

func farthestVectorIndex(a kMeansAdapter, vectors []gVector, centroids [][]float64) (int, error) {
	best, bestIdx := -1.0, 0
	for i, v := range vectors {
		m := math.MaxFloat64
		for _, c := range centroids {
			o, err := a.baseObjective(v, c)
			if err != nil {
				return 0, err
			}
			if o < m {
				m = o
			}
		}
		if m > best {
			best, bestIdx = m, i
		}
	}
	return bestIdx, nil
}

// Partition evaluation (PartitionEvaluator).

type partitionDecision int

const (
	decisionKeepCurrent partitionDecision = iota
	decisionAcceptCandidate
	decisionInvalidCandidate
)

type partitionParameters struct {
	codec                 *guardiannVectorCodec
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
		codec: &guardiannVectorCodec{config: guardiannConfig{metric: metric}}, minRelativeSseGain: 0.10, minSeparation: 0.3, maxLowMarginRate: 0.25,
		minChildFraction: 0.015, maxRelativeImbalance: 1.0, lowMarginThreshold: -1.0, alphaSseGain: 1.0,
		betaSeparationGain: 0.5, gammaImbalancePenalty: 1.0, deltaLowMarginPenalty: 0.75, minScoreGain: 0.05,
	}
}

type partition struct {
	centroids   []gVector
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
func evaluatePartitions(currentVectors []gVector, current partition, candidateVectors []gVector, candidate partition, p partitionParameters) (evaluationResult, error) {
	cs, err := evaluatePartition(currentVectors, current, p)
	if err != nil {
		return evaluationResult{}, err
	}
	ks, err := evaluatePartition(candidateVectors, candidate, p)
	if err != nil {
		return evaluationResult{}, err
	}
	relativeSseGain := (cs.sse - ks.sse) / math.Max(cs.sse, 1e-12)
	separationGain := nanToZero(ks.separation) - nanToZero(cs.separation)
	lowMarginPenalty := math.Max(0, nanToZero(ks.lowMarginRate)-nanToZero(cs.lowMarginRate))
	imbalancePenalty := math.Max(0, ks.relativeImbalance()-cs.relativeImbalance())
	scoreGain := p.alphaSseGain*relativeSseGain + p.betaSeparationGain*separationGain -
		p.gammaImbalancePenalty*imbalancePenalty - p.deltaLowMarginPenalty*lowMarginPenalty
	switch {
	case ks.smallestFrac < p.minChildFraction:
		return evaluationResult{decisionInvalidCandidate, scoreGain}, nil
	case ks.relativeImbalance() > p.maxRelativeImbalance:
		return evaluationResult{decisionKeepCurrent, scoreGain}, nil
	case len(candidate.centroids) >= 2 && (math.IsNaN(ks.separation) || ks.separation < p.minSeparation):
		return evaluationResult{decisionKeepCurrent, scoreGain}, nil
	case len(candidate.centroids) >= 2 && ks.lowMarginRate > p.maxLowMarginRate:
		return evaluationResult{decisionKeepCurrent, scoreGain}, nil
	case relativeSseGain < p.minRelativeSseGain:
		return evaluationResult{decisionKeepCurrent, scoreGain}, nil
	case scoreGain < p.minScoreGain:
		return evaluationResult{decisionKeepCurrent, scoreGain}, nil
	}
	return evaluationResult{decisionAcceptCandidate, scoreGain}, nil
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

func evaluatePartition(vectors []gVector, p partition, params partitionParameters) (partitionStats, error) {
	n, k := len(vectors), len(p.centroids)
	distance := params.codec.distance
	needP95 := params.codec.config.metric != VectorMetricCosine && params.lowMarginThreshold <= 0
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
		d, err := distance(v, ownC)
		if err != nil {
			return partitionStats{}, err
		}
		if params.codec.config.metric == VectorMetricCosine {
			sse += 2 * d
		} else {
			sse += l2SquaredSequential(v.data, ownC.data)
		}
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
			margin, err := computeMargin(params.codec, p, v, own)
			if err != nil {
				return partitionStats{}, err
			}
			margins = append(margins, margin)
		}
	}
	overallP95 := math.NaN()
	if p95.Len() > 0 {
		overallP95 = p95[0]
	}
	lowMarginThreshold := params.lowMarginThreshold
	if lowMarginThreshold <= 0 {
		if params.codec.config.metric == VectorMetricCosine {
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
				d, err := distance(p.centroids[i], p.centroids[j])
				if err != nil {
					return partitionStats{}, err
				}
				minDist = math.Min(minDist, d)
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
	}, nil
}

func computeMargin(codec *guardiannVectorCodec, p partition, v gVector, own int) (float64, error) {
	if codec.config.metric == VectorMetricCosine {
		clamped := func(c gVector) float64 { return math.Max(-1, math.Min(1, dotSequential(v.data, c.data))) }
		ownS := clamped(p.centroids[own])
		second := math.Inf(-1)
		for j, c := range p.centroids {
			if j != own {
				second = math.Max(second, clamped(c))
			}
		}
		return ownS - second, nil
	}
	ownD, err := codec.distance(v, p.centroids[own])
	if err != nil {
		return 0, err
	}
	second := math.Inf(1)
	for j, c := range p.centroids {
		if j != own {
			d, err := codec.distance(v, c)
			if err != nil {
				return 0, err
			}
			second = math.Min(second, d)
		}
	}
	return second - ownD, nil
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
