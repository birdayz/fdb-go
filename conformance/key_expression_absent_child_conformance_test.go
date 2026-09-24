//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// An absent child of a key expression, read as Java reads it (RFC-257 WS-J,
// ws-j-design.md 4d): protobuf-java hands an absent message field over as its
// default instance, an expression with no root, which fromProto refuses
// ("Exactly one root must be specified for an index"). From stored bytes only a
// Nesting's child can be absent (the others are proto2 required and fail at
// parse in both engines); in memory any can, and the JVM reads those protos
// partially parsed.
var _ = Describe("RFC-257 an absent key-expression child is Java's refusal", func() {
	field := &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("order_id"), FanType: gen.Field_SCALAR.Enum(), NullInterpretation: gen.Field_NOT_UNIQUE.Enum()}}
	const (
		deserialization = "com.apple.foundationdb.record.metadata.expressions.KeyExpression$DeserializationException"
		noRoot          = "Exactly one root must be specified for an index"
	)
	for _, c := range []struct {
		name string
		expr *gen.KeyExpression
	}{
		{"a nesting's child", &gen.KeyExpression{Nesting: &gen.Nesting{Parent: field.GetField()}}},
		{"a grouping's whole key", &gen.KeyExpression{Grouping: &gen.Grouping{GroupedCount: proto.Int32(0)}}},
		{"a dimensions' whole key", &gen.KeyExpression{Dimensions: &gen.Dimensions{PrefixSize: proto.Int32(0), DimensionsSize: proto.Int32(0)}}},
		{"a key-with-value's inner key", &gen.KeyExpression{KeyWithValue: &gen.KeyWithValue{SplitPoint: proto.Int32(0)}}},
		{"a split's joined key", &gen.KeyExpression{Split: &gen.Split{SplitSize: proto.Int32(1)}}},
		{"a function's arguments", &gen.KeyExpression{Function: &gen.Function{Name: proto.String("add")}}},
	} {
		It(c.name, func() {
			b, err := proto.MarshalOptions{AllowPartial: true}.Marshal(c.expr)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				Class string `json:"class"`
				Error string `json:"error"`
			}
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "keyExpressionFromPartialProtoJava", map[string]any{
				"expression": BytesToIntArray(b),
			}, &java)).To(Succeed())
			GinkgoWriter.Printf("ABSENTCHILD %s java=%s %q\n", c.name, java.Class, java.Error)
			Expect([]string{java.Class, java.Error}).To(Equal([]string{deserialization, noRoot}))

			_, goErr := recordlayer.KeyExpressionFromProto(c.expr)
			var de *recordlayer.KeyExpressionDeserializationError
			Expect(errors.As(goErr, &de)).To(BeTrue(), "%v", goErr)
			Expect(de.Message).To(Equal(noRoot))
		})
	}

	It("a nesting's child is absent from stored bytes; the others fail at parse", func() {
		stored, err := proto.Marshal(&gen.KeyExpression{Nesting: &gen.Nesting{Parent: field.GetField()}})
		Expect(err).NotTo(HaveOccurred())
		var parsed gen.KeyExpression
		Expect(proto.Unmarshal(stored, &parsed)).To(Succeed())
		_, goErr := recordlayer.KeyExpressionFromProto(&parsed)
		var de *recordlayer.KeyExpressionDeserializationError
		Expect(errors.As(goErr, &de)).To(BeTrue(), "%v", goErr)
		Expect(de.Message).To(Equal(noRoot))

		partial, err := proto.MarshalOptions{AllowPartial: true}.Marshal(&gen.KeyExpression{Grouping: &gen.Grouping{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(proto.Unmarshal(partial, &gen.KeyExpression{})).NotTo(Succeed(), "a grouping without its required whole key parses")
	})
})
