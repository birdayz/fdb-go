package recordlayer

import (
	"errors"
	"math"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/gen/demopb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func partitionTestBuilder() *RecordMetaDataBuilder {
	b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("order_id")))
	b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("val_int64"), 10), Field("val_int64"), Field("id")))
	return b
}

func TestPartitionFunctionFloorAndSingleton(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ x, width, want int64 }{
		{-11, 10, -2},
		{-10, 10, -1},
		{-1, 10, -1},
		{0, 10, 0},
		{9, 10, 0},
		{10, 10, 1},
		{math.MinInt64, 1, math.MinInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MinInt64, 10, -922337203685477581},
		{math.MaxInt64, 10, 922337203685477580},
		{math.MinInt64, math.MaxInt64, -2},
		{math.MaxInt64, math.MaxInt64, 1},
	} {
		p := PartitionExpr(Field("coord_x"), tc.width)
		got, err := p.Evaluate(nil, &demopb.Order{CoordX: proto.Int64(tc.x)})
		if err != nil || len(got) != 1 || len(got[0]) != 1 || got[0][0] != tc.want {
			t.Fatalf("floor(%d/%d) = %v, %v; want %d", tc.x, tc.width, got, err, tc.want)
		}
		if createsDuplicates(p) || p.ColumnSize() != 1 {
			t.Fatal("partition function is not a singleton")
		}
	}
	generic := FunctionExpr(FunctionNamePartitionV1, Concat(Field("coord_x"), Literal(int64(10))))
	if !createsDuplicates(generic) {
		t.Fatal("bare generic function must retain its duplicate-producing default")
	}
	div, err := FunctionExpr("div", Concat(Literal(int64(-1)), Literal(int64(10)))).Evaluate(nil, nil)
	if err != nil || div[0][0] != int64(0) {
		t.Fatalf("existing div changed: %v, %v", div, err)
	}
	_, err = PartitionExpr(Field("coord_x"), 10).Evaluate(nil, &demopb.Order{})
	var routing *PartitionRoutingError
	if !errors.As(err, &routing) {
		t.Fatalf("NULL source = %v, want PartitionRoutingError", err)
	}
	for _, value := range []any{nil, "10", int32(10), float64(10)} {
		_, err := evaluatePartition(nil, nil, [][]any{{value, int64(10)}})
		if !errors.As(err, &routing) {
			t.Fatalf("source %T = %v, want PartitionRoutingError", value, err)
		}
	}
}

func TestPartitionFunctionProtoAndStructuralEquality(t *testing.T) {
	t.Parallel()
	expr := PartitionExpr(Field("coord_x"), 10)
	encoded := expr.ToKeyExpression()
	if encoded.Function.GetName() != FunctionNamePartitionV1 || encoded.Function.Arguments.GetThen().Child[1].GetValue().GetLongValue() != 10 {
		t.Fatalf("not the required Function proto: %v", encoded)
	}
	data, err := proto.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &gen.KeyExpression{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatal(err)
	}
	back, err := KeyExpressionFromProto(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.(*PartitionFunctionKeyExpression); !ok || createsDuplicates(back) {
		t.Fatalf("round trip lost singleton subtype: %T", back)
	}
	generic := FunctionExpr(FunctionNamePartitionV1, expr.Arguments())
	for _, other := range []KeyExpression{back, generic} {
		if !keyExpressionEquals(expr, other) || !keyExpressionEquals(other, expr) {
			t.Fatal("function equality must be symmetric across subtypes")
		}
	}
	if keyExpressionEquals(expr, PartitionExpr(Field("coord_x"), 11)) || keyExpressionEquals(expr, PartitionExpr(Field("coord_y"), 10)) {
		t.Fatal("different interval/source compared equal")
	}
	for _, arguments := range []KeyExpression{
		Concat(Field("coord_x"), Literal(int64(0))), Concat(Field("coord_x"), Literal(int64(-1))),
		Concat(Field("coord_x"), Literal(int32(10))), Concat(FanOut("tags"), Literal(int64(10))),
		Concat(Field("coord_x"), Field("coord_y")),
	} {
		_, err := KeyExpressionFromProto((&FunctionKeyExpression{name: FunctionNamePartitionV1, arguments: arguments}).ToKeyExpression())
		var typed *KeyExpressionDeserializationError
		if !errors.As(err, &typed) {
			t.Fatalf("malformed arguments %v accepted: %v", arguments.ToKeyExpression(), err)
		}
	}
}

func TestPartitionFunctionRenamePreservesSubtype(t *testing.T) {
	t.Parallel()
	file := protodesc.ToFileDescriptorProto(demopb.File_record_layer_demo_proto)
	copy, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	expr := PartitionExpr(Field("coord_x"), 10)
	source := demopb.File_record_layer_demo_proto.Messages().ByName("Order")
	unchanged, err := renameFields(expr, source, copy.Messages().ByName("Order"))
	if err != nil || unchanged != expr {
		t.Fatalf("no-op rename = %T, %v", unchanged, err)
	}
	for _, message := range file.MessageType {
		if message.GetName() == "Order" {
			for _, field := range message.Field {
				if field.GetName() == "coord_x" {
					field.Name = proto.String("effective_ns")
				}
			}
		}
	}
	renamedFile, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := renameFields(expr, source, renamedFile.Messages().ByName("Order"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := renamed.(*PartitionFunctionKeyExpression); !ok || createsDuplicates(renamed) || !keyExpressionEquals(renamed, PartitionExpr(Field("effective_ns"), 10)) {
		t.Fatalf("rename lost partition identity: %T", renamed)
	}
}

func TestPartitionMetadataValidation(t *testing.T) {
	t.Parallel()
	p := func() KeyExpression { return PartitionExpr(Field("coord_x"), 10) }
	for _, tc := range []struct {
		name string
		edit func(*RecordMetaDataBuilder)
	}{
		{"intermingled plain sibling", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Customer").SetPrimaryKey(Concat(Field("customer_id"), Field("price")))
		}},
		{"missing type", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(p(), Field("coord_x"), Field("order_id")))
		}},
		{"wrong position", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("coord_x"), p(), Field("order_id")))
		}},
		{"missing logical source", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), p(), Field("order_id")))
		}},
		{"wrong source type", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("price"), 10), Field("price"), Field("order_id")))
		}},
		{"zero width", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("coord_x"), 0), Field("coord_x"), Field("order_id")))
		}},
		{"negative width", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("coord_x"), -10), Field("coord_x"), Field("order_id")))
		}},
		{"unprefixed local", func(b *RecordMetaDataBuilder) { b.AddIndex("Order", NewIndex("bad", Field("price"))) }},
		{"inconsistent interval", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", Concat(PartitionExpr(Field("coord_x"), 11), Field("price"))))
		}},
		{"index type discriminator", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", Concat(RecordTypeKey(), p(), Field("price"))))
		}},
		{"unique without source", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", Concat(p(), Field("price"))).SetUnique())
		}},
		{"unique source in values", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", KeyWithValue(Concat(p(), Field("price"), Field("coord_x")), 2)).SetUnique())
		}},
		{"ungrouped text", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewTextIndex("bad", Concat(p(), Nest("flower", Field("type")))))
		}},
		{"repeated text body", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewTextIndex("bad", GroupBy(FanOut("tags"), p(), Field("quantity"))))
		}},
		{"aggregate", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewCountIndex("bad", GroupBy(Field("price"), p())))
		}},
		{"universal", func(b *RecordMetaDataBuilder) { b.AddUniversalIndex(NewIndex("bad", Concat(p(), Field("price")))) }},
		{"multi-type", func(b *RecordMetaDataBuilder) {
			b.AddMultiTypeIndex([]string{"Order", "TypedRecord"}, NewIndex("bad", Concat(p(), Field("price"))))
		}},
		{"store count", func(b *RecordMetaDataBuilder) { b.SetRecordCountKey(RecordTypeKey()) }},
		{"plain table partition index", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Customer", NewIndex("bad", Concat(PartitionExpr(Field("customer_id"), 10), Field("name"))))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := partitionTestBuilder()
			tc.edit(b)
			if _, err := b.Build(); err == nil {
				t.Fatal("unsupported partition metadata accepted")
			}
		})
	}
	b := partitionTestBuilder()
	b.AddIndex("Order", NewIndex("value", Concat(p(), Field("price"))))
	b.AddIndex("Order", NewIndex("fanout", Concat(p(), FanOut("tags"))))
	b.AddIndex("Order", NewIndex("cover", KeyWithValue(Concat(p(), Field("price"), Field("quantity")), 2)))
	b.AddIndex("Order", NewIndex("unique", Concat(p(), Field("coord_x"), Field("price"))).SetUnique())
	b.AddIndex("Order", NewTextIndex("text", GroupBy(Nest("flower", Field("type")), p(), Field("quantity"))))
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	back, err := RecordMetaDataFromProto(wire)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := back.GetRecordType("Order").PartitionSpec()
	if err != nil || spec.SourceColumn() != "coord_x" || spec.IntervalWidth() != 10 {
		t.Fatalf("spec round trip = %v, %v", spec, err)
	}
	plain, err := back.GetRecordType("Customer").PartitionSpec()
	if err != nil || plain != nil {
		t.Fatalf("plain table acquired partition spec: %v, %v", plain, err)
	}
	for _, rt := range wire.RecordTypes {
		if rt.GetName() == "Order" {
			rt.PrimaryKey.Then.Child[1].Function.Name = proto.String("fdb_partition_v2")
		}
	}
	if _, err := RecordMetaDataFromProto(wire); err == nil {
		t.Fatal("unknown required partition version accepted")
	}
}
