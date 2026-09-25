//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
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
// in-memory or partially parsed proto lacks them, and protobuf-java parses a fan
// type number the enum does not declare into the unknown fields, a missing fan
// type too, where protobuf-go keeps it in the field), and a Nesting whose
// parent is such a Field (NestingKeyExpression.java:67-73). Alone, each is
// KeyExpression.DeserializationException; inside a meta-data proto,
// RecordMetaData.build wraps it in MetaDataProtoDeserializationException "Error
// converting from protobuf" (RecordMetaDataBuilder.java:210-227, :261-265), as it
// wraps a subspace-key counter fault (:290-295). Both engines read the same
// bytes, partially parsed; Go's parse is protobuf-go's, so an undeclared fan
// type reaches Go's loader in the field.
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

			var parsed gen.KeyExpression
			Expect(proto.UnmarshalOptions{AllowPartial: true}.Unmarshal(intsToBytes(partial(c.expr)), &parsed)).To(Succeed())
			_, goErr := recordlayer.KeyExpressionFromProto(&parsed)
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
		{"an index root whose field's fan type is 7", func(md *gen.MetaData) {
			md.Indexes = append(md.Indexes, &gen.Index{
				Name: proto.String("i"), RecordType: []string{"Order"},
				RootExpression: &gen.KeyExpression{Field: unknownFan}, AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
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

			var parsed gen.MetaData
			Expect(proto.UnmarshalOptions{AllowPartial: true}.Unmarshal(intsToBytes(partial(md)), &parsed)).To(Succeed())
			_, goErr := recordlayer.RecordMetaDataFromProto(&parsed)
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

// An index predicate's comparison operand is read when the meta-data is
// loaded, by IndexComparison.SimpleComparison(proto):
// Objects.requireNonNull(LiteralKeyExpression.fromProtoValue(operand)). An
// operand with two values is its RecordCoreException "More than one value
// encoded in value", not wrapped (RecordMetaDataBuilder wraps only a
// DeserializationException); one with none is a NullPointerException, which Go
// refuses as a RecordCoreError of its own text (DIVERGENCES.md). Both engines
// read the same bytes.
var _ = Describe("RFC-257 an index predicate's operand Java cannot read is refused as Java refuses it", func() {
	demo := func(operand *gen.Value) *gen.MetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		p.Version = proto.Int32(1)
		p.Indexes = append(p.Indexes, &gen.Index{
			Name: proto.String("i"), RecordType: []string{"Order"}, Type: proto.String("value"),
			RootExpression: recordlayer.Field("price").ToKeyExpression(), SubspaceKey: tuple.Tuple{"i"}.Pack(),
			AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
			Predicate: &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
				Value:      []string{"price"},
				Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{Type: gen.ComparisonType_GREATER_THAN.Enum(), Operand: operand}},
			}},
		})
		return p
	}
	for _, c := range []struct {
		name, class, error string
		operand            *gen.Value
	}{
		{
			"two values", "com.apple.foundationdb.record.RecordCoreException", "More than one value encoded in value",
			&gen.Value{LongValue: proto.Int64(1), StringValue: proto.String("a")},
		},
		{"no value", "java.lang.NullPointerException", "index comparison operand has no value", &gen.Value{}},
		{"one value (admitted)", "", "", &gen.Value{IntValue: proto.Int32(3)}},
	} {
		It(c.name, func() {
			b, err := proto.MarshalOptions{AllowPartial: true}.Marshal(demo(c.operand))
			Expect(err).NotTo(HaveOccurred())
			var java javaAnyVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildPartialMetaDataAnyVerdict", map[string]any{
				"protoBytes": BytesToIntArray(b),
			}, &java)).To(Succeed())
			var parsed gen.MetaData
			Expect(proto.UnmarshalOptions{AllowPartial: true}.Unmarshal(b, &parsed)).To(Succeed())
			_, goErr := recordlayer.RecordMetaDataFromProto(&parsed)
			GinkgoWriter.Printf("OPERAND %s java=%t %s %q go=%v\n", c.name, java.Valid, java.Class, java.Error, goErr)
			if c.class == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Class).To(Equal(c.class))
			var rce *recordlayer.RecordCoreError
			Expect(errors.As(goErr, &rce)).To(BeTrue(), "%T %v", goErr, goErr)
			Expect(rce.Message).To(Equal(c.error))
			if c.class != "java.lang.NullPointerException" {
				Expect(rce.Message).To(Equal(java.Error))
			}
		})
	}
})

// A stored meta-data proto is parsed by protobuf-java with its default
// recursion limit (CodedInputStream, 100 levels), so an index root nested
// deeper than that is refused at parse, before any key-expression check; Go
// parses shared bytes with the same limit (recordlayer.UnmarshalAsJava).
var _ = Describe("RFC-257 a key expression nested past protobuf's recursion limit", func() {
	nested := func(depth int) *gen.KeyExpression {
		root := recordlayer.Field("price").ToKeyExpression()
		for i := 1; i < depth; i++ {
			root = &gen.KeyExpression{Nesting: &gen.Nesting{
				Parent: &gen.Field{FieldName: proto.String("n"), FanType: gen.Field_SCALAR.Enum()},
				Child:  root,
			}}
		}
		return root
	}
	demo := func(depth int) []byte {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		p.Version = proto.Int32(1)
		p.Indexes = append(p.Indexes, &gen.Index{
			Name: proto.String("i"), RecordType: []string{"Order"}, Type: proto.String("value"),
			RootExpression: nested(depth), SubspaceKey: tuple.Tuple{"i"}.Pack(),
			AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
		})
		b, err := proto.Marshal(p)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	It("49 levels parse in both engines, 50 in neither", func() {
		var java javaAnyVerdict
		Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildPartialMetaDataAnyVerdict", map[string]any{
			"protoBytes": BytesToIntArray(demo(49)),
		}, &java)).To(Succeed())
		GinkgoWriter.Printf("DEPTH 49 java=%t %s %q\n", java.Valid, java.Class, java.Error)
		// Parsed; refused later for its made-up field.
		Expect(java.Class).To(Equal("com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"))
		Expect(recordlayer.UnmarshalAsJava(demo(49), &gen.MetaData{})).To(Succeed())

		err := NewJavaInvoker().InvokeAs(context.Background(), "buildPartialMetaDataAnyVerdict", map[string]any{
			"protoBytes": BytesToIntArray(demo(50)),
		}, &java)
		var je *JavaError
		Expect(errors.As(err, &je)).To(BeTrue(), "%v", err)
		GinkgoWriter.Printf("DEPTH 50 java=%s %q\n", je.ExceptionFullClass, je.Message)
		Expect(je.ExceptionFullClass).To(Equal("com.google.protobuf.InvalidProtocolBufferException"))
		goErr := recordlayer.UnmarshalAsJava(demo(50), &gen.MetaData{})
		Expect(goErr).To(MatchError(ContainSubstring("exceeded maximum recursion depth")))
		Expect(proto.Unmarshal(demo(50), &gen.MetaData{})).To(Succeed(), "protobuf-go's own default admits it")
	})
})

// A stored template's meta-data that fails to load is reported with Java's
// code (ExceptionUtil.toRelationalException): a MetaDataException is
// SYNTAX_OR_ACCESS_VIOLATION (42000), and a parse failure or any other
// RecordCoreException UNKNOWN (XXXXX), where Go's catalog answered XX000. Go
// writes the template row raw at the shared catalog subspace (past its own
// build path, as another writer would), and each engine creates a schema from
// it: Java's CREATE SCHEMA through JDBC, Go's catalog load its CREATE SCHEMA
// runs.
var _ = Describe("RFC-257 a stored template Java cannot load", func() {
	demo := func(edit func(*gen.MetaData)) []byte {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		p.Version = proto.Int32(1)
		edit(p)
		b, err := proto.MarshalOptions{AllowPartial: true}.Marshal(p)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	index := func(root *gen.KeyExpression, pred *gen.Predicate) func(*gen.MetaData) {
		return func(p *gen.MetaData) {
			p.Indexes = append(p.Indexes, &gen.Index{
				Name: proto.String("i"), RecordType: []string{"Order"}, Type: proto.String("value"),
				RootExpression: root, SubspaceKey: tuple.Tuple{"i"}.Pack(),
				AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1), Predicate: pred,
			})
		}
	}
	price := recordlayer.Field("price").ToKeyExpression()
	deep := price
	for i := 1; i < 50; i++ {
		deep = &gen.KeyExpression{Nesting: &gen.Nesting{Parent: &gen.Field{FieldName: proto.String("n"), FanType: gen.Field_SCALAR.Enum()}, Child: deep}}
	}
	for _, c := range []struct {
		name string
		md   []byte
		code string
	}{
		// A required field unset fails the full parse, before the key
		// expression's own check.
		{"a root field without its fan type", demo(index(&gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("price")}}, nil)), "XXXXX"},
		{"an operand with two values", demo(index(price, &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{
				Type: gen.ComparisonType_GREATER_THAN.Enum(), Operand: &gen.Value{LongValue: proto.Int64(1), StringValue: proto.String("a")},
			}},
		}})), "XXXXX"},
		{"a root nested 50 deep", demo(index(deep, nil)), "XXXXX"},
		{"a subspace-key counter without its flag", demo(func(p *gen.MetaData) { p.SubspaceKeyCounter = proto.Int64(3) }), "42000"},
	} {
		It(c.name, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			name := "BADT_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			catalogMD, err := catalog.BuildCatalogMetaData()
			Expect(err).NotTo(HaveOccurred())
			db := recordlayer.NewFDBDatabase(sharedDB)
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(catalog.DefaultCatalogSubspace()).
					SetMetaDataProvider(catalogMD).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Templates{TEMPLATE_NAME: proto.String(name), TEMPLATE_VERSION: proto.Int32(1), META_DATA: c.md})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			var out map[string]any
			javaErr := NewJavaInvoker().InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &out)
			var je *JavaError
			Expect(errors.As(javaErr, &je)).To(BeTrue(), "%v", javaErr)

			cat, err := catalog.OpenRecordLayerStoreCatalog()
			Expect(err).NotTo(HaveOccurred())
			_, goErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := cat.SchemaTemplateCatalog().LoadSchemaTemplate(catalog.NewFDBTransaction(rtx), name)
				return nil, err
			})
			var ae *api.Error
			goCode := ""
			if errors.As(goErr, &ae) {
				goCode = string(ae.Code)
			}
			GinkgoWriter.Printf("BADTEMPLATE %s java=%s %s %q go=%s %v\n", c.name, je.SQLState, je.ExceptionFullClass, je.Message, goCode, goErr)
			Expect(je.SQLState).To(Equal(c.code), "Java's code moved")
			Expect(goCode).To(Equal(je.SQLState))
		})
	}
})
