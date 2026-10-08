package recordlayer

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tests targeting uncovered lines in atomic_index_helpers.go and runner.go.
var _ = Describe("Helper function coverage", func() {
	Describe("toInt64", func() {
		// Lines 177-192 of atomic_index_helpers.go.
		// Covered: int64. Uncovered: int32, int, float64, float32, default error.
		It("converts int64", func() {
			v, err := toInt64(int64(42))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(int64(42)))
		})

		It("converts int32", func() {
			v, err := toInt64(int32(42))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(int64(42)))
		})

		It("converts int", func() {
			v, err := toInt64(int(42))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(int64(42)))
		})

		It("converts float64", func() {
			v, err := toInt64(float64(42.9))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(int64(42))) // truncates
		})

		It("converts float32", func() {
			v, err := toInt64(float32(42.9))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(int64(42))) // truncates
		})

		// WIRE PARITY: a SUM/MAX_EVER/MIN_EVER atomic index over a DOUBLE field truncates
		// the value toward zero, matching Java's Number.longValue() (AtomicMutation.java
		// SUM_LONG :187 / *_EVER_LONG :199 call numVal.longValue(); Java has no float SUM
		// variant and its factory.validate never rejects a double field). The summand is
		// encoded little-endian for MutationType.ADD, so a floor-based conversion
		// (-42.9 → -43) would write DIFFERENT index bytes than Java for the same record.
		// Pin truncation-toward-zero so a future "fix" can't silently diverge the wire.
		It("truncates negative floats toward zero (Java longValue parity, not floor)", func() {
			v, err := toInt64(float64(-42.9))
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(int64(-42))) // toward zero, NOT -43 (floor)

			v32, err := toInt64(float32(-42.9))
			Expect(err).NotTo(HaveOccurred())
			Expect(v32).To(Equal(int64(-42)))
		})

		It("returns error for unsupported type", func() {
			_, err := toInt64("not a number")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot convert"))
		})
	})

	Describe("indexGroupingCount", func() {
		It("returns grouping count from GroupingKeyExpression", func() {
			// GroupBy(grouped=Field("b"), groupBy=Field("a"))
			// wholeKey=Concat(a,b), groupedCount=1, groupingCount=2-1=1
			gke := GroupBy(Field("b"), Field("a"))
			Expect(indexGroupingCount(gke)).To(Equal(1))
		})

		It("returns full column size for non-grouping expression", func() {
			expr := Concat(Field("a"), Field("b"))
			Expect(indexGroupingCount(expr)).To(Equal(2))
		})
	})

	Describe("exponentialDelay", func() {
		// Java's ExponentialDelay: each delay is uniform in [0, current), and
		// current then doubles, capped at the maximum, floored at 2 ms.
		It("draws below the current bound, which doubles to the maximum", func() {
			d := newExponentialDelay(100*time.Millisecond, 350*time.Millisecond, nil)
			for _, bound := range []time.Duration{100, 200, 350, 350} {
				got := d.delay()
				Expect(got).To(BeNumerically(">=", 0))
				Expect(got).To(BeNumerically("<", bound*time.Millisecond))
			}
		})

		It("floors the bound at 2 ms", func() {
			d := newExponentialDelay(0, time.Second, nil)
			Expect(d.delay()).To(Equal(time.Duration(0)), "the first draw is below the initial 0 ms")
			Expect(d.current).To(Equal(2 * time.Millisecond))
		})
	})
})
