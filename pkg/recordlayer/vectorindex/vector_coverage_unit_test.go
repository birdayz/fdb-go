package vectorindex

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Coverage Unit Tests (vector)", func() {
	// vec_math.go: dot() with mismatched-length vectors (lines 9-10).
	Describe("dot", func() {
		It("truncates to shorter vector when b is shorter", func() {
			a := []float64{1, 2, 3, 4}
			b := []float64{10, 20}
			// Only the first 2 elements contribute: 1*10 + 2*20 = 50
			Expect(dot(a, b)).To(Equal(50.0))
		})

		It("truncates to shorter vector when a is shorter", func() {
			a := []float64{3, 4}
			b := []float64{1, 2, 5, 6}
			Expect(dot(a, b)).To(Equal(11.0))
		})

		It("handles empty vectors", func() {
			Expect(dot(nil, nil)).To(Equal(0.0))
			Expect(dot([]float64{1}, nil)).To(Equal(0.0))
			Expect(dot(nil, []float64{1})).To(Equal(0.0))
		})
	})

	// hnsw_vector.go: distance functions with mismatched-length vectors.
	Describe("euclideanDistance", func() {
		It("truncates to shorter vector when b is shorter", func() {
			a := []float64{1, 2, 3}
			b := []float64{4, 6}
			// (1-4)^2 + (2-6)^2 = 9 + 16 = 25; true L2 = sqrt(25) = 5
			Expect(euclideanDistance(a, b)).To(Equal(5.0))
		})
	})

	Describe("cosineDistance", func() {
		It("truncates to shorter vector when b is shorter", func() {
			a := []float64{1, 0, 999}
			b := []float64{1, 0}
			// dot=1, normA=1, normB=1 => sim=1 => distance=0
			Expect(cosineDistance(a, b)).To(BeNumerically("~", 0.0, 1e-10))
		})

		It("returns max distance for antiparallel vectors", func() {
			// Antiparallel unit vectors: sim = -1.0 exactly.
			// The sim < -1.0 and sim > 1.0 clamp branches are defensive
			// guards for floating-point drift — unreachable with real
			// arithmetic but keep the result in [0, 2].
			a := []float64{1, 0, 0}
			b := []float64{-1, 0, 0}
			Expect(cosineDistance(a, b)).To(BeNumerically("~", 2.0, 1e-10))
		})
	})

	Describe("innerProductDistance", func() {
		It("truncates to shorter vector when b is shorter", func() {
			a := []float64{2, 3, 100}
			b := []float64{4, 5}
			// dot = 2*4 + 3*5 = 23; distance = -23
			Expect(innerProductDistance(a, b)).To(Equal(-23.0))
		})
	})

	Describe("vectorDistance", func() {
		It("dispatches to euclidean by default", func() {
			a := []float64{0, 0}
			b := []float64{3, 4}
			// 3^2 + 4^2 = 25; true L2 = sqrt(25) = 5
			Expect(vectorDistance(a, b, VectorMetricEuclidean)).To(Equal(5.0))
		})

		It("dispatches to cosine", func() {
			a := []float64{1, 0}
			b := []float64{0, 1}
			Expect(vectorDistance(a, b, VectorMetricCosine)).To(BeNumerically("~", 1.0, 1e-10))
		})

		It("dispatches to inner product", func() {
			a := []float64{2, 3}
			b := []float64{4, 5}
			Expect(vectorDistance(a, b, VectorMetricInnerProduct)).To(Equal(-23.0))
		})
	})

	Describe("VectorMetric properties", func() {
		It("euclidean satisfies both properties", func() {
			Expect(VectorMetricEuclidean.satisfiesPreservedUnderTranslation()).To(BeTrue())
			Expect(VectorMetricEuclidean.satisfiesTriangleInequality()).To(BeTrue())
		})

		It("euclidean-square is translation-preserved but not a true metric", func() {
			// Matches Java EuclideanSquareMetric: preserved under translation (default),
			// but satisfiesTriangleInequality() == false (squared L2 is not a true metric).
			Expect(VectorMetricEuclideanSquare.satisfiesPreservedUnderTranslation()).To(BeTrue())
			Expect(VectorMetricEuclideanSquare.satisfiesTriangleInequality()).To(BeFalse())
		})

		It("cosine satisfies neither property", func() {
			Expect(VectorMetricCosine.satisfiesPreservedUnderTranslation()).To(BeFalse())
			Expect(VectorMetricCosine.satisfiesTriangleInequality()).To(BeFalse())
		})

		It("inner product satisfies neither property", func() {
			Expect(VectorMetricInnerProduct.satisfiesPreservedUnderTranslation()).To(BeFalse())
			Expect(VectorMetricInnerProduct.satisfiesTriangleInequality()).To(BeFalse())
		})
	})

	// hnsw_stats.go: context attachment and stat tracking.
	Describe("HNSWStats", func() {
		It("WithHNSWStats and GetHNSWStats round-trip", func() {
			ctx, stats := WithHNSWStats(context.Background())
			Expect(stats).NotTo(BeNil())

			retrieved := GetHNSWStats(ctx)
			Expect(retrieved).To(BeIdenticalTo(stats))
		})

		It("GetHNSWStats returns nil when not attached", func() {
			Expect(GetHNSWStats(context.Background())).To(BeNil())
		})

		It("stat helper functions increment counters", func() {
			stats := &HNSWStats{}

			hnswStatGet(stats)
			hnswStatGet(stats)
			Expect(stats.FDBGets.Load()).To(Equal(int64(2)))

			hnswStatBatchGet(stats)
			Expect(stats.FDBBatchGets.Load()).To(Equal(int64(1)))

			hnswStatRangeRead(stats)
			hnswStatRangeRead(stats)
			hnswStatRangeRead(stats)
			Expect(stats.FDBRangeReads.Load()).To(Equal(int64(3)))

			hnswStatCacheHit(stats)
			Expect(stats.CacheHits.Load()).To(Equal(int64(1)))
		})

		It("stat helper functions are no-ops with nil", func() {
			// Should not panic.
			hnswStatGet(nil)
			hnswStatBatchGet(nil)
			hnswStatRangeRead(nil)
			hnswStatCacheHit(nil)
		})
	})
})
