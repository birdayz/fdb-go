package vectorindex

import (
	"math"
	"math/rand"
	"reflect"
	"testing"

	"fdb.dev/pkg/rabitq"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// The fit kMeansFit replaced with its pass kernels, kept verbatim (with the
// codec distance it called) as the reference the kernels must reproduce bit
// for bit: same objective, assignment, sizes, centroids, distances and errors.

func baseObjectiveReference(a kMeansAdapter, v gVector, c []float64) (float64, error) {
	if a.codec.config.metric == VectorMetricCosine || a.codec.quantizer != nil && v.typ == rabitq.TypeByte {
		d, err := distanceReference(a.codec, v, gVector{data: c, typ: vectorcodec.TypeDouble})
		if a.codec.config.metric != VectorMetricCosine {
			d *= d
		}
		return d, err
	}
	return l2SquaredSequential(v.data, c), nil
}

func javaMetricDistanceReference(a, b []float64, metric VectorMetric) float64 {
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

func kMeansFitReference(random *splittableRandom, codec *guardiannVectorCodec, vectors []gVector, k, maxIterations, maxRestarts int) (kMeansResult, error) {
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
			index, err := farthestVectorIndexReference(a, vectors, [][]float64{centroid})
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
			res.distances[i], err = baseObjectiveReference(a, v, centroid)
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
		centroids, err := initKMeansPPReference(a, random, vectors, k)
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
			changed, err := assignmentStepReference(a, vectors, centroids, order, assignment, projected)
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
					index, err := farthestVectorIndexReference(a, vectors, centroids)
					if err != nil {
						return kMeansResult{}, err
					}
					copy(next[c], vectors[index].data)
					sizes[c] = 1
					continue
				}
				scale(next[c], 1/float64(sizes[c]))
				if a.meaninglessNorm(next[c]) {
					index, err := farthestVectorIndexReference(a, vectors, centroids)
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
		if _, err = assignmentStepReference(a, vectors, centroids, order, assignment, projected); err != nil {
			return kMeansResult{}, err
		}
		copy(sizes, projected)
		cand := kMeansResult{
			clusterSizes: append([]int(nil), sizes...), assignment: append([]int(nil), assignment...),
			distances: make([]float64, n),
		}
		for i, v := range vectors {
			cand.distances[i], err = baseObjectiveReference(a, v, centroids[assignment[i]])
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

func assignmentStepReference(a kMeansAdapter, vectors []gVector, centroids [][]float64, order, assignment, projected []int) (int, error) {
	changed := 0
	for _, i := range order {
		bestC := 0
		bestScore, err := baseObjectiveReference(a, vectors[i], centroids[0])
		if err != nil {
			return 0, err
		}
		for c := 1; c < len(centroids); c++ {
			s, err := baseObjectiveReference(a, vectors[i], centroids[c])
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

func initKMeansPPReference(a kMeansAdapter, random *splittableRandom, vectors []gVector, k int) ([][]float64, error) {
	n := len(vectors)
	centroids := make([][]float64, 0, k)
	latest := append([]float64(nil), vectors[random.nextInt(n)].data...)
	centroids = append(centroids, latest)
	weights := make([]float64, n)
	total := 0.0
	for i, v := range vectors {
		var err error
		weights[i], err = baseObjectiveReference(a, v, latest)
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
				d, err := baseObjectiveReference(a, v, latest)
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

func farthestVectorIndexReference(a kMeansAdapter, vectors []gVector, centroids [][]float64) (int, error) {
	best, bestIdx := -1.0, 0
	for i, v := range vectors {
		m := math.MaxFloat64
		for _, c := range centroids {
			o, err := baseObjectiveReference(a, v, c)
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

func distanceReference(c *guardiannVectorCodec, a, b gVector) (float64, error) {
	d := javaMetricDistanceReference(a.data, b.data, c.config.metric)
	if c.quantizer != nil {
		var err error
		switch {
		case a.typ != rabitq.TypeByte && b.typ == rabitq.TypeByte:
			d, err = c.quantizer.Distance(a.data, b.encoded, c.config.numDimensions)
		case a.typ == rabitq.TypeByte && b.typ != rabitq.TypeByte:
			d, err = c.quantizer.Distance(b.data, a.encoded, c.config.numDimensions)
		}
		if err == nil && (a.typ == rabitq.TypeByte) != (b.typ == rabitq.TypeByte) {
			switch c.config.metric {
			case VectorMetricEuclidean:
				d = math.Sqrt(math.Max(0, d))
			case VectorMetricEuclideanSquare:
				d = math.Max(0, d)
			}
		}
		if err != nil || math.IsNaN(d) || math.IsInf(d, 0) {
			return 0, &recordlayer.IllegalArgumentError{Message: "distance is infinite or not a number"}
		}
	}
	return d, nil
}

// The kernel fit equals the reference over odd vector counts (the kernels'
// leftovers), every metric, plain, mixed and all-RaBitQ vectors with a zero
// vector among them, k = 1..4 and the default and many-restart knobs.
func TestKMeansFitMatchesReference(t *testing.T) {
	t.Parallel()
	rnd := rand.New(rand.NewSource(9))
	for _, shape := range [][2]int{{41, 3}, {37, 64}, {23, 131}} {
		raw := make([][]float64, shape[0])
		for i := range raw {
			v := make([]float64, shape[1])
			for j := range v {
				v[j] = float64(i%5)*0.3 + rnd.Float64()*0.01
				if i%17 == 0 {
					v[j%shape[1]] += 50
				}
			}
			if i == 7 {
				clear(v)
			}
			raw[i] = v
		}
		for _, metric := range []VectorMetric{VectorMetricEuclidean, VectorMetricCosine, VectorMetricEuclideanSquare} {
			for _, encoded := range []string{"none", "mixed", "all"} {
				cfg := defaultGuardiannConfig(shape[1])
				cfg.metric, cfg.useRaBitQ = metric, encoded != "none"
				codec := &guardiannVectorCodec{config: cfg}
				if cfg.useRaBitQ {
					codec = newGuardiannVectorCodec(cfg, &guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: make([]float64, shape[1])})
				}
				vectors := make([]gVector, len(raw))
				for i, v := range raw {
					half, err := vectorcodec.Deserialize(vectorcodec.SerializeHalf(v))
					if err != nil {
						t.Fatal(err)
					}
					vectors[i] = gVector{data: half, typ: vectorcodec.TypeHalf}
					if i != 7 && (encoded == "all" || encoded == "mixed" && i%3 != 1) {
						if vectors[i], err = codec.decode(codec.encode(vectors[i])); err != nil {
							t.Fatal(err)
						}
					}
				}
				for _, knobs := range [][2]int{{8, 3}, {1, 31}} {
					for k := 1; k <= 4; k++ {
						want, werr := kMeansFitReference(&splittableRandom{seed: 3, gamma: goldenGamma}, codec, vectors, k, knobs[0], knobs[1])
						got, gerr := kMeansFit(&splittableRandom{seed: 3, gamma: goldenGamma}, codec, vectors, k, knobs[0], knobs[1])
						if (werr == nil) != (gerr == nil) || werr != nil && werr.Error() != gerr.Error() {
							t.Fatalf("n=%d d=%d metric %v %s knobs %v k=%d: error %v, want %v", shape[0], shape[1], metric, encoded, knobs, k, gerr, werr)
						}
						if werr == nil && !reflect.DeepEqual(got, want) {
							t.Fatalf("n=%d d=%d metric %v %s knobs %v k=%d: objective %v, want %v", shape[0], shape[1], metric, encoded, knobs, k, got.objective, want.objective)
						}
					}
				}
			}
		}
	}
}

// Each multi-sum kernel's sums are the scalar sums, bit for bit.
func TestPassKernelsMatchScalar(t *testing.T) {
	t.Parallel()
	rnd := rand.New(rand.NewSource(5))
	vec := func(d int) []float64 {
		v := make([]float64, d)
		for i := range v {
			v[i] = rnd.NormFloat64() * math.Pow(10, float64(rnd.Intn(7)-3))
		}
		return v
	}
	for trial := 0; trial < 200; trial++ {
		d := 1 + rnd.Intn(300)
		v, c := [4][]float64{vec(d), vec(d), vec(d), vec(d)}, [2][]float64{vec(d), vec(d)}
		check := func(name string, got, want float64) {
			t.Helper()
			if got != want {
				t.Fatalf("d=%d %s: %v, want %v", d, name, got, want)
			}
		}
		a, b := l2SquaredSequentialPair(v[0], c[0], c[1])
		check("l2 pair 0", a, l2SquaredSequential(v[0], c[0]))
		check("l2 pair 1", b, l2SquaredSequential(v[0], c[1]))
		s00, s01, s10, s11 := l2SquaredSequentialQuad(v[0], v[1], c[0], c[1])
		for j, s := range [4]float64{s00, s01, s10, s11} {
			check("l2 quad", s, l2SquaredSequential(v[j/2], c[j%2]))
		}
		f0, f1, f2, f3 := l2SquaredSequentialFour(c[0], v[0], v[1], v[2], v[3])
		for j, s := range [4]float64{f0, f1, f2, f3} {
			check("l2 four", s, l2SquaredSequential(v[j], c[0]))
		}
		a, b = dotSequentialPair(v[0], c[0], c[1])
		check("dot pair 0", a, dotSequential(v[0], c[0]))
		check("dot pair 1", b, dotSequential(v[0], c[1]))
		s00, s01, s10, s11 = dotSequentialQuad(v[0], v[1], c[0], c[1])
		for j, s := range [4]float64{s00, s01, s10, s11} {
			check("dot quad", s, dotSequential(v[j/2], c[j%2]))
		}
		f0, f1, f2, f3 = dotSequentialFour(c[0], v[0], v[1], v[2], v[3])
		for j, s := range [4]float64{f0, f1, f2, f3} {
			check("dot four", s, dotSequential(v[j], c[0]))
		}
	}
}
