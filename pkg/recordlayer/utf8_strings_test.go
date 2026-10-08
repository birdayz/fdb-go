package recordlayer

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func utf8CheckFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	const (
		str = descriptorpb.FieldDescriptorProto_TYPE_STRING
		i64 = descriptorpb.FieldDescriptorProto_TYPE_INT64
		msg = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	)
	field := func(name string, number int32, label *descriptorpb.FieldDescriptorProto_Label, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	entry := func(name string, key, value descriptorpb.FieldDescriptorProto_Type, valueType string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name:    proto.String(name),
			Field:   []*descriptorpb.FieldDescriptorProto{field("key", 1, optional, key, ""), field("value", 2, optional, value, valueType)},
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
		}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("utf8_check.proto"),
		Package: proto.String("utf8check"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Rec"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, optional, i64, ""),
					field("s", 2, optional, str, ""),
					field("rs", 3, repeated, str, ""),
					field("mk", 4, repeated, msg, ".utf8check.Rec.MkEntry"),
					field("mv", 5, repeated, msg, ".utf8check.Rec.MvEntry"),
					field("mm", 6, repeated, msg, ".utf8check.Rec.MmEntry"),
					field("inner", 7, optional, msg, ".utf8check.Inner"),
					field("ri", 8, repeated, msg, ".utf8check.Inner"),
					field("ns", 9, optional, msg, ".utf8check.NoStr"),
				},
				NestedType: []*descriptorpb.DescriptorProto{
					entry("MkEntry", str, i64, ""),
					entry("MvEntry", i64, str, ""),
					entry("MmEntry", i64, msg, ".utf8check.Inner"),
				},
			},
			{Name: proto.String("Inner"), Field: []*descriptorpb.FieldDescriptorProto{
				field("t", 1, optional, str, ""),
				field("deep", 2, optional, msg, ".utf8check.Inner"),
			}},
			{Name: proto.String("NoStr"), Field: []*descriptorpb.FieldDescriptorProto{
				field("x", 1, optional, i64, ""),
				field("loop", 2, optional, msg, ".utf8check.NoStr"),
			}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

// Every place a string can sit — a field, a repeated element, a map key, a
// map value, a message map value, a nested and a repeated message — is
// checked, and the error names its path.
func TestCheckUTF8Strings_EveryArm(t *testing.T) {
	t.Parallel()
	file := utf8CheckFile(t)
	rec := file.Messages().ByName("Rec")
	inner := file.Messages().ByName("Inner")
	reach := newStringReach(rec)
	rt := &RecordType{Name: "Rec", Descriptor: rec, unionFieldNumber: 1, reachesString: reach.reaches(rec), stringReach: reach}
	str := protoreflect.ValueOfString
	innerWith := func(m protoreflect.Message, text string) protoreflect.Value {
		v := dynamicpb.NewMessage(inner)
		v.Set(inner.Fields().ByName("t"), str(text))
		return protoreflect.ValueOfMessage(v)
	}
	field := func(name protoreflect.Name) protoreflect.FieldDescriptor { return rec.Fields().ByName(name) }
	for _, c := range []struct {
		path string
		fill func(m protoreflect.Message, text string)
	}{
		{"s", func(m protoreflect.Message, text string) { m.Set(field("s"), str(text)) }},
		{"rs", func(m protoreflect.Message, text string) {
			l := m.Mutable(field("rs")).List()
			l.Append(str("fine"))
			l.Append(str(text))
		}},
		{"mk", func(m protoreflect.Message, text string) {
			m.Mutable(field("mk")).Map().Set(str(text).MapKey(), protoreflect.ValueOfInt64(1))
		}},
		{"mv", func(m protoreflect.Message, text string) {
			m.Mutable(field("mv")).Map().Set(protoreflect.ValueOfInt64(1).MapKey(), str(text))
		}},
		{"mm.t", func(m protoreflect.Message, text string) {
			m.Mutable(field("mm")).Map().Set(protoreflect.ValueOfInt64(1).MapKey(), innerWith(m, text))
		}},
		{"inner.t", func(m protoreflect.Message, text string) { m.Set(field("inner"), innerWith(m, text)) }},
		{"inner.deep.t", func(m protoreflect.Message, text string) {
			outer := dynamicpb.NewMessage(inner)
			outer.Set(inner.Fields().ByName("deep"), innerWith(m, text))
			m.Set(field("inner"), protoreflect.ValueOfMessage(outer))
		}},
		{"ri.t", func(m protoreflect.Message, text string) {
			l := m.Mutable(field("ri")).List()
			l.Append(innerWith(m, "fine"))
			l.Append(innerWith(m, text))
		}},
	} {
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			valid := dynamicpb.NewMessage(rec)
			c.fill(valid, "naïve 日本")
			if err := rt.checkUTF8Strings(valid); err != nil {
				t.Fatalf("valid text refused: %v", err)
			}
			bad := dynamicpb.NewMessage(rec)
			c.fill(bad, "ok\xffno")
			var e *InvalidUTF8StringError
			if err := rt.checkUTF8Strings(bad); !errors.As(err, &e) || e.Field != c.path {
				t.Fatalf("want InvalidUTF8StringError at %s, got %v", c.path, err)
			}
		})
	}
	// The reach prunes a type that holds no string at any depth, recursive
	// ones included.
	for name, want := range map[protoreflect.Name]bool{"Rec": true, "Inner": true, "NoStr": false} {
		if got := reach.reaches(file.Messages().ByName(name)); got != want {
			t.Errorf("reaches(%s) = %t, want %t", name, got, want)
		}
	}
}
