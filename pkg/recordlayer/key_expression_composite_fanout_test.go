package recordlayer

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// referenceEvaluate is expr's tuples by the semantics the single-pass
// evaluations must keep: a composite's are the cross product of its
// children's, in child order with the earlier child most significant; a
// fan-out nesting over a repeated message field's are its child's over each
// element in turn; both through this reference, so neither
// CompositeKeyExpression.Evaluate nor NestingKeyExpression.evaluateRepeated
// computes them. Any other expression's are its own Evaluate.
func referenceEvaluate(expr KeyExpression, record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	switch e := expr.(type) {
	case *CompositeKeyExpression:
		result := [][]any{{}}
		for _, child := range e.expressions {
			childTuples, err := referenceEvaluate(child, record, msg)
			if err != nil {
				return nil, err
			}
			var crossed [][]any
			for _, existing := range result {
				for _, tup := range childTuples {
					crossed = append(crossed, append(append([]any(nil), existing...), tup...))
				}
			}
			result = crossed
		}
		return result, nil
	case *NestingKeyExpression:
		if e.fanType != FanTypeFanOut || msg == nil {
			break
		}
		m := msg.ProtoReflect()
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(e.parentField))
		if fd == nil || !fd.IsList() || fd.Message() == nil {
			break
		}
		var result [][]any
		list := m.Get(fd).List()
		for i := 0; i < list.Len(); i++ {
			tuples, err := referenceEvaluate(e.child, record, list.Get(i).Message().Interface())
			if err != nil {
				return nil, err
			}
			result = append(result, tuples...)
		}
		return result, nil
	}
	return expr.Evaluate(record, msg)
}

func compositeFanoutDescriptors(t *testing.T) (rec, elem protoreflect.MessageDescriptor) {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, n int32, label *descriptorpb.FieldDescriptorProto_Label, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Label: label, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	str, i64, msg := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("composite_fanout_test.proto"), Package: proto.String("cft"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Elem"), Field: []*descriptorpb.FieldDescriptorProto{field("a", 1, opt, str, ""), field("b", 2, opt, i64, "")}},
			{Name: proto.String("Rec"), Field: []*descriptorpb.FieldDescriptorProto{
				field("x", 1, opt, i64, ""), field("e", 2, rep, msg, ".cft.Elem"), field("f", 3, rep, msg, ".cft.Elem"),
				field("y", 4, opt, str, ""), field("tags", 5, rep, str, ""),
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName("Rec"), fd.Messages().ByName("Elem")
}

// CompositeKeyExpression.Evaluate evaluates each child once and builds the
// cross product of fan-out children directly, and a fan-out nesting takes a
// flat child's EvaluateFlat per element. Their tuples, their order, and an
// empty fan-out's empty result are the reference's (referenceEvaluate), for
// zero to three elements per repeated field, absent fields, two fan-outs,
// a repeated scalar, nested composites and a fan-out nesting root.
func TestCompositeKeyExpressionFanOutMatchesReferenceCrossProduct(t *testing.T) {
	t.Parallel()
	recD, elemD := compositeFanoutDescriptors(t)
	exprs := map[string]KeyExpression{
		"scalars only":              Concat(Field("x"), Field("y")),
		"fan-out between scalars":   Concat(Field("x"), NestFanOut("e", Concat(Field("a"), Field("b"))), Field("y")),
		"leading fan-out":           Concat(NestFanOut("e", Field("a")), Field("x"), Field("y")),
		"trailing fan-out":          Concat(Field("x"), Field("y"), NestFanOut("f", Field("b"))),
		"two fan-outs":              Concat(NestFanOut("e", Field("a")), Field("x"), NestFanOut("f", Concat(Field("b"), Field("a")))),
		"repeated scalar fan-out":   Concat(FanOut("tags"), Field("x")),
		"nested composite fan-out":  Concat(Concat(Field("x"), NestFanOut("e", Field("b"))), Field("y")),
		"three fan-outs":            Concat(FanOut("tags"), NestFanOut("e", Field("a")), NestFanOut("f", Field("a"))),
		"fan-out child of fan-out":  NestFanOut("e", Concat(Field("a"), Field("b"))),
		"concatenated repeated tag": Concat(FieldConcatenate("tags"), NestFanOut("e", Field("a"))),
	}
	rng := rand.New(rand.NewSource(283))
	elem := func(i int) protoreflect.Message {
		m := dynamicpb.NewMessage(elemD)
		if rng.Intn(4) != 0 {
			m.Set(elemD.Fields().ByName("a"), protoreflect.ValueOfString(fmt.Sprintf("a%d", i)))
		}
		if rng.Intn(4) != 0 {
			m.Set(elemD.Fields().ByName("b"), protoreflect.ValueOfInt64(int64(i)))
		}
		return m
	}
	for iter := 0; iter < 400; iter++ {
		m := dynamicpb.NewMessage(recD)
		f := recD.Fields()
		if rng.Intn(3) != 0 {
			m.Set(f.ByName("x"), protoreflect.ValueOfInt64(rng.Int63n(100)))
		}
		if rng.Intn(3) != 0 {
			m.Set(f.ByName("y"), protoreflect.ValueOfString(fmt.Sprintf("y%d", iter)))
		}
		for _, name := range []protoreflect.Name{"e", "f"} {
			list := m.Mutable(f.ByName(name)).List()
			for i, n := 0, rng.Intn(4); i < n; i++ {
				list.Append(protoreflect.ValueOfMessage(elem(i)))
			}
		}
		tags := m.Mutable(f.ByName("tags")).List()
		for i, n := 0, rng.Intn(4); i < n; i++ {
			tags.Append(protoreflect.ValueOfString(fmt.Sprintf("t%d", i)))
		}
		record := asStored(m)
		for name, expr := range exprs {
			got, gotErr := expr.Evaluate(record, m)
			want, wantErr := referenceEvaluate(expr, record, m)
			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("%s, iteration %d: error %v, reference %v", name, iter, gotErr, wantErr)
			}
			if len(got) == 0 && len(want) == 0 {
				continue // nil and empty both mean no entries
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s, iteration %d:\n got %v\nwant %v", name, iter, got, want)
			}
			for i, tup := range got {
				if cap(tup) != len(tup) && len(got) > 1 {
					t.Fatalf("%s, iteration %d: tuple %d has spare capacity %d > %d: appending to it could overwrite another tuple", name, iter, i, cap(tup), len(tup))
				}
			}
		}
	}
}

// An empty repeated field under a fan-out yields no tuples at all, never one
// tuple of the other children's values (which would index a record whose
// array is empty).
func TestCompositeKeyExpressionEmptyFanOutYieldsNoTuples(t *testing.T) {
	t.Parallel()
	recD, _ := compositeFanoutDescriptors(t)
	m := dynamicpb.NewMessage(recD)
	m.Set(recD.Fields().ByName("x"), protoreflect.ValueOfInt64(7))
	m.Set(recD.Fields().ByName("y"), protoreflect.ValueOfString("why"))
	for _, expr := range []KeyExpression{
		Concat(Field("x"), NestFanOut("e", Field("a")), Field("y")),
		Concat(NestFanOut("e", Field("a")), Field("x")),
		Concat(Field("x"), FanOut("tags")),
		Concat(Concat(Field("x"), NestFanOut("f", Field("b"))), Field("y")),
	} {
		got, err := expr.Evaluate(asStored(m), m)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("empty fan-out evaluated to %v, want no tuples", got)
		}
	}
}
