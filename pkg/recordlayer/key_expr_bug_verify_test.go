package recordlayer

import (
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("KeyExprBugVerify", func() {
	// Bug 1: FieldKeyExpression.Evaluate must return nil for unset proto2 optional fields.
	// Java checks message.hasField(fd) and returns null for unset fields.
	Describe("Bug1: unset proto2 field returns nil", func() {
		It("unset optional int64 field returns nil", func() {
			order := &gen.Order{} // order_id is nil (unset)
			expr := Field("order_id")
			result, err := expr.Evaluate(nil, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("unset optional string field returns nil", func() {
			customer := &gen.Customer{} // name is nil (unset)
			expr := Field("name")
			result, err := expr.Evaluate(nil, customer)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("set field returns actual value", func() {
			order := &gen.Order{OrderId: proto.Int64(42)}
			expr := Field("order_id")
			result, err := expr.Evaluate(nil, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(42)}}))
		})
	})

	// Bug 2: FieldKeyExpression nil message must respect FanType.
	// Java's getNullResult(): FanOut → empty, Concatenate → [[emptyList]], None → [[nil]].
	Describe("Bug2: nil message respects FanType", func() {
		It("FanType.None on nil returns [[nil]]", func() {
			expr := Field("order_id")
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("FanType.FanOut on nil returns empty", func() {
			expr := FanOut("tags")
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeEmpty())
		})

		It("FanType.Concatenate on nil returns [[empty nested tuple]]", func() {
			expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(HaveLen(1))
			// Empty nested tuple.Tuple (packable), not a bare []any (Pack panics on []any).
			emptyTuple, ok := result[0][0].(tuple.Tuple)
			Expect(ok).To(BeTrue(), "Concatenate on nil should return an empty nested tuple")
			Expect(emptyTuple).To(BeEmpty())
		})
	})

	// Bug 3: NestingKeyExpression.Evaluate must not panic on nil message.
	Describe("Bug3: NestingKeyExpression nil message", func() {
		It("Nest on nil message returns [[nil]]", func() {
			expr := Nest("flower", Field("type"))
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("NestFanOut on nil message returns empty", func() {
			expr := NestFanOut("items", Field("name"))
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeEmpty())
		})

		It("nested chain with unset outer message field returns [[nil]]", func() {
			order := &gen.Order{OrderId: proto.Int64(1)} // flower is nil
			expr := Nest("flower", Field("type"))
			result, err := expr.Evaluate(nil, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})
	})

	// Bug 4: RecordTypeKeyExpression.Evaluate must not panic on nil message.
	Describe("Bug4: RecordTypeKeyExpression nil message", func() {
		It("returns [[nil]] on nil message", func() {
			expr := RecordTypeKey()
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("works on non-nil message", func() {
			order := &gen.Order{OrderId: proto.Int64(1)}
			expr := RecordTypeKey()
			result, err := expr.Evaluate(nil, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(HaveLen(1))
			Expect(result[0][0]).To(Equal("Order"))
		})
	})
})
