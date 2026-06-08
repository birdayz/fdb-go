package recordlayer

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
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

	Describe("keyExpressionHasNullField", func() {
		// Lines 130-172 of atomic_index_helpers.go.
		// Covers: CompositeKeyExpression, GroupingKeyExpression, EmptyKeyExpression,
		// default case, nil message.

		It("returns true for nil message", func() {
			Expect(keyExpressionHasNullField(nil, Field("price"))).To(BeTrue())
		})

		It("returns false for set field", func() {
			order := &gen.Order{Price: proto.Int32(100)}
			Expect(keyExpressionHasNullField(order, Field("price"))).To(BeFalse())
		})

		It("returns true for unset field", func() {
			order := &gen.Order{}
			Expect(keyExpressionHasNullField(order, Field("price"))).To(BeTrue())
		})

		It("returns true for unknown field name", func() {
			order := &gen.Order{Price: proto.Int32(100)}
			Expect(keyExpressionHasNullField(order, Field("nonexistent"))).To(BeTrue())
		})

		It("handles CompositeKeyExpression - all set", func() {
			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			}
			expr := Concat(Field("order_id"), Field("price"))
			Expect(keyExpressionHasNullField(order, expr)).To(BeFalse())
		})

		It("handles CompositeKeyExpression - one unset", func() {
			order := &gen.Order{OrderId: proto.Int64(1)}
			expr := Concat(Field("order_id"), Field("price"))
			Expect(keyExpressionHasNullField(order, expr)).To(BeTrue())
		})

		It("handles GroupingKeyExpression", func() {
			order := &gen.Order{Price: proto.Int32(100)}
			expr := Ungrouped(Field("price"))
			Expect(keyExpressionHasNullField(order, expr)).To(BeFalse())

			order2 := &gen.Order{}
			Expect(keyExpressionHasNullField(order2, expr)).To(BeTrue())
		})

		It("returns false for EmptyKeyExpression", func() {
			order := &gen.Order{}
			Expect(keyExpressionHasNullField(order, EmptyKey())).To(BeFalse())
		})

		It("returns false for default (unknown) expression type", func() {
			// VersionKeyExpression is not handled by the switch — falls to default.
			order := &gen.Order{Price: proto.Int32(100)}
			Expect(keyExpressionHasNullField(order, VersionKey())).To(BeFalse())
		})

		It("handles NestingKeyExpression with unknown parent field", func() {
			order := &gen.Order{}
			expr := Nest("nonexistent_field", Field("type"))
			Expect(keyExpressionHasNullField(order, expr)).To(BeTrue())
		})

		It("handles NestingKeyExpression with unset parent message", func() {
			// Order has an optional Flower field. If not set, should be true.
			order := &gen.Order{OrderId: proto.Int64(1)}
			expr := Nest("flower", Field("type"))
			Expect(keyExpressionHasNullField(order, expr)).To(BeTrue())
		})

		It("handles NestingKeyExpression with set parent message", func() {
			order := &gen.Order{
				OrderId: proto.Int64(1),
				Flower:  &gen.Flower{Type: proto.String("Rose")},
			}
			expr := Nest("flower", Field("type"))
			Expect(keyExpressionHasNullField(order, expr)).To(BeFalse())
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

	Describe("calculateDelay", func() {
		// Lines 223-231 of runner.go.
		It("returns exponential backoff with jitter", func() {
			runner := &FDBDatabaseRunner{
				InitialDelay: 100 * time.Millisecond,
				MaxDelay:     10 * time.Second,
			}

			// Attempt 1: base delay = 100ms * 2^0 = 100ms, jitter 0.5-1.5x → 50-150ms
			d1 := runner.calculateDelay(1)
			Expect(d1).To(BeNumerically(">=", 50*time.Millisecond))
			Expect(d1).To(BeNumerically("<=", 150*time.Millisecond))

			// Attempt 3: base delay = 100ms * 2^2 = 400ms, jitter → 200-600ms
			d3 := runner.calculateDelay(3)
			Expect(d3).To(BeNumerically(">=", 200*time.Millisecond))
			Expect(d3).To(BeNumerically("<=", 600*time.Millisecond))
		})

		It("caps at MaxDelay", func() {
			runner := &FDBDatabaseRunner{
				InitialDelay: 1 * time.Second,
				MaxDelay:     2 * time.Second,
			}

			// Attempt 10: base = 1s * 2^9 = 512s, capped to 2s, jitter → 1-3s
			d := runner.calculateDelay(10)
			Expect(d).To(BeNumerically(">=", 1*time.Second))
			Expect(d).To(BeNumerically("<=", 3*time.Second))
		})
	})
})
