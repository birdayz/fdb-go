package recordlayer

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
)

func varintField(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}

// protobuf-go keeps a closed enum's undeclared number in the field;
// closedEnumsAsJava moves it where protobuf-java's parser puts it, at every
// kind of field that can hold one, and in the unknown-field order Java writes.
func TestClosedEnumsAsJava(t *testing.T) {
	t.Parallel()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	enum := descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
	message := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("closed_enums.proto"), Package: proto.String("ce"), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{Name: proto.String("E"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("A"), Number: proto.Int32(1)}, {Name: proto.String("B"), Number: proto.Int32(2)},
			}},
			// A map's enum value type must declare 0 first.
			{Name: proto.String("F"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("F0"), Number: proto.Int32(0)}, {Name: proto.String("F1"), Number: proto.Int32(1)},
			}},
		},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("M"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("e"), Number: proto.Int32(1), Label: optional, Type: enum, TypeName: proto.String(".ce.E")},
				{Name: proto.String("r"), Number: proto.Int32(2), Label: repeated, Type: enum, TypeName: proto.String(".ce.E")},
				{Name: proto.String("child"), Number: proto.Int32(3), Label: optional, Type: message, TypeName: proto.String(".ce.M")},
				{Name: proto.String("m"), Number: proto.Int32(4), Label: repeated, Type: message, TypeName: proto.String(".ce.M.MEntry")},
				{Name: proto.String("x"), Number: proto.Int32(5), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
			},
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("MEntry"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("key"), Number: proto.Int32(1), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("value"), Number: proto.Int32(2), Label: optional, Type: enum, TypeName: proto.String(".ce.F")},
				},
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	md := fd.Messages().ByName("M")
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	entry := protowire.AppendBytes(protowire.AppendTag(nil, 4, protowire.BytesType),
		cat(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k"), varintField(2, 7)))
	child := protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), varintField(1, 9))
	// e 7, r [A, 8, B], child.e 9, m {"k": 7}, x 5, and unknown fields 1 (as
	// a fixed32, a wire type the field does not take) and 30.
	raw := cat(varintField(1, 7), varintField(2, 1), varintField(2, 8), varintField(2, 2), child, entry, varintField(5, 5),
		protowire.AppendFixed32(protowire.AppendTag(nil, 1, protowire.Fixed32Type), 4), varintField(30, 1))
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(raw, m); err != nil {
		t.Fatal(err)
	}
	if got := m.Get(md.Fields().ByName("e")).Enum(); got != 7 {
		t.Fatalf("protobuf-go's parse no longer keeps the number in the field (e=%d); revisit proto_closed_enums.go", got)
	}
	closedEnumsAsJava(m, newClosedEnumReach(false, md), false)

	if m.Has(md.Fields().ByName("e")) {
		t.Error("e is still set")
	}
	r := m.Get(md.Fields().ByName("r")).List()
	if r.Len() != 2 || r.Get(0).Enum() != 1 || r.Get(1).Enum() != 2 {
		t.Errorf("r keeps its declared elements in order; got %d elements", r.Len())
	}
	c := m.Get(md.Fields().ByName("child")).Message()
	if c.Has(md.Fields().ByName("e")) || !bytes.Equal(c.GetUnknown(), varintField(1, 9)) {
		t.Errorf("child: e set %t, unknown %x", c.Has(md.Fields().ByName("e")), c.GetUnknown())
	}
	mp := m.Get(md.Fields().ByName("m")).Map()
	if v := mp.Get(protoreflect.ValueOfString("k").MapKey()); v.Enum() != 0 {
		t.Errorf("the map value reads the default F0, got %d", v.Enum())
	}
	// Java's UnknownFieldSet order: by number, a field's varints before its
	// fixed32s.
	want := cat(varintField(1, 7), protowire.AppendFixed32(protowire.AppendTag(nil, 1, protowire.Fixed32Type), 4), varintField(2, 8), varintField(30, 1))
	if got := m.GetUnknown(); !bytes.Equal(got, want) {
		t.Errorf("unknown fields %x, want %x", got, want)
	}

	// A message with nothing to move is left as it is.
	plain := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(cat(varintField(1, 2), varintField(30, 1)), plain); err != nil {
		t.Fatal(err)
	}
	closedEnumsAsJava(plain, newClosedEnumReach(false, md), false)
	if plain.Get(md.Fields().ByName("e")).Enum() != 2 || !bytes.Equal(plain.GetUnknown(), varintField(30, 1)) {
		t.Error("a declared number was moved")
	}
}

// The stored protos Go decodes read as Java reads them: a store header whose
// record-count state Java cannot read is its default, READABLE, and a key
// expression's fan type 7 is a missing fan type.
func TestStoredProtosReadAsJava(t *testing.T) {
	t.Parallel()
	var header gen.DataStoreInfo
	if err := UnmarshalVTAsJava(&header, varintField(9, 7)); err != nil {
		t.Fatal(err)
	}
	if header.RecordCountState != nil || header.GetRecordCountState() != gen.DataStoreInfo_READABLE {
		t.Errorf("record count state %v, want unset (READABLE)", header.GetRecordCountState())
	}
	if !bytes.Equal(header.ProtoReflect().GetUnknown(), varintField(9, 7)) {
		t.Errorf("the number is kept as an unknown field: %x", header.ProtoReflect().GetUnknown())
	}

	field := append(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "f"), varintField(2, 7)...)
	var f gen.Field
	if err := proto.Unmarshal(field, &f); err != nil {
		t.Fatal(err)
	}
	_, err := KeyExpressionFromProto(&gen.KeyExpression{Field: &f})
	var de *KeyExpressionDeserializationError
	if !errors.As(err, &de) || de.Message != "Serialized Field is missing fan type" {
		t.Errorf("fan type 7 through protobuf-go's parse: %v", err)
	}
}

// javaDecodeFile is a proto2 file with a closed enum E (A=1, B=2) in every
// position the decoder reads: optional, required, repeated (packed or not), a
// oneof member beside a string member, a nested message, a group, and map
// values of F (F0=0, F2=2; a map's enum declares 0 first) and of the message.
func javaDecodeFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	required := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum()
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	enum := descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
	message := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	entry := func(name, typeName string, typ *descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("key"), Number: proto.Int32(1), Label: optional, Type: str},
				{Name: proto.String("value"), Number: proto.Int32(2), Label: optional, Type: typ, TypeName: proto.String(typeName)},
			},
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
		}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("java_decode.proto"), Package: proto.String("jd"), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{Name: proto.String("E"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("A"), Number: proto.Int32(1)}, {Name: proto.String("B"), Number: proto.Int32(2)},
			}},
			// A map's enum value type declares 0 first.
			{Name: proto.String("F"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("F0"), Number: proto.Int32(0)}, {Name: proto.String("F2"), Number: proto.Int32(2)},
			}},
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("M"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("e"), Number: proto.Int32(1), Label: optional, Type: enum, TypeName: proto.String(".jd.E")},
					{Name: proto.String("r"), Number: proto.Int32(2), Label: repeated, Type: enum, TypeName: proto.String(".jd.E")},
					{Name: proto.String("child"), Number: proto.Int32(3), Label: optional, Type: message, TypeName: proto.String(".jd.M")},
					{Name: proto.String("s"), Number: proto.Int32(4), Label: optional, Type: str, OneofIndex: proto.Int32(0)},
					{Name: proto.String("o"), Number: proto.Int32(5), Label: optional, Type: enum, TypeName: proto.String(".jd.E"), OneofIndex: proto.Int32(0)},
					{Name: proto.String("g"), Number: proto.Int32(6), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_GROUP.Enum(), TypeName: proto.String(".jd.M.G")},
					{Name: proto.String("me"), Number: proto.Int32(7), Label: repeated, Type: message, TypeName: proto.String(".jd.M.MeEntry")},
					{Name: proto.String("mm"), Number: proto.Int32(8), Label: repeated, Type: message, TypeName: proto.String(".jd.M.MmEntry")},
					{Name: proto.String("kids"), Number: proto.Int32(9), Label: repeated, Type: message, TypeName: proto.String(".jd.M")},
				},
				NestedType: []*descriptorpb.DescriptorProto{
					{Name: proto.String("G"), Field: []*descriptorpb.FieldDescriptorProto{
						{Name: proto.String("ge"), Number: proto.Int32(1), Label: optional, Type: enum, TypeName: proto.String(".jd.E")},
					}},
					entry("MeEntry", ".jd.F", enum),
					entry("MmEntry", ".jd.M", message),
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("choice")}},
			},
			{
				Name: proto.String("Req"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("need"), Number: proto.Int32(1), Label: required, Type: enum, TypeName: proto.String(".jd.E")},
				},
			},
			{
				Name: proto.String("Deep"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("next"), Number: proto.Int32(1), Label: optional, Type: message, TypeName: proto.String(".jd.Deep")},
					{Name: proto.String("e"), Number: proto.Int32(2), Label: optional, Type: enum, TypeName: proto.String(".jd.E")},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func bytesField(num protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), b)
}

// Bytes holding a closed enum's undeclared number decode as protobuf-java's
// parser reads them (MessageReflection.mergeFieldFrom), occurrence by
// occurrence: each case's bytes, its Java reading, and what protobuf-go's own
// decode followed by an in-memory move (the reading before) gave instead.
func TestJavaDecodeRuleReadsOccurrencesAsJava(t *testing.T) {
	t.Parallel()
	file := javaDecodeFile(t)
	md := file.Messages().ByName("M")
	f := md.Fields()
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	decode := func(t *testing.T, rule javaDecodeRule, b []byte) *dynamicpb.Message {
		t.Helper()
		m := dynamicpb.NewMessage(md)
		if err := rule.unmarshal(b, m, newClosedEnumReach(false, md)); err != nil {
			t.Fatal(err)
		}
		return m
	}
	t.Run("the last declared occurrence wins", func(t *testing.T) {
		// Go's decode keeps the last occurrence, 7, and the move then left the
		// field unset; Java sets A, and keeps 7 unknown.
		m := decode(t, javaRecordRule, cat(varintField(1, 1), varintField(1, 7)))
		if got := m.Get(f.ByName("e")).Enum(); !m.Has(f.ByName("e")) || got != 1 {
			t.Errorf("e = %d (set %t), want A", got, m.Has(f.ByName("e")))
		}
		if !bytes.Equal(m.GetUnknown(), varintField(1, 7)) {
			t.Errorf("unknown %x", m.GetUnknown())
		}
		m = decode(t, javaRecordRule, cat(varintField(1, 7), varintField(1, 2)))
		if got := m.Get(f.ByName("e")).Enum(); got != 2 || !bytes.Equal(m.GetUnknown(), varintField(1, 7)) {
			t.Errorf("e = %d, unknown %x; want B and 7", got, m.GetUnknown())
		}
	})
	t.Run("an undeclared oneof member leaves its sibling set", func(t *testing.T) {
		m := decode(t, javaRecordRule, cat(bytesField(4, []byte("x")), varintField(5, 9)))
		if got := m.WhichOneof(md.Oneofs().ByName("choice")); got == nil || got.Name() != "s" {
			t.Fatalf("the oneof holds %v, want s", got)
		}
		if m.Get(f.ByName("s")).String() != "x" || !bytes.Equal(m.GetUnknown(), varintField(5, 9)) {
			t.Errorf("s %q, unknown %x", m.Get(f.ByName("s")).String(), m.GetUnknown())
		}
	})
	t.Run("a packed list keeps its declared elements in order", func(t *testing.T) {
		packed := protowire.AppendVarint(protowire.AppendVarint(protowire.AppendVarint(nil, 2), 8), 1)
		m := decode(t, javaRecordRule, bytesField(2, packed))
		r := m.Get(f.ByName("r")).List()
		if r.Len() != 2 || r.Get(0).Enum() != 2 || r.Get(1).Enum() != 1 || !bytes.Equal(m.GetUnknown(), varintField(2, 8)) {
			t.Errorf("r has %d elements, unknown %x", r.Len(), m.GetUnknown())
		}
	})
	t.Run("nested messages, list elements and groups are read the same way", func(t *testing.T) {
		group := cat(protowire.AppendTag(nil, 6, protowire.StartGroupType), varintField(1, 7), protowire.AppendTag(nil, 6, protowire.EndGroupType))
		m := decode(t, javaRecordRule, cat(bytesField(3, cat(varintField(1, 1), varintField(1, 7))), bytesField(9, varintField(1, 8)), group))
		child := m.Get(f.ByName("child")).Message()
		if child.Get(f.ByName("e")).Enum() != 1 || !bytes.Equal(child.GetUnknown(), varintField(1, 7)) {
			t.Errorf("child e %d, unknown %x", child.Get(f.ByName("e")).Enum(), child.GetUnknown())
		}
		kid := m.Get(f.ByName("kids")).List().Get(0).Message()
		if kid.Has(f.ByName("e")) || !bytes.Equal(kid.GetUnknown(), varintField(1, 8)) {
			t.Errorf("kids[0] e set %t, unknown %x", kid.Has(f.ByName("e")), kid.GetUnknown())
		}
		g := m.Get(f.ByName("g")).Message()
		if g.Has(g.Descriptor().Fields().ByName("ge")) || !bytes.Equal(g.GetUnknown(), varintField(1, 7)) {
			t.Errorf("group ge set, unknown %x", g.GetUnknown())
		}
	})
	t.Run("a map entry", func(t *testing.T) {
		k := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k")
		// A DynamicMessage entry keeps its last declared value (F2), else the
		// default (F0).
		m := decode(t, javaRecordRule, cat(bytesField(7, cat(k, varintField(2, 2), varintField(2, 7))), bytesField(7, cat(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "z"), varintField(2, 9)))))
		me := m.Get(f.ByName("me")).Map()
		if v := me.Get(protoreflect.ValueOfString("k").MapKey()); v.Enum() != 2 {
			t.Errorf("me[k] = %d, want F2", v.Enum())
		}
		if v := me.Get(protoreflect.ValueOfString("z").MapKey()); !v.IsValid() || v.Enum() != 0 {
			t.Errorf("me[z] = %v, want the default F0", v)
		}
		// A generated class's entry whose last value is undeclared goes whole
		// to the unknown fields.
		rule := javaRecordRule
		rule.generated = true
		occ := bytesField(7, cat(k, varintField(2, 7)))
		m = decode(t, rule, occ)
		if m.Get(f.ByName("me")).Map().Len() != 0 || !bytes.Equal(m.GetUnknown(), occ) {
			t.Errorf("generated: %d entries, unknown %x", m.Get(f.ByName("me")).Map().Len(), m.GetUnknown())
		}
		// A message value is read as merge reads a message.
		m = decode(t, javaRecordRule, bytesField(8, cat(k, bytesField(2, cat(varintField(1, 1), varintField(1, 7))))))
		v := m.Get(f.ByName("mm")).Map().Get(protoreflect.ValueOfString("k").MapKey()).Message()
		if v.Get(f.ByName("e")).Enum() != 1 || !bytes.Equal(v.GetUnknown(), varintField(1, 7)) {
			t.Errorf("mm[k].e %d, unknown %x", v.Get(f.ByName("e")).Enum(), v.GetUnknown())
		}
	})
	t.Run("a required closed enum holding only an undeclared number is missing", func(t *testing.T) {
		req := file.Messages().ByName("Req")
		m := dynamicpb.NewMessage(req)
		err := javaRecordRule.unmarshal(varintField(1, 7), m, newClosedEnumReach(false, req))
		if err == nil || !strings.Contains(err.Error(), "required field") {
			t.Errorf("err = %v, want the required-field refusal protobuf-java's buildParsed raises", err)
		}
		// Before, protobuf-go's decode passed its required check with 7 in the
		// field; the move then left it unset with nothing checking again.
		m = dynamicpb.NewMessage(req)
		if err := proto.Unmarshal(varintField(1, 7), m); err != nil {
			t.Fatalf("protobuf-go's own decode refuses the number now (%v); revisit proto_closed_enums.go", err)
		}
		if err := javaPartialRule.unmarshal(varintField(1, 7), dynamicpb.NewMessage(req), newClosedEnumReach(false, req)); err != nil {
			t.Errorf("a partial decode skips the check: %v", err)
		}
	})
}

// protobuf-go counts the root message against RecursionLimit and
// protobuf-java counts only nested ones (CodedInputStream.checkRecursionLimit,
// 100), so a root Java parses admits 101 levels and a record, one level below
// Java's union, 100. Both paths are driven: bytes without an undeclared
// number (protobuf-go's decode) and with one at the deepest level (merge).
func TestJavaDecodeRuleRecursionLimit(t *testing.T) {
	t.Parallel()
	deep := javaDecodeFile(t).Messages().ByName("Deep")
	nest := func(levels int, leafEnum uint64) []byte {
		b := varintField(2, leafEnum)
		for i := 1; i < levels; i++ {
			b = bytesField(1, b)
		}
		return b
	}
	for _, c := range []struct {
		rule   javaDecodeRule
		name   string
		admits int
	}{{javaRootRule, "root", 101}, {javaRecordRule, "record", 100}} {
		for _, leaf := range []uint64{1, 7} {
			for _, levels := range []int{c.admits, c.admits + 1} {
				err := c.rule.unmarshal(nest(levels, leaf), dynamicpb.NewMessage(deep), newClosedEnumReach(false, deep))
				if (err == nil) != (levels <= c.admits) {
					t.Errorf("%s, %d levels, leaf %d: err = %v, want admitted iff levels <= %d", c.name, levels, leaf, err, c.admits)
				}
			}
		}
	}
}

// Java reads a proto2 file's field of an open enum as closed when the file has
// dependencies (legacy_closed_enum defaults to true for proto2,
// Descriptors.legacyEnumFieldTreatedAsClosed); protobuf-go's enum reports
// open. An editions file resolves the java feature where it is set.
func TestClosedEnumFieldIsJavasLegacyClosedEnum(t *testing.T) {
	t.Parallel()
	open, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("open.proto"), Package: proto.String("op"), Syntax: proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("O"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("O0"), Number: proto.Int32(0)},
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := new(protoregistry.Files)
	if err := files.RegisterFile(open); err != nil {
		t.Fatal(err)
	}
	javaFeatures := func(v bool) *descriptorpb.FeatureSet {
		fs := &descriptorpb.FeatureSet{}
		java := varintField(1, map[bool]uint64{false: 0, true: 1}[v])
		fs.ProtoReflect().SetUnknown(bytesField(javaFeaturesNumber, java))
		return fs
	}
	field := func(syntax string, edition *descriptorpb.Edition, opts *descriptorpb.FieldOptions) protoreflect.FieldDescriptor {
		fdp := &descriptorpb.FileDescriptorProto{
			Name: proto.String("uses_" + syntax + ".proto"), Package: proto.String("u"), Syntax: proto.String(syntax), Edition: edition,
			Dependency: []string{"open.proto"},
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("M"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("o"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), TypeName: proto.String(".op.O"), Options: opts,
			}}}},
		}
		fd, err := protodesc.NewFile(fdp, files)
		if err != nil {
			t.Fatalf("%s: %v", syntax, err)
		}
		return fd.Messages().ByName("M").Fields().ByName("o")
	}
	if fd := field("proto2", nil, nil); fd.Enum().IsClosed() || !closedEnumField(fd) {
		t.Errorf("proto2 field of an open enum: enum closed %t, field closed %t; want false, true", fd.Enum().IsClosed(), closedEnumField(fd))
	}
	ed := descriptorpb.Edition_EDITION_2023.Enum()
	if fd := field("editions", ed, nil); closedEnumField(fd) {
		t.Error("an editions field without the java feature is open")
	}
	if fd := field("editions", ed, &descriptorpb.FieldOptions{Features: javaFeatures(true)}); !closedEnumField(fd) {
		t.Error("legacy_closed_enum set on the field closes it")
	}
}

// A message Go holds in memory with an undeclared number is saved as Java
// reads the bytes Go writes for it: asJava returns a clone with that reading
// and leaves the caller's message as it was.
func TestAsJavaClonesOnlyWhatItMoves(t *testing.T) {
	t.Parallel()
	md := javaDecodeFile(t).Messages().ByName("M")
	rt := &RecordType{Name: "M", Descriptor: md}
	x := newClosedEnumReach(false, md)
	rt.reachesClosedEnum, rt.closedEnumReach = x.reaches(md), x
	e := md.Fields().ByName("e")

	clean := dynamicpb.NewMessage(md)
	clean.Set(e, protoreflect.ValueOfEnum(1))
	if got := rt.asJava(clean); got != proto.Message(clean) {
		t.Error("a message with nothing undeclared is not cloned")
	}
	held := dynamicpb.NewMessage(md)
	held.Set(e, protoreflect.ValueOfEnum(7))
	got := rt.asJava(held).(*dynamicpb.Message)
	if got == held || !held.Has(e) || held.Get(e).Enum() != 7 {
		t.Fatal("the caller's message was changed")
	}
	if got.Has(e) || !bytes.Equal(got.GetUnknown(), varintField(1, 7)) {
		t.Errorf("the clone: e set %t, unknown %x", got.Has(e), got.GetUnknown())
	}
	// The clone's bytes read back, in Java's reading, as the clone.
	wire, err := proto.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	back := dynamicpb.NewMessage(md)
	if err := javaRecordRule.unmarshal(wire, back, x); err != nil || !proto.Equal(back, got) {
		t.Errorf("round trip: %v, equal %t", err, proto.Equal(back, got))
	}
}

// vtproto's decoder has no recursion limit, so UnmarshalVTAsJava hands it only
// a type whose messages cannot nest past protobuf-java's limit; a type that
// nests without bound is decoded by protobuf-go under the limit.
func TestUnmarshalVTAsJavaKeepsJavasRecursionLimit(t *testing.T) {
	t.Parallel()
	// A continuation of fixed depth is bounded; the store header holds a
	// KeyExpression (its record-count key), which nests through Then.
	if !vtBounded((&gen.UnionContinuation{}).ProtoReflect().Descriptor(), javaVTRule.limit) {
		t.Error("UnionContinuation is not bounded")
	}
	for _, m := range []proto.Message{&gen.DataStoreInfo{}, &gen.KeyExpression{}} {
		if vtBounded(m.ProtoReflect().Descriptor(), javaVTRule.limit) {
			t.Fatalf("%T nests through KeyExpression; it is not bounded", m)
		}
	}
	// KeyExpression{then{child: ...}}: 2k+1 levels for k nestings.
	nest := func(k int) []byte {
		var b []byte
		for i := 0; i < k; i++ {
			b = bytesField(1, bytesField(1, b))
		}
		return b
	}
	if err := UnmarshalVTAsJava(&gen.KeyExpression{}, nest(50)); err != nil {
		t.Errorf("101 levels: %v", err)
	}
	if err := UnmarshalVTAsJava(&gen.KeyExpression{}, nest(51)); err == nil {
		t.Error("103 levels were read; protobuf-java refuses them")
	}
	if err := (&gen.KeyExpression{}).UnmarshalVT(nest(51)); err != nil {
		t.Errorf("vtproto's own decoder refuses 103 levels now (%v); revisit vtBounded", err)
	}
}
