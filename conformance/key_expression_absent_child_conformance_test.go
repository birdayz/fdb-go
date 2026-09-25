//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
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

// A key expression Java's constructors refuse, read as Java reads it (RFC-257
// WS-J, ws-j-design.md 3.6): a Field without its name or its fan type
// (FieldKeyExpression.java:122-128; both are proto2 required fields, so only an
// in-memory or partially parsed proto lacks them, and an unknown fan type number
// is parsed into the unknown fields, a missing fan type too), and a Nesting whose
// parent is such a Field (NestingKeyExpression.java:67-73). Alone, each is
// KeyExpression.DeserializationException; inside a meta-data proto,
// RecordMetaData.build wraps it in MetaDataProtoDeserializationException "Error
// converting from protobuf" (RecordMetaDataBuilder.java:210-227, :261-265), as it
// wraps a subspace-key counter fault (:290-295). The JVM reads the protos
// partially parsed.
var _ = Describe("RFC-257 a key expression Java cannot deserialize is refused as Java refuses it", func() {
	const (
		deserialization = "com.apple.foundationdb.record.metadata.expressions.KeyExpression$DeserializationException"
		protoDeser      = "com.apple.foundationdb.record.RecordMetaDataBuilder$MetaDataProtoDeserializationException"
		metaDataExc     = "com.apple.foundationdb.record.metadata.MetaDataException"
	)
	noName := &gen.Field{FanType: gen.Field_SCALAR.Enum()}
	noFan := &gen.Field{FieldName: proto.String("order_id")}
	unknownFan := func() *gen.Field {
		f := &gen.Field{FieldName: proto.String("order_id")}
		f.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 7))
		return f
	}()
	price := &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("price"), FanType: gen.Field_SCALAR.Enum()}}
	exprs := []struct {
		name string
		expr *gen.KeyExpression
		want string
	}{
		{"a field without its name", &gen.KeyExpression{Field: noName}, "Serialized Field is missing field name"},
		{"a field without its fan type", &gen.KeyExpression{Field: noFan}, "Serialized Field is missing fan type"},
		{"a field whose fan type is 7", &gen.KeyExpression{Field: unknownFan}, "Serialized Field is missing fan type"},
		{"a nesting's parent without its fan type", &gen.KeyExpression{Nesting: &gen.Nesting{Parent: noFan, Child: price}}, "Serialized Field is missing fan type"},
		{"a nesting's parent without its name, and no child", &gen.KeyExpression{Nesting: &gen.Nesting{Parent: noName}}, "Serialized Field is missing field name"},
	}
	partial := func(m proto.Message) []int {
		b, err := proto.MarshalOptions{AllowPartial: true}.Marshal(m)
		Expect(err).NotTo(HaveOccurred())
		return BytesToIntArray(b)
	}
	for _, c := range exprs {
		It("alone: "+c.name, func() {
			var java struct {
				Class string `json:"class"`
				Error string `json:"error"`
			}
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "keyExpressionFromPartialProtoJava", map[string]any{
				"expression": partial(c.expr),
			}, &java)).To(Succeed())
			GinkgoWriter.Printf("DESERIALIZE %s java=%s %q\n", c.name, java.Class, java.Error)
			Expect([]string{java.Class, java.Error}).To(Equal([]string{deserialization, c.want}))

			_, goErr := recordlayer.KeyExpressionFromProto(c.expr)
			var de *recordlayer.KeyExpressionDeserializationError
			Expect(errors.As(goErr, &de)).To(BeTrue(), "%v", goErr)
			Expect(de.Message).To(Equal(c.want))
			var rce *recordlayer.RecordCoreError
			Expect(errors.As(goErr, &rce)).To(BeTrue(), "a DeserializationException is a RecordCoreException")
		})
	}

	demo := func() *gen.MetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		return p
	}
	for _, c := range []struct {
		name       string
		edit       func(*gen.MetaData)
		causeClass string
		cause      string
	}{
		{"an index root whose field has no name", func(md *gen.MetaData) {
			md.Indexes = append(md.Indexes, &gen.Index{
				Name: proto.String("i"), RecordType: []string{"Order"},
				RootExpression: &gen.KeyExpression{Field: noName}, AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
			})
		}, deserialization, "Serialized Field is missing field name"},
		{"an index root nesting under a parent with no fan type", func(md *gen.MetaData) {
			md.Indexes = append(md.Indexes, &gen.Index{
				Name: proto.String("i"), RecordType: []string{"Order"},
				RootExpression: &gen.KeyExpression{Nesting: &gen.Nesting{Parent: noFan, Child: price}}, AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
			})
		}, deserialization, "Serialized Field is missing fan type"},
		{"a primary key whose field has no fan type", func(md *gen.MetaData) {
			for _, rt := range md.RecordTypes {
				if rt.GetName() == "Order" {
					rt.PrimaryKey = &gen.KeyExpression{Field: noFan}
				}
			}
		}, deserialization, "Serialized Field is missing fan type"},
		{"a record-count key with no root", func(md *gen.MetaData) {
			md.RecordCountKey = &gen.KeyExpression{}
		}, deserialization, "Exactly one root must be specified for an index"},
		{"a subspace-key counter without its flag", func(md *gen.MetaData) {
			md.SubspaceKeyCounter = proto.Int64(3)
		}, metaDataExc, "subspaceKeyCounter is set but usesSubspaceKeyCounter is not set in the meta-data proto"},
	} {
		It("in a meta-data proto: "+c.name, func() {
			md := demo()
			c.edit(md)
			var java javaAnyVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildPartialMetaDataAnyVerdict", map[string]any{
				"protoBytes": partial(md),
			}, &java)).To(Succeed())
			GinkgoWriter.Printf("DESERIALIZE_MD %s java=%t %s %q cause=%s %q\n", c.name, java.Valid, java.Class, java.Error, java.CauseClass, java.CauseError)
			Expect([]string{java.Class, java.Error, java.CauseClass, java.CauseError}).To(Equal([]string{protoDeser, "Error converting from protobuf", c.causeClass, c.cause}))

			_, goErr := recordlayer.RecordMetaDataFromProto(md)
			var pde *recordlayer.MetaDataProtoDeserializationError
			Expect(errors.As(goErr, &pde)).To(BeTrue(), "%T %v", goErr, goErr)
			Expect(goErr.Error()).To(Equal(java.Error))
			var mde *recordlayer.MetaDataError
			Expect(errors.As(goErr, &mde)).To(BeTrue(), "a MetaDataProtoDeserializationException is a MetaDataException")
			switch c.causeClass {
			case deserialization:
				var de *recordlayer.KeyExpressionDeserializationError
				Expect(errors.As(pde.Cause, &de)).To(BeTrue(), "%v", pde.Cause)
				Expect(de.Message).To(Equal(java.CauseError))
			default:
				var cause *recordlayer.MetaDataError
				Expect(errors.As(pde.Cause, &cause)).To(BeTrue(), "%v", pde.Cause)
				Expect(cause.Message).To(Equal(java.CauseError))
			}
		})
	}
})
