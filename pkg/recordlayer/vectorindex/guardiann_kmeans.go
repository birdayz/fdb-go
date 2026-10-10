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
	if !a.sequentialL2(v) {
		return a.objectiveOf(a.codec.distance(v, gVector{data: c, typ: vectorcodec.TypeDouble}))
	}
	return l2SquaredSequential(v.data, c), nil
}

// objectiveOf is the objective of a distance d: its square but for cosine.
func (a kMeansAdapter) objectiveOf(d float64, err error) (float64, error) {
	if a.codec.config.metric != VectorMetricCosine {
		d *= d
	}
	return d, err
}

// sequentialL2 is whether v's objective is l2SquaredSequential.
func (a kMeansAdapter) sequentialL2(v gVector) bool {
	return a.codec.config.metric != VectorMetricCosine && (a.codec.quantizer == nil || v.typ != rabitq.TypeByte)
}

// plainCosine is whether every objective is javaMetricDistance's cosine,
// which squared norms computed once reproduce exactly.
func (a kMeansAdapter) plainCosine() bool {
	return a.codec.config.metric == VectorMetricCosine && a.codec.quantizer == nil
}

// cosineFromDot is javaMetricDistance's cosine from its three dot products.
func cosineFromDot(dot, na, nb float64) float64 {
	if na == 0 || nb == 0 {
		return math.Inf(1)
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
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
		return cosineFromDot(dotSequential(a, b), dotSequential(a, a), dotSequential(b, b))
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

// l2SquaredSequentialPair is l2SquaredSequential of v against c0 and c1: two
// sums in their own order, interleaved so their add latencies overlap.
// Kept out of line: inlined into the assignment loop, its sums spill to memory.
//
//go:noinline
func l2SquaredSequentialPair(v, c0, c1 []float64) (s0, s1 float64) {
	if len(c0) < len(v) || len(c1) < len(v) {
		panic("l2SquaredSequentialPair: centroid shorter than vector")
	}
	c0, c1 = c0[:len(v)], c1[:len(v)]
	for i, x := range v {
		d0 := x - c0[i]
		d1 := x - c1[i]
		s0 += d0 * d0
		s1 += d1 * d1
	}
	return s0, s1
}

// l2SquaredSequentialQuad is l2SquaredSequentialPair of v0 and of v1: four
// interleaved sums.
//
//go:noinline
func l2SquaredSequentialQuad(v0, v1, c0, c1 []float64) (s00, s01, s10, s11 float64) {
	n := len(v0)
	if len(v1) != n || len(c0) < n || len(c1) < n {
		panic("l2SquaredSequentialQuad: lengths differ")
	}
	v1, c0, c1 = v1[:n], c0[:n], c1[:n]
	for i, x := range v0 {
		y := v1[i]
		d00, d01 := x-c0[i], x-c1[i]
		d10, d11 := y-c0[i], y-c1[i]
		s00 += d00 * d00
		s01 += d01 * d01
		s10 += d10 * d10
		s11 += d11 * d11
	}
	return s00, s01, s10, s11
}

// l2SquaredSequentialFour is (c - v)² summed for four vectors at once, which
// rounds exactly as l2SquaredSequential(v, c).
//
//go:noinline
func l2SquaredSequentialFour(c, v0, v1, v2, v3 []float64) (s0, s1, s2, s3 float64) {
	n := len(c)
	if len(v0) != n || len(v1) != n || len(v2) != n || len(v3) != n {
		panic("l2SquaredSequentialFour: lengths differ")
	}
	v0, v1, v2, v3 = v0[:n], v1[:n], v2[:n], v3[:n]
	for i, x := range c {
		d0, d1, d2, d3 := x-v0[i], x-v1[i], x-v2[i], x-v3[i]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	return s0, s1, s2, s3
}

// dotSequentialQuad is dotSequentialPair of v0 and of v1: four interleaved sums.
//
//go:noinline
func dotSequentialQuad(v0, v1, c0, c1 []float64) (s00, s01, s10, s11 float64) {
	n := len(v0)
	if len(v1) != n || len(c0) < n || len(c1) < n {
		panic("dotSequentialQuad: lengths differ")
	}
	v1, c0, c1 = v1[:n], c0[:n], c1[:n]
	for i, x := range v0 {
		y := v1[i]
		s00 += x * c0[i]
		s01 += x * c1[i]
		s10 += y * c0[i]
		s11 += y * c1[i]
	}
	return s00, s01, s10, s11
}

// dotSequentialFour is c·v summed for four vectors at once, which rounds
// exactly as dotSequential(v, c).
//
//go:noinline
func dotSequentialFour(c, v0, v1, v2, v3 []float64) (s0, s1, s2, s3 float64) {
	n := len(c)
	if len(v0) != n || len(v1) != n || len(v2) != n || len(v3) != n {
		panic("dotSequentialFour: lengths differ")
	}
	v0, v1, v2, v3 = v0[:n], v1[:n], v2[:n], v3[:n]
	for i, x := range c {
		s0 += x * v0[i]
		s1 += x * v1[i]
		s2 += x * v2[i]
		s3 += x * v3[i]
	}
	return s0, s1, s2, s3
}

func dotSequential(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// dotSequentialPair is dotSequential of v with c0 and c1, interleaved and kept
// out of line as l2SquaredSequentialPair.
//
//go:noinline
func dotSequentialPair(v, c0, c1 []float64) (s0, s1 float64) {
	if len(c0) < len(v) || len(c1) < len(v) {
		panic("dotSequentialPair: centroid shorter than vector")
	}
	c0, c1 = c0[:len(v)], c1[:len(v)]
	for i, x := range v {
		s0 += x * c0[i]
		s1 += x * c1[i]
	}
	return s0, s1
}

// kMeansFit is KMeans.fit with lambda 0 (GuardiANN's call): k-means++
// initialisation, Lloyd iterations, and the best of maxRestarts+1 runs.
func kMeansFit(random *splittableRandom, codec *guardiannVectorCodec, vectors []gVector, k, maxIterations, maxRestarts int) (kMeansResult, error) {
	return kMeansLloyd(random, codec, vectors, k, maxIterations, maxRestarts, true)
}

// kMeansLloyd is kMeansFit; without stopWhenStable every restart runs all
// maxIterations, the work the peel's admission bounds.
func kMeansLloyd(random *splittableRandom, codec *guardiannVectorCodec, vectors []gVector, k, maxIterations, maxRestarts int, stopWhenStable bool) (kMeansResult, error) {
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
	l := newLloyd(kMeansAdapter{codec: codec}, vectors)
	n := len(vectors)
	dims := len(vectors[0].data)
	if k == 1 {
		centroid := make([]float64, dims)
		for _, v := range vectors {
			addInto(centroid, v.data)
		}
		scale(centroid, 1/float64(n))
		if l.a.meaninglessNorm(centroid) {
			index, err := l.farthest([][]float64{centroid})
			if err != nil {
				return kMeansResult{}, err
			}
			copy(centroid, vectors[index].data)
		} else {
			l.a.renormalize(centroid)
		}
		res := kMeansResult{
			centroids: []gVector{{data: centroid, typ: vectorcodec.TypeDouble}}, clusterSizes: []int{n},
			assignment: make([]int, n), distances: make([]float64, n),
		}
		if err := l.objectivesTo(centroid, res.distances); err != nil {
			return kMeansResult{}, err
		}
		for _, d := range res.distances {
			res.objective += d
		}
		return res, nil
	}
	next := make([][]float64, k)
	for c := range next {
		next[c] = make([]float64, dims)
	}
	var best *kMeansResult
	for r := 0; r <= maxRestarts; r++ {
		centroids, toFirst, err := l.initKMeansPP(random, k)
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
			for c := range next {
				clear(next[c])
			}
			changed, err := l.assign(centroids, toFirst, assignment, projected, next, nil)
			if err != nil {
				return kMeansResult{}, err
			}
			toFirst = nil
			copy(sizes, projected)
			if changed == 0 && stopWhenStable {
				break
			}
			for c := 0; c < k; c++ {
				if sizes[c] == 0 {
					index, err := l.farthest(centroids)
					if err != nil {
						return kMeansResult{}, err
					}
					copy(next[c], vectors[index].data)
					sizes[c] = 1
					continue
				}
				scale(next[c], 1/float64(sizes[c]))
				if l.a.meaninglessNorm(next[c]) {
					index, err := l.farthest(centroids)
					if err != nil {
						return kMeansResult{}, err
					}
					copy(next[c], vectors[index].data)
				} else {
					l.a.renormalize(next[c])
				}
			}
			centroids, next = next, centroids
		}
		projected := make([]int, k)
		cand := kMeansResult{distances: make([]float64, n)}
		if _, err = l.assign(centroids, nil, assignment, projected, nil, cand.distances); err != nil {
			return kMeansResult{}, err
		}
		cand.clusterSizes, cand.assignment = projected, append([]int(nil), assignment...)
		for _, d := range cand.distances {
			cand.objective += d
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

// Kept out of line, as l2SquaredSequentialPair; the loop around it runs faster.
//
//go:noinline
func addInto(dst, v []float64) {
	if len(v) < len(dst) {
		panic("addInto: vector shorter than sum")
	}
	v = v[:len(dst)]
	for i := range dst {
		dst[i] += v[i]
	}
}

func scale(v []float64, f float64) {
	for i := range v {
		v[i] *= f
	}
}

// objKind is how a vector's objective is computed: by a kernel, or else by
// baseObjective.
type objKind uint8

const (
	objGeneral objKind = iota
	objL2              // l2SquaredSequential
	objCosine          // javaMetricDistance's cosine, from the vector's squared norm
	objCode            // the RaBitQ estimate, from the vector's decoded code
)

// lloyd is a fit's vectors with what their objectives reuse: each vector's
// kind and, for plain cosine, its squared norm.
type lloyd struct {
	a       kMeansAdapter
	vectors []gVector
	kinds   []objKind
	norms   []float64
	scratch []float64
}

func newLloyd(a kMeansAdapter, vectors []gVector) *lloyd {
	l := &lloyd{a: a, vectors: vectors, kinds: make([]objKind, len(vectors))}
	if a.plainCosine() {
		l.norms = make([]float64, len(vectors))
	}
	for i, v := range vectors {
		switch {
		case l.norms != nil:
			l.kinds[i], l.norms[i] = objCosine, dotSequential(v.data, v.data)
		case a.codec.quantizer != nil && v.typ == rabitq.TypeByte && v.code != nil:
			l.kinds[i] = objCode
		case a.sequentialL2(v):
			l.kinds[i] = objL2
		}
	}
	return l
}

// target is what objectives against one centroid share.
type target struct {
	c      []float64
	norm   float64        // its squared norm, for cosine
	scorer *rabitq.Scorer // its RaBitQ scorer
}

func (l *lloyd) target(c []float64) target {
	t := target{c: c}
	if l.norms != nil {
		t.norm = dotSequential(c, c)
	}
	if l.a.codec.quantizer != nil {
		t.scorer = l.a.codec.quantizer.NewScorer(c)
	}
	return t
}

// codeObjective is the objective of a code whose Dot with the scorer's query is dot.
func (l *lloyd) codeObjective(sc *rabitq.Scorer, code *rabitq.Code, dot float64) (float64, error) {
	return l.a.objectiveOf(l.a.codec.fromEstimate(sc.Finish(code, dot)))
}

// objective is baseObjective(vectors[i], t.c).
func (l *lloyd) objective(i int, t target) (float64, error) {
	v := l.vectors[i]
	switch l.kinds[i] {
	case objL2:
		return l2SquaredSequential(v.data, t.c), nil
	case objCosine:
		return cosineFromDot(dotSequential(v.data, t.c), l.norms[i], t.norm), nil
	case objCode:
		return l.codeObjective(t.scorer, v.code, v.code.Dot(t.c))
	}
	return l.a.baseObjective(v, t.c)
}

// objectivesTo writes every vector's objective against c into out, four
// vectors per kernel pass where their kinds allow. Every objective error is
// notFiniteDistance, so the order passes run in never changes a fit's error.
func (l *lloyd) objectivesTo(c []float64, out []float64) error {
	t := l.target(c)
	vs := l.vectors
	for i := 0; i < len(vs); {
		kind := l.kinds[i]
		if kind == objGeneral || i+4 > len(vs) || l.kinds[i+1] != kind || l.kinds[i+2] != kind || l.kinds[i+3] != kind {
			var err error
			if out[i], err = l.objective(i, t); err != nil {
				return err
			}
			i++
			continue
		}
		v, o := vs[i:i+4], out[i:i+4]
		switch kind {
		case objL2:
			o[0], o[1], o[2], o[3] = l2SquaredSequentialFour(c, v[0].data, v[1].data, v[2].data, v[3].data)
		case objCosine:
			d0, d1, d2, d3 := dotSequentialFour(c, v[0].data, v[1].data, v[2].data, v[3].data)
			for j, d := range [4]float64{d0, d1, d2, d3} {
				o[j] = cosineFromDot(d, l.norms[i+j], t.norm)
			}
		case objCode:
			d0, d1, d2, d3 := rabitq.DotFour(v[0].code, v[1].code, v[2].code, v[3].code, c)
			for j, d := range [4]float64{d0, d1, d2, d3} {
				var err error
				if o[j], err = l.codeObjective(t.scorer, v[j].code, d); err != nil {
					return err
				}
			}
		}
		i += 4
	}
	return nil
}

// assign assigns each vector to its nearest centroid, with next also summing
// it into next[c] in vector order and with distances recording it. toFirst,
// when set, holds the vectors' objectives against centroids[0].
func (l *lloyd) assign(centroids [][]float64, toFirst []float64, assignment, projected []int,
	next [][]float64, distances []float64,
) (int, error) {
	k := len(centroids)
	targets := make([]target, k)
	for c := range centroids {
		targets[c] = l.target(centroids[c])
	}
	var toSecond []float64
	if toFirst != nil && k == 2 {
		if cap(l.scratch) < len(l.vectors) {
			l.scratch = make([]float64, len(l.vectors))
		}
		toSecond = l.scratch[:len(l.vectors)]
		if err := l.objectivesTo(centroids[1], toSecond); err != nil {
			return 0, err
		}
	}
	changed := 0
	apply := func(i int, bestC int, bestScore float64) {
		if assignment[i] != bestC {
			assignment[i] = bestC
			changed++
		}
		projected[bestC]++
		if next != nil {
			addInto(next[bestC], l.vectors[i].data)
		}
		if distances != nil {
			distances[i] = bestScore
		}
	}
	nearer := func(i int, s0, s1 float64) {
		if s1 < s0 {
			apply(i, 1, s1)
		} else {
			apply(i, 0, s0)
		}
	}
	vs := l.vectors
	for i := 0; i < len(vs); {
		if k != 2 {
			bestC := 0
			bestScore, err := l.objective(i, targets[0])
			if err != nil {
				return 0, err
			}
			for c := 1; c < k; c++ {
				s, err := l.objective(i, targets[c])
				if err != nil {
					return 0, err
				}
				if s < bestScore {
					bestScore, bestC = s, c
				}
			}
			apply(i, bestC, bestScore)
			i++
			continue
		}
		if toSecond != nil {
			nearer(i, toFirst[i], toSecond[i])
			i++
			continue
		}
		kind := l.kinds[i]
		c0, c1 := centroids[0], centroids[1]
		if kind != objGeneral && i+1 < len(vs) && l.kinds[i+1] == kind {
			// Two vectors against both centroids: four sums per kernel pass.
			var s [4]float64
			switch kind {
			case objL2:
				s[0], s[1], s[2], s[3] = l2SquaredSequentialQuad(vs[i].data, vs[i+1].data, c0, c1)
			case objCosine:
				d00, d01, d10, d11 := dotSequentialQuad(vs[i].data, vs[i+1].data, c0, c1)
				s[0], s[1] = cosineFromDot(d00, l.norms[i], targets[0].norm), cosineFromDot(d01, l.norms[i], targets[1].norm)
				s[2], s[3] = cosineFromDot(d10, l.norms[i+1], targets[0].norm), cosineFromDot(d11, l.norms[i+1], targets[1].norm)
			case objCode:
				d00, d01, d10, d11 := rabitq.DotPairs(vs[i].code, vs[i+1].code, c0, c1)
				for j, d := range [4]float64{d00, d01, d10, d11} {
					var err error
					code := vs[i+j/2].code
					if s[j], err = l.codeObjective(targets[j%2].scorer, code, d); err != nil {
						return 0, err
					}
				}
			}
			nearer(i, s[0], s[1])
			nearer(i+1, s[2], s[3])
			i += 2
			continue
		}
		var s0, s1 float64
		switch kind {
		case objL2:
			s0, s1 = l2SquaredSequentialPair(vs[i].data, c0, c1)
		case objCosine:
			d0, d1 := dotSequentialPair(vs[i].data, c0, c1)
			s0, s1 = cosineFromDot(d0, l.norms[i], targets[0].norm), cosineFromDot(d1, l.norms[i], targets[1].norm)
		default:
			var err error
			if s0, err = l.objective(i, targets[0]); err != nil {
				return 0, err
			}
			if s1, err = l.objective(i, targets[1]); err != nil {
				return 0, err
			}
		}
		nearer(i, s0, s1)
		i++
	}
	return changed, nil
}

// initKMeansPP is k-means++ seeding. It also returns the vectors' objectives
// against the first centroid when they are still exact (k = 2).
func (l *lloyd) initKMeansPP(random *splittableRandom, k int) ([][]float64, []float64, error) {
	vectors := l.vectors
	n := len(vectors)
	centroids := make([][]float64, 0, k)
	latest := append([]float64(nil), vectors[random.nextInt(n)].data...)
	centroids = append(centroids, latest)
	weights := make([]float64, n)
	if err := l.objectivesTo(latest, weights); err != nil {
		return nil, nil, err
	}
	total := 0.0
	for _, w := range weights {
		total += w
	}
	var latestObjectives []float64
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
			if latestObjectives == nil {
				latestObjectives = make([]float64, n)
			}
			if err := l.objectivesTo(latest, latestObjectives); err != nil {
				return nil, nil, err
			}
			total = 0
			for i, d := range latestObjectives {
				if d < weights[i] {
					weights[i] = d
				}
				total += weights[i]
			}
		}
	}
	if k != 2 {
		weights = nil
	}
	return centroids, weights, nil
}

// farthest is the index of the vector farthest from its nearest centroid.
func (l *lloyd) farthest(centroids [][]float64) (int, error) {
	nearest := make([]float64, len(l.vectors))
	for i := range nearest {
		nearest[i] = math.MaxFloat64
	}
	objectives := make([]float64, len(l.vectors))
	for _, centroid := range centroids {
		if err := l.objectivesTo(centroid, objectives); err != nil {
			return 0, err
		}
		for i, o := range objectives {
			if o < nearest[i] {
				nearest[i] = o
			}
		}
	}
	best, bestIdx := -1.0, 0
	for i, m := range nearest {
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
