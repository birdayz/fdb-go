// Portions derived from FoundationDB Record Layer (RaBitQuantizer.java,
// EncodedRealVector.java, VectorType.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rabitq

import (
	"container/heap"
	"fmt"
	"math"
)

// eps is the epsilon used for floor quantization.
const eps = 1e-5

// eps0 is the error scaling constant from the RaBitQ paper.
const eps0 = 1.9

// nEnum controls the sweep range extension for bestRescaleFactor.
const nEnum = 10

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
	if !ValidNumExBits(numExBits) {
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

	// The norm-then-square operation order is part of the serialized
	// calibration, not interchangeable with the dot product itself.
	xuCbNorm := l2Norm(xuCb)
	xuCbNormSqr := xuCbNorm * xuCbNorm

	ipResidualXuCbSafe := ipResidualXuCb
	if ipResidualXuCb == 0.0 {
		ipResidualXuCbSafe = math.Inf(1)
	}

	// Java retains NaN when rounding makes this argument negative (including
	// zero residuals). The error factor is serialized verbatim, including
	// Java's host-dependent NaN sign (AMD64 and ARM64 differ), so clamping
	// or canonicalizing it would change persisted vector bytes.
	sqrtArg := ((residualL2Sqr*xuCbNormSqr)/
		(ipResidualXuCbSafe*ipResidualXuCbSafe) - 1.0) /
		float64(max(1, dims-1))
	// Round before doubling: a compiler may lower 2*x to x+x and fuse
	// the multiplication producing x into that addition.
	tmpError := float64(residualL2Norm * eps0 * math.Sqrt(sqrtArg))

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
		k := int(math.Floor(float64(t*oAbs[i]) + eps))
		if k > maxLevel {
			k = maxLevel
		}
		code[i] = k
		ipNorm += float64((float64(k) + 0.5) * oAbs[i])
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
		cur := int(float64(tStart*oAbs[i]) + eps)
		curOB[i] = cur
		sqrDen += float64(float64(cur)*float64(cur)) + float64(cur)
		numer += float64((float64(cur) + 0.5) * oAbs[i])
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

		sqrDen += float64(2.0 * float64(u))
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
