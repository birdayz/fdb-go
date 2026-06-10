package rabitq

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"math"
)

// TypeByte is the wire ordinal for RaBitQ-encoded vectors.
// Matches Java's VectorType.RABITQ.ordinal() = 3.
const TypeByte byte = 3

// Metric identifies a distance metric for RaBitQ quantization.
// Values match recordlayer.VectorMetric so that a simple type conversion works.
type Metric int

const (
	MetricEuclidean Metric = iota
	MetricCosine
	MetricInnerProduct
)

// eps is the epsilon used for floor quantization.
const eps = 1e-5

// eps0 is the error scaling constant from the RaBitQ paper.
const eps0 = 1.9

// nEnum controls the sweep range extension for bestRescaleFactor.
const nEnum = 10

// tightStart defines the sweep start fractions per numExBits.
// Index 0 unused; defined up to 8 extra bits (matching Java/C++ source).
var tightStart = [9]float64{
	0.00, 0.15, 0.20, 0.52, 0.59, 0.71, 0.75, 0.77, 0.81,
}

// Quantizer implements recordlayer.VectorQuantizer using RaBitQ.
type Quantizer struct {
	metric    Metric
	numExBits int
}

// NewQuantizer creates a new RaBitQ quantizer implementing VectorQuantizer.
// numExBits is clamped to [1, 8]; out-of-range values default to 4.
func NewQuantizer(metric Metric, numExBits int) *Quantizer {
	if numExBits < 1 || numExBits > 8 {
		numExBits = 4
	}
	return &Quantizer{metric: metric, numExBits: numExBits}
}

// Encode quantizes a float64 vector into compact bytes for storage.
func (q *Quantizer) Encode(vector []float64) []byte {
	rq := NewRaBitQuantizer(q.metric, q.numExBits)
	return rq.Encode(vector).ToBytes()
}

// Distance estimates the distance between a raw query vector and stored
// quantized bytes. Returns the estimated distance.
func (q *Quantizer) Distance(query []float64, storedBytes []byte, numDimensions int) (float64, error) {
	encoded, err := EncodedVectorFromBytes(storedBytes, numDimensions, q.numExBits)
	if err != nil {
		return 0, err
	}
	est := NewRaBitEstimator(q.metric, q.numExBits)
	return est.Distance(query, encoded)
}

// Decode reconstructs an approximate float64 vector from stored quantized bytes.
// Used for pairwise distance in the neighbor selection heuristic.
func (q *Quantizer) Decode(storedBytes []byte, numDimensions int) ([]float64, error) {
	encoded, err := EncodedVectorFromBytes(storedBytes, numDimensions, q.numExBits)
	if err != nil {
		return nil, err
	}

	// Reconstruct approximate vector: un-center the quantized codes.
	cb := float64(int(1)<<q.numExBits) - 0.5
	dims := encoded.NumDimensions()
	xuc := make([]float64, dims)
	var xucNormSqr float64
	for i := 0; i < dims; i++ {
		xuc[i] = float64(encoded.Encoded[i]) - cb
		xucNormSqr += xuc[i] * xuc[i]
	}

	// Scale to approximate original norm (sqrt(fAddEx) = ||original||).
	origNorm := math.Sqrt(encoded.FAddEx)
	xucNorm := math.Sqrt(xucNormSqr)
	if xucNorm > 0 && origNorm > 0 {
		scale := origNorm / xucNorm
		for i := range xuc {
			xuc[i] *= scale
		}
	}

	return xuc, nil
}

// GetTypeByte returns the type ordinal byte used as the first byte of encoded data.
func (q *Quantizer) GetTypeByte() byte {
	return TypeByte
}

// EncodedVector is the quantized representation of a real vector using RaBitQ.
// Wire-compatible with Java's EncodedRealVector.
type EncodedVector struct {
	// Encoded stores one quantized level per dimension.
	// Each value is in [0, 2^(numExBits+1) - 1].
	// Matches Java's int[] encoded field.
	Encoded []int

	// FAddEx is the precomputed additive factor (||residual||^2 for Euclidean metrics).
	FAddEx float64

	// FRescaleEx is the precomputed rescale factor for the dot product term.
	FRescaleEx float64

	// FErrorEx is the precomputed error bound scaling factor.
	FErrorEx float64

	// NumExBits is the number of extra bits used for quantization (1-8).
	NumExBits int
}

// NumDimensions returns the number of dimensions of the encoded vector.
func (e *EncodedVector) NumDimensions() int {
	return len(e.Encoded)
}

// ToBytes serializes the encoded vector to bytes, wire-compatible with Java's
// EncodedRealVector.getRawData(). Format:
//
//	[1 byte: type ordinal 3 = RABITQ]
//	[8 bytes: fAddEx as big-endian float64]
//	[8 bytes: fRescaleEx as big-endian float64]
//	[8 bytes: fErrorEx as big-endian float64]
//	[remaining: bit-packed encoded components, big-endian]
//
// Each component is packed in (numExBits+1) bits, big-endian bit order.
func (e *EncodedVector) ToBytes() []byte {
	numDims := e.NumDimensions()
	bitsPerComponent := e.NumExBits + 1
	numBits := numDims * bitsPerComponent
	// Header: 1 (type) + 8 (fAddEx) + 8 (fRescaleEx) + 8 (fErrorEx) = 25 bytes.
	packedLen := (numBits + 7) / 8
	length := 25 + packedLen
	buf := make([]byte, length)

	buf[0] = TypeByte
	binary.BigEndian.PutUint64(buf[1:], math.Float64bits(e.FAddEx))
	binary.BigEndian.PutUint64(buf[9:], math.Float64bits(e.FRescaleEx))
	binary.BigEndian.PutUint64(buf[17:], math.Float64bits(e.FErrorEx))

	packEncodedComponents(e.Encoded, bitsPerComponent, buf[25:])
	return buf
}

// packEncodedComponents packs encoded component values into a byte slice,
// big-endian bit order. Matches Java's EncodedRealVector.packEncodedComponents().
func packEncodedComponents(encoded []int, bitsPerComponent int, dst []byte) {
	remainingBitsInByte := 8
	var currentByte byte
	pos := 0

	for i := 0; i < len(encoded); i++ {
		component := encoded[i]
		remainingBitsInComponent := bitsPerComponent

		for remainingBitsInComponent > 0 {
			remainingMask := (1 << remainingBitsInComponent) - 1
			remainingComponent := component & remainingMask

			if remainingBitsInComponent <= remainingBitsInByte {
				currentByte |= byte(remainingComponent << (remainingBitsInByte - remainingBitsInComponent))
				remainingBitsInByte -= remainingBitsInComponent
				if remainingBitsInByte == 0 {
					remainingBitsInByte = 8
					dst[pos] = currentByte
					pos++
					currentByte = 0
				}
				break
			}

			// remainingBitsInComponent > remainingBitsInByte
			currentByte |= byte(remainingComponent >> (remainingBitsInComponent - remainingBitsInByte))
			remainingBitsInComponent -= remainingBitsInByte
			remainingBitsInByte = 8
			dst[pos] = currentByte
			pos++
			currentByte = 0
		}
	}

	if remainingBitsInByte < 8 {
		dst[pos] = currentByte
	}
}

// EncodedVectorFromBytes deserializes an EncodedVector from bytes.
// Wire-compatible with Java's EncodedRealVector.fromBytes().
func EncodedVectorFromBytes(data []byte, numDimensions, numExBits int) (*EncodedVector, error) {
	if len(data) < 25 {
		return nil, fmt.Errorf("rabitq: data too short: %d bytes", len(data))
	}
	if data[0] != TypeByte {
		return nil, fmt.Errorf("rabitq: expected type ordinal %d, got %d", TypeByte, data[0])
	}

	fAddEx := math.Float64frombits(binary.BigEndian.Uint64(data[1:]))
	fRescaleEx := math.Float64frombits(binary.BigEndian.Uint64(data[9:]))
	fErrorEx := math.Float64frombits(binary.BigEndian.Uint64(data[17:]))

	components, err := unpackComponents(data[25:], numDimensions, numExBits)
	if err != nil {
		return nil, err
	}
	return &EncodedVector{
		Encoded:    components,
		FAddEx:     fAddEx,
		FRescaleEx: fRescaleEx,
		FErrorEx:   fErrorEx,
		NumExBits:  numExBits,
	}, nil
}

// unpackComponents unpacks bit-packed encoded components from a byte slice.
// Matches Java's EncodedRealVector.unpackComponents().
func unpackComponents(data []byte, numDimensions, numExBits int) ([]int, error) {
	result := make([]int, numDimensions)
	if err := unpackComponentsInto(data, result, numExBits); err != nil {
		return nil, err
	}
	return result, nil
}

// unpackComponentsInto unpacks into a caller-provided buffer (len = dims) —
// the allocation-free path the per-query Scorer reuses across codes.
func unpackComponentsInto(data []byte, result []int, numExBits int) error {
	numDimensions := len(result)
	// Validate packed data length to avoid panics on truncated data.
	bitsPerComponent := numExBits + 1
	totalBits := numDimensions * bitsPerComponent
	expectedBytes := (totalBits + 7) / 8
	if len(data) < expectedBytes {
		return fmt.Errorf("rabitq: truncated encoded vector data: got %d bytes, need %d", len(data), expectedBytes)
	}
	// The bit loop ACCUMULATES via |= — the buffer must start zeroed, and a
	// reused Scorer buffer arrives dirty (re-scoring the SAME code is
	// idempotent under OR, which is exactly how the original differential
	// test missed this; different codes through one scorer corrupted every
	// estimate after the first).
	for i := range result {
		result[i] = 0
	}
	remainingBitsInByte := 8
	pos := 0
	currentByte := data[pos]

	for i := 0; i < numDimensions; i++ {
		remainingBitsForComponent := bitsPerComponent

		for remainingBitsForComponent > 0 {
			mask := (1 << remainingBitsInByte) - 1
			maskedByte := int(currentByte) & mask

			if remainingBitsForComponent <= remainingBitsInByte {
				result[i] |= maskedByte >> (remainingBitsInByte - remainingBitsForComponent)
				remainingBitsInByte -= remainingBitsForComponent
				if remainingBitsInByte == 0 {
					remainingBitsInByte = 8
					if i+1 < numDimensions {
						pos++
						currentByte = data[pos]
					}
				}
				break
			}

			// remainingBitsForComponent > remainingBitsInByte
			result[i] |= maskedByte << (remainingBitsForComponent - remainingBitsInByte)
			remainingBitsForComponent -= remainingBitsInByte
			remainingBitsInByte = 8
			pos++
			currentByte = data[pos]
		}
	}
	return nil
}

// --- RaBitQ Quantizer ---

// RaBitQuantizer implements the RaBitQ quantization scheme for compressing
// high-dimensional vectors into compact integer-based representations.
// Matches Java's RaBitQuantizer.
type RaBitQuantizer struct {
	NumExBits int
	Metric    Metric
}

// NewRaBitQuantizer creates a new quantizer with the given metric and bit precision.
// numExBits must be in [1, 8].
func NewRaBitQuantizer(metric Metric, numExBits int) *RaBitQuantizer {
	if numExBits < 1 || numExBits > 8 {
		panic(fmt.Sprintf("rabitq: numExBits must be in [1, 8], got %d", numExBits))
	}
	return &RaBitQuantizer{
		NumExBits: numExBits,
		Metric:    metric,
	}
}

// Encode quantizes a real-valued vector into an EncodedVector.
// Matches Java's RaBitQuantizer.encode().
func (q *RaBitQuantizer) Encode(vec []float64) *EncodedVector {
	return q.encodeInternal(vec).encodedVector
}

// rabitqResult holds intermediate values from the encoding process.
type rabitqResult struct {
	encodedVector *EncodedVector
	t             float64 // chosen rescale factor
	ipNormInv     float64 // 1 / sum_i((k_i + 0.5) * oAbs[i])
}

// quantizeExResult holds the per-dimension codes and intermediate values.
type quantizeExResult struct {
	code      []int
	t         float64
	ipNormInv float64
}

// encodeInternal performs the core encoding logic.
// Matches Java's RaBitQuantizer.encodeInternal().
func (q *RaBitQuantizer) encodeInternal(data []float64) *rabitqResult {
	dims := len(data)

	base := q.exBitsCode(data)
	signedCode := base.code
	ipInv := base.ipNormInv

	totalCode := make([]int, dims)
	for i := 0; i < dims; i++ {
		sgn := 0
		if data[i] >= 0.0 {
			sgn = 1
		}
		totalCode[i] = signedCode[i] + (sgn << q.NumExBits)
	}

	cb := -(float64(int(1)<<q.NumExBits) - 0.5)
	xuCb := make([]float64, dims)
	for i := 0; i < dims; i++ {
		xuCb[i] = float64(totalCode[i]) + cb
	}

	residualL2Sqr := dot(data, data)
	residualL2Norm := math.Sqrt(residualL2Sqr)
	ipResidualXuCb := dot(data, xuCb)

	xuCbNormSqr := dot(xuCb, xuCb)

	ipResidualXuCbSafe := ipResidualXuCb
	if ipResidualXuCb == 0.0 {
		ipResidualXuCbSafe = math.Inf(1)
	}

	// Clamp to 0 to handle floating-point rounding where Cauchy-Schwarz
	// ratio is slightly < 1.0, making the expression negative.
	sqrtArg := ((residualL2Sqr*xuCbNormSqr)/
		(ipResidualXuCbSafe*ipResidualXuCbSafe) - 1.0) /
		float64(max(1, dims-1))
	tmpError := residualL2Norm * eps0 * math.Sqrt(math.Max(0.0, sqrtArg))

	// All supported metrics use the same formula (matching Java switch).
	fAddEx := residualL2Sqr
	fRescaleEx := ipInv * (-2.0 * residualL2Norm)
	fErrorEx := 2.0 * tmpError

	return &rabitqResult{
		encodedVector: &EncodedVector{
			Encoded:    totalCode,
			FAddEx:     fAddEx,
			FRescaleEx: fRescaleEx,
			FErrorEx:   fErrorEx,
			NumExBits:  q.NumExBits,
		},
		t:         base.t,
		ipNormInv: ipInv,
	}
}

// exBitsCode builds per-dimension extra-bit code using the best t found by
// bestRescaleFactor and returns the signed code, t, and ipNormInv.
// Matches Java's RaBitQuantizer.exBitsCode().
func (q *RaBitQuantizer) exBitsCode(residual []float64) *quantizeExResult {
	dims := len(residual)

	oAbs := absOfNormalized(residual)
	qr := q.quantizeEx(oAbs)

	k := qr.code
	mask := (1 << q.NumExBits) - 1

	signed := make([]int, dims)
	for j := 0; j < dims; j++ {
		if residual[j] < 0 {
			signed[j] = (^k[j]) & mask
		} else {
			signed[j] = k[j]
		}
	}

	return &quantizeExResult{code: signed, t: qr.t, ipNormInv: qr.ipNormInv}
}

// quantizeEx quantizes a vector of absolute values.
// Matches Java's RaBitQuantizer.quantizeEx().
func (q *RaBitQuantizer) quantizeEx(oAbs []float64) *quantizeExResult {
	dim := len(oAbs)
	maxLevel := (1 << q.NumExBits) - 1

	t := q.bestRescaleFactor(oAbs)

	var ipNorm float64
	code := make([]int, dim)
	for i := 0; i < dim; i++ {
		k := int(math.Floor(t*oAbs[i] + eps))
		if k > maxLevel {
			k = maxLevel
		}
		code[i] = k
		ipNorm += (float64(k) + 0.5) * oAbs[i]
	}

	var ipNormInv float64
	if ipNorm > 0.0 && !math.IsInf(ipNorm, 0) && !math.IsNaN(ipNorm) {
		ipNormInv = 1.0 / ipNorm
		if math.IsInf(ipNormInv, 0) || ipNormInv == 0.0 {
			ipNormInv = 1.0
		}
	} else {
		ipNormInv = 1.0
	}

	return &quantizeExResult{code: code, t: t, ipNormInv: ipNormInv}
}

// rescaleNode is a min-heap entry for the bestRescaleFactor sweep.
type rescaleNode struct {
	t   float64
	idx int
}

// rescaleHeap is a min-heap of rescaleNodes ordered by t.
type rescaleHeap []rescaleNode

func (h rescaleHeap) Len() int           { return len(h) }
func (h rescaleHeap) Less(i, j int) bool { return h[i].t < h[j].t }
func (h rescaleHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *rescaleHeap) Push(x any)        { *h = append(*h, x.(rescaleNode)) }
func (h *rescaleHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// bestRescaleFactor finds the optimal rescaling factor t for quantization.
// Uses a priority-queue sweep over critical t values where floor(t * oAbs[i])
// changes. Matches Java's RaBitQuantizer.bestRescaleFactor().
func (q *RaBitQuantizer) bestRescaleFactor(oAbs []float64) float64 {
	numDimensions := len(oAbs)

	var maxO float64
	for _, v := range oAbs {
		if v > maxO {
			maxO = v
		}
	}
	if maxO <= 0.0 {
		return 0.0
	}

	maxLevel := (1 << q.NumExBits) - 1
	tEnd := float64(maxLevel+nEnum) / maxO
	tStart := tEnd * tightStart[q.NumExBits]

	curOB := make([]int, numDimensions)
	sqrDen := float64(numDimensions) * 0.25
	var numer float64

	for i := 0; i < numDimensions; i++ {
		cur := int(tStart*oAbs[i] + eps)
		curOB[i] = cur
		sqrDen += float64(cur)*float64(cur) + float64(cur)
		numer += (float64(cur) + 0.5) * oAbs[i]
	}

	pq := &rescaleHeap{}
	heap.Init(pq)
	for i := 0; i < numDimensions; i++ {
		if oAbs[i] > 0.0 {
			tNext := float64(curOB[i]+1) / oAbs[i]
			heap.Push(pq, rescaleNode{t: tNext, idx: i})
		}
	}

	var maxIp float64
	var bestT float64

	for pq.Len() > 0 {
		node := heap.Pop(pq).(rescaleNode)
		curT := node.t
		i := node.idx

		curOB[i]++
		u := curOB[i]

		sqrDen += 2.0 * float64(u)
		numer += oAbs[i]

		curIp := numer / math.Sqrt(sqrDen)
		if curIp > maxIp {
			maxIp = curIp
			bestT = curT
		}

		if u < maxLevel {
			tNext := float64(u+1) / oAbs[i]
			if tNext < tEnd {
				heap.Push(pq, rescaleNode{t: tNext, idx: i})
			}
		}
	}

	return bestT
}

// absOfNormalized computes the element-wise absolute values of the L2-normalized
// input vector. Matches Java's RaBitQuantizer.absOfNormalized().
func absOfNormalized(x []float64) []float64 {
	y := make([]float64, len(x))
	n := l2Norm(x)
	if n == 0.0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return y
	}
	inv := 1.0 / n
	for i := 0; i < len(x); i++ {
		y[i] = math.Abs(x[i] * inv)
	}
	return y
}

// l2Norm computes the L2 norm of a vector.
func l2Norm(x []float64) float64 {
	return math.Sqrt(dot(x, x))
}

// dot computes the dot product of two vectors.
func dot(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += a[i] * b[i]
	}
	return sum
}

// --- RaBitQ Estimator ---

// RaBitEstimator estimates distance between a raw query vector and an encoded vector.
// Matches Java's RaBitEstimator.
type RaBitEstimator struct {
	Metric    Metric
	NumExBits int
}

// NewRaBitEstimator creates a new estimator with the given metric and precision.
func NewRaBitEstimator(metric Metric, numExBits int) *RaBitEstimator {
	return &RaBitEstimator{
		Metric:    metric,
		NumExBits: numExBits,
	}
}

// DistanceEstimate holds estimated distance and error bound.
type DistanceEstimate struct {
	Distance float64
	Error    float64
}

// EstimateDistance estimates the distance between a raw query vector and an
// encoded vector, returning both the estimated distance and the error bound.
// Matches Java's RaBitEstimator.estimateDistanceAndErrorBound().
func (e *RaBitEstimator) EstimateDistance(query []float64, encoded *EncodedVector) DistanceEstimate {
	if e.Metric == MetricCosine {
		qNormSqr := dot(query, query)
		if !(qNormSqr > 0.0) || math.IsInf(qNormSqr, 0) || math.IsNaN(qNormSqr) {
			return DistanceEstimate{Distance: math.NaN(), Error: 0.0}
		}
	}

	cb := float64(int(1)<<e.NumExBits) - 0.5
	gAdd := dot(query, query)
	gError := math.Sqrt(gAdd)

	// Compute dot(query, xuc) where xuc[i] = encoded[i] - cb.
	// Fused into single pass to avoid allocating a dims-sized []float64.
	dims := encoded.NumDimensions()
	var dotProduct float64
	for i := 0; i < dims && i < len(query); i++ {
		dotProduct += query[i] * (float64(encoded.Encoded[i]) - cb)
	}

	euclideanSquare := encoded.FAddEx + gAdd + encoded.FRescaleEx*dotProduct
	euclideanSquareError := encoded.FErrorEx * gError

	switch e.Metric {
	case MetricCosine:
		return DistanceEstimate{
			Distance: 0.5 * euclideanSquare,
			Error:    0.5 * euclideanSquareError,
		}
	case MetricInnerProduct:
		// Java: DOT_PRODUCT_METRIC => 0.5 * euclideanSquare - 1
		return DistanceEstimate{
			Distance: 0.5*euclideanSquare - 1,
			Error:    0.5 * euclideanSquareError,
		}
	default:
		// MetricEuclidean (squared L2) — matches Java's EUCLIDEAN_SQUARE_METRIC.
		return DistanceEstimate{
			Distance: euclideanSquare,
			Error:    euclideanSquareError,
		}
	}
}

// Distance returns the estimated distance between a raw query and an encoded vector.
// Returns an error if the result is non-finite (Inf or NaN), matching Java's
// behavior of throwing on non-finite distance estimates.
func (e *RaBitEstimator) Distance(query []float64, encoded *EncodedVector) (float64, error) {
	est := e.EstimateDistance(query, encoded)
	if math.IsInf(est.Distance, 0) || math.IsNaN(est.Distance) {
		return 0, fmt.Errorf("rabitq: distance estimate is not finite: %v", est.Distance)
	}
	return est.Distance, nil
}

// Scorer is a single-threaded, per-query distance estimator: the query's
// self-dot is computed ONCE and the component buffer is reused across codes.
// The general Distance path paid a fresh []int allocation, a bit-unpack, an
// estimator construction AND a re-derived query norm PER CODE — at SPFresh
// posting-scan volume (thousands of codes per query) that overhead dominated
// the estimate cost (RFC-094 094.4 tuning).
type Scorer struct {
	metric    Metric
	numExBits int
	query     []float64
	gAdd      float64
	buf       []int
}

// NewScorer prepares a scorer for one query vector. Not safe for concurrent
// use (the component buffer is shared across Score calls).
func (q *Quantizer) NewScorer(query []float64) *Scorer {
	// Copy: the scorer snapshots the query self-dot, so it must also snapshot
	// the components — callers reuse residual buffers across postings, and a
	// live reference would silently mix one posting's gAdd with the next
	// posting's components.
	query = append([]float64(nil), query...)
	return &Scorer{
		metric:    q.metric,
		numExBits: q.numExBits,
		query:     query,
		gAdd:      dot(query, query),
		buf:       make([]int, len(query)),
	}
}

// Score estimates the distance to one stored code. Identical math to
// Distance (EstimateDistance), minus the per-code allocations.
func (s *Scorer) Score(data []byte, numDimensions int) (float64, error) {
	// Match EstimateDistance's cosine zero/non-finite query guard: those
	// inputs must error like Distance does, never rank as finite (codex P2).
	if s.metric == MetricCosine && (!(s.gAdd > 0.0) || math.IsInf(s.gAdd, 0) || math.IsNaN(s.gAdd)) {
		return 0, fmt.Errorf("rabitq: distance estimate is not finite: %v", math.NaN())
	}
	if len(data) < 25 {
		return 0, fmt.Errorf("rabitq: data too short: %d bytes", len(data))
	}
	if data[0] != TypeByte {
		return 0, fmt.Errorf("rabitq: expected type ordinal %d, got %d", TypeByte, data[0])
	}
	fAddEx := math.Float64frombits(binary.BigEndian.Uint64(data[1:]))
	fRescaleEx := math.Float64frombits(binary.BigEndian.Uint64(data[9:]))
	if cap(s.buf) < numDimensions {
		s.buf = make([]int, numDimensions)
	}
	comps := s.buf[:numDimensions]
	if err := unpackComponentsInto(data[25:], comps, s.numExBits); err != nil {
		return 0, err
	}
	cb := float64(int(1)<<s.numExBits) - 0.5
	var dotProduct float64
	for i := 0; i < numDimensions && i < len(s.query); i++ {
		dotProduct += s.query[i] * (float64(comps[i]) - cb)
	}
	euclideanSquare := fAddEx + s.gAdd + fRescaleEx*dotProduct
	var d float64
	switch s.metric {
	case MetricCosine:
		d = 0.5 * euclideanSquare
	case MetricInnerProduct:
		d = 0.5*euclideanSquare - 1
	default:
		d = euclideanSquare
	}
	if math.IsInf(d, 0) || math.IsNaN(d) {
		return 0, fmt.Errorf("rabitq: distance estimate is not finite: %v", d)
	}
	return d, nil
}
