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
		Concat(FieldWithNullStandin("coord_x", FanTypeNone, NullStandinNotNull), Literal(int64(10))),
	} {
		_, err := KeyExpressionFromProto((&FunctionKeyExpression{name: FunctionNamePartitionV1, arguments: arguments}).ToKeyExpression())
		var typed *KeyExpressionDeserializationError
		var cause *KeyExpressionError
		var core *RecordCoreError
		if !errors.As(err, &typed) || !errors.As(err, &cause) || !errors.As(err, &core) || typed.Cause != cause {
			t.Fatalf("malformed arguments %v lost typed validation cause: %v", arguments.ToKeyExpression(), err)
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

// Serialize deliberately invalid builder state without invoking Build's guards.
func partitionMetadataProtoUnchecked(t testing.TB, b *RecordMetaDataBuilder) *gen.MetaData {
	t.Helper()
	md := &RecordMetaData{
		recordTypes: b.recordTypes, fileDescriptor: b.fileDescriptor, unionDescriptor: b.unionDescriptor,
		indexes: b.indexes, universalIndexes: b.universalIndexes, version: b.version,
		recordCountKey: b.recordCountKey,
	}
	wire, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestPartitionCanonicalGuardsAtBuildAndLoad(t *testing.T) {
	t.Parallel()
	p := func() KeyExpression { return PartitionExpr(Field("coord_x"), 10) }
	window := func(t testing.TB, idx *Index) *Index {
		t.Helper()
		if err := idx.SetPredicateProto(&gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"}, Size: proto.Int32(2), Direction: gen.RowNumberWindowPredicate_ASC.Enum(),
		}}); err != nil {
			t.Fatal(err)
		}
		return idx
	}
	for _, tc := range []struct {
		name, recordType, indexName, message string
		configure                            func(testing.TB, *RecordMetaDataBuilder)
	}{
		{"universal", "Order", "", "universal and multi-type indexes are not supported", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddUniversalIndex(NewIndex("bad", Field("price")))
		}},
		{"multi-type", "Order", "", "universal and multi-type indexes are not supported", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddMultiTypeIndex([]string{"Order", "TypedRecord"}, NewIndex("bad", Field("price")))
		}},
		{"nonleading discriminator", "Order", "bad", "single-table local indexes must not repeat the record type discriminator", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", Concat(p(), RecordTypeKey(), Field("price"))))
		}},
		{"duplicate PK coordinate", "Order", "", "primary key must be (RecordTypeKey, partition function, logical key...)", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), p(), p(), Field("coord_x"), Field("order_id")))
		}},
		{"duplicate index coordinate", "Order", "bad", "local index must begin with the identical partition function", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", Concat(p(), p(), Field("price"))))
		}},
		{"coordinate in values", "Order", "bad", "covering index must keep the partition coordinate in its key", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", KeyWithValue(Concat(p(), Field("price")), 0)))
		}},
		{"windowed VALUE", "Order", "bad", "windowed indexes are not supported", func(t testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", window(t, NewIndex("bad", Concat(p(), Field("price")))))
		}},
		{"windowed TEXT", "Order", "bad", "windowed indexes are not supported", func(t testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", window(t, NewTextIndex("bad", GroupBy(Nest("flower", Field("type")), p()))))
		}},
		{"covering source only in values", "Order", "bad", "covering index cannot trim a primary key column from included values", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", KeyWithValue(Concat(p(), Field("price"), Field("coord_x")), 2)))
		}},
		{"covering other PK only in values", "Order", "bad", "covering index cannot trim a primary key column from included values", func(_ testing.TB, b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("bad", KeyWithValue(Concat(p(), Field("price"), Field("order_id")), 2)))
		}},
		{"tokenized PK", "TypedRecord", "bad", "TEXT index cannot trim a primary key column from the tokenized body", func(_ testing.TB, b *RecordMetaDataBuilder) {
			q := PartitionExpr(Field("val_int64"), 10)
			b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), q, Field("val_int64"), Field("val_string")))
			b.AddIndex("TypedRecord", NewTextIndex("bad", GroupBy(Field("val_string"), q)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := partitionTestBuilder()
			tc.configure(t, b)
			wire := partitionMetadataProtoUnchecked(t, b)
			_, buildErr := b.Build()
			_, loadErr := RecordMetaDataFromProto(wire)
			for label, err := range map[string]error{"Build": buildErr, "Load": loadErr} {
				var typed *PartitionMetadataError
				if !errors.As(err, &typed) || typed.Message != tc.message || typed.RecordType != tc.recordType || typed.IndexName != tc.indexName {
					t.Fatalf("%s = %v, want PartitionMetadataError(%q,%q,%q)", label, err, tc.recordType, tc.indexName, tc.message)
				}
			}
		})
	}
	// These controls prove the ordinary descriptor/window validators permit
	// the shapes that the partition-specific restrictions refuse above.
	for _, kind := range []string{"universal", "multi-type", "windowed VALUE", "windowed TEXT", "covering value-only PK", "tokenized PK"} {
		t.Run("plain control "+kind, func(t *testing.T) {
			t.Parallel()
			b := partitionTestBuilder()
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("coord_x"), Field("order_id")))
			b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("val_int64"), Field("id")))
			switch kind {
			case "universal":
				b.AddUniversalIndex(NewIndex("control", Field("price")))
			case "multi-type":
				b.AddMultiTypeIndex([]string{"Order", "TypedRecord"}, NewIndex("control", Field("price")))
			case "windowed VALUE":
				b.AddIndex("Order", window(t, NewIndex("control", Field("price"))))
			case "windowed TEXT":
				b.AddIndex("Order", window(t, NewTextIndex("control", Nest("flower", Field("type")))))
			case "covering value-only PK":
				b.AddIndex("Order", NewIndex("control", KeyWithValue(Concat(Field("price"), Field("coord_x")), 1)))
			case "tokenized PK":
				b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("val_int64"), Field("val_string")))
				b.AddIndex("TypedRecord", NewTextIndex("control", Field("val_string")))
			}
			md, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			wire, err := md.ToProto()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecordMetaDataFromProto(wire); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPartitionNotNullSourceAndMetadataCauses(t *testing.T) {
	t.Parallel()
	b := partitionTestBuilder()
	p := PartitionExpr(FieldWithNullStandin("coord_x", FanTypeNone, NullStandinNotNull), 10)
	b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), p, Field("coord_x"), Field("order_id")))
	wire := partitionMetadataProtoUnchecked(t, b)
	_, buildErr := b.Build()
	_, loadErr := RecordMetaDataFromProto(wire)
	for label, err := range map[string]error{"Build": buildErr, "Load": loadErr} {
		var key *KeyExpressionError
		if !errors.As(err, &key) || key.Message != "partition source must be an ordinary scalar field" {
			t.Fatalf("%s = %v, want the NOT_NULL source guard", label, err)
		}
	}
	var decoded *KeyExpressionDeserializationError
	if !errors.As(loadErr, &decoded) || decoded.Cause == nil {
		t.Fatalf("metadata load lost the key-expression validation cause: %v", loadErr)
	}
	bad := PartitionExpr(Field("coord_x"), 0)
	rt := &RecordType{Name: "Order", PrimaryKey: Concat(RecordTypeKey(), bad, Field("coord_x"), Field("order_id"))}
	_, err := rt.PartitionSpec()
	var metadata *PartitionMetadataError
	var key *KeyExpressionError
	if !errors.As(err, &metadata) || !errors.As(err, &key) || metadata.Cause != key || key.Message != "partition interval must be a positive BIGINT literal" {
		t.Fatalf("derived metadata lost its validation cause: %v", err)
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
			b.AddIndex("Order", NewIndex("bad", Concat(p(), RecordTypeKey(), Field("price"))))
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
		{"universal", func(b *RecordMetaDataBuilder) { b.AddUniversalIndex(NewIndex("bad", Field("price"))) }},
		{"multi-type", func(b *RecordMetaDataBuilder) {
			b.AddMultiTypeIndex([]string{"Order", "TypedRecord"}, NewIndex("bad", Field("price")))
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
