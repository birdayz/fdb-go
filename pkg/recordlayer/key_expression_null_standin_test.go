package recordlayer

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func nullStandinRenameDescriptors(t *testing.T) (src, dst protoreflect.MessageDescriptor) {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	scalar := func(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: optional, Type: typ.Enum()}
	}
	message := func(name string, number int32, typeName string) *descriptorpb.FieldDescriptorProto {
		f := scalar(name, number, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE)
		f.TypeName = proto.String(typeName)
		return f
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("null_standin_rename.proto"), Package: proto.String("nsr"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Flower"), Field: []*descriptorpb.FieldDescriptorProto{scalar("type", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING)}},
			{Name: proto.String("Order"), Field: []*descriptorpb.FieldDescriptorProto{
				scalar("price", 3, descriptorpb.FieldDescriptorProto_TYPE_INT32), message("flower", 2, ".nsr.Flower"),
			}},
			{Name: proto.String("Bloom"), Field: []*descriptorpb.FieldDescriptorProto{scalar("kind", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING)}},
			{Name: proto.String("Sale"), Field: []*descriptorpb.FieldDescriptorProto{
				scalar("cost", 3, descriptorpb.FieldDescriptorProto_TYPE_INT32), message("bloom", 2, ".nsr.Bloom"),
			}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName("Order"), fd.Messages().ByName("Sale")
}

// Java's Key.Evaluated.NullStandin on a field (Key.java:394-421,
// FieldKeyExpression.java:205-240). The live-JVM comparison is
// conformance/null_standin_conformance_test.go; these pin each standin's
// evaluation, proto mapping and equality without a cluster.

func TestNullStandin_Evaluate(t *testing.T) {
	t.Parallel()
	packRow := func(row []any) []byte {
		k := make(tuple.Tuple, len(row))
		for i, v := range row {
			k[i] = v
		}
		return k.Pack()
	}
	eval := func(t *testing.T, expr KeyExpression, msg proto.Message) [][]any {
		t.Helper()
		got, err := expr.Evaluate(nil, msg)
		if err != nil {
			t.Fatalf("Evaluate(%v): %v", expr, err)
		}
		return got
	}
	unset := &gen.Order{OrderId: proto.Int64(1)}
	withEmptyFlower := &gen.Order{OrderId: proto.Int64(1), Flower: &gen.Flower{}}
	for _, c := range []struct {
		name string
		expr KeyExpression
		msg  proto.Message
		want [][]any
	}{
		{"NULL: unset is null", FieldWithNullStandin("price", FanTypeNone, NullStandinNull), unset, [][]any{{nil}}},
		{"NULL_UNIQUE: unset is null", FieldWithNullStandin("price", FanTypeNone, NullStandinNullUnique), unset, [][]any{{nil}}},
		{"NOT_NULL: unset is the default", FieldWithNullStandin("price", FanTypeNone, NullStandinNotNull), unset, [][]any{{int64(0)}}},
		{"NOT_NULL: a null message is null", FieldWithNullStandin("price", FanTypeNone, NullStandinNotNull), nil, [][]any{{nil}}},
		{"Concatenate NOT_NULL: a null message is the empty list", FieldWithNullStandin("tags", FanTypeConcatenate, NullStandinNotNull), nil, [][]any{{tuple.Tuple{}}}},
		{"Concatenate NULL: a null message is null", FieldWithNullStandin("tags", FanTypeConcatenate, NullStandinNull), nil, [][]any{{nil}}},
		{"FanOut: a null message has no entries", FieldWithNullStandin("tags", FanTypeFanOut, NullStandinNotNull), nil, nil},
		{
			"NOT_NULL parent: the child reads the default message",
			&NestingKeyExpression{parentField: "flower", fanType: FanTypeNone, child: FieldWithNullStandin("color", FanTypeNone, NullStandinNotNull), parentNullStandin: NullStandinNotNull},
			unset,
			[][]any{{int64(gen.Color_RED)}},
		},
		{
			"NULL parent: the child reads a null message",
			&NestingKeyExpression{parentField: "flower", fanType: FanTypeNone, child: FieldWithNullStandin("color", FanTypeNone, NullStandinNotNull), parentNullStandin: NullStandinNull},
			unset,
			[][]any{{nil}},
		},
		{
			"a present empty parent: a NOT_NULL child is the default",
			Nest("flower", FieldWithNullStandin("color", FanTypeNone, NullStandinNotNull)),
			withEmptyFlower,
			[][]any{{int64(gen.Color_RED)}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := eval(t, c.expr, c.msg)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if string(packRow(got[i])) != string(packRow(c.want[i])) {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestNullStandin_ProtoRoundTrip(t *testing.T) {
	t.Parallel()
	// src and dst number their fields alike under other names, so the rename
	// visitor rebuilds every field and nesting of the expression.
	src, dst := nullStandinRenameDescriptors(t)
	for _, ni := range []gen.Field_NullInterpretation{gen.Field_NOT_UNIQUE, gen.Field_UNIQUE, gen.Field_NOT_NULL} {
		field := func(name string) *gen.Field {
			return &gen.Field{FieldName: proto.String(name), FanType: gen.Field_SCALAR.Enum(), NullInterpretation: ni.Enum()}
		}
		in := &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
			{Field: field("price")},
			{Nesting: &gen.Nesting{Parent: field("flower"), Child: &gen.KeyExpression{Field: field("type")}}},
		}}}
		expr, err := KeyExpressionFromProto(in)
		if err != nil {
			t.Fatalf("%v: %v", ni, err)
		}
		if out := expr.ToKeyExpression(); !proto.Equal(in, out) {
			t.Fatalf("%v: round trip changed the expression:\n in %v\nout %v", ni, in, out)
		}
		renamed, err := renameFields(expr, src, dst)
		if err != nil {
			t.Fatalf("%v: rename: %v", ni, err)
		}
		children := renamed.ToKeyExpression().GetThen().GetChild()
		for _, got := range []gen.Field_NullInterpretation{
			children[0].GetField().GetNullInterpretation(),
			children[1].GetNesting().GetParent().GetNullInterpretation(),
			children[1].GetNesting().GetChild().GetField().GetNullInterpretation(),
		} {
			if got != ni {
				t.Fatalf("%v: rename dropped the standin: %v", ni, renamed.ToKeyExpression())
			}
		}
	}
}

// Java's FieldKeyExpression.equals does not compare the standin
// (FieldKeyExpression.java:406-410), and NestingKeyExpression.equals compares
// the parent with it.
func TestNullStandin_NotPartOfEquality(t *testing.T) {
	t.Parallel()
	for _, s := range []NullStandin{NullStandinNullUnique, NullStandinNotNull} {
		if !keyExpressionEquals(FieldWithNullStandin("price", FanTypeNone, s), Field("price")) {
			t.Fatalf("a field with standin %v is not equal to the default field", s)
		}
		nested := &NestingKeyExpression{parentField: "flower", fanType: FanTypeNone, child: Field("type"), parentNullStandin: s}
		if !keyExpressionEquals(nested, Nest("flower", Field("type"))) {
			t.Fatalf("a nesting whose parent has standin %v is not equal to the default nesting", s)
		}
	}
	if keyExpressionEquals(FieldWithNullStandin("price", FanTypeNone, NullStandinNotNull), Field("quantity")) {
		t.Fatal("fields with different names are equal")
	}
}
