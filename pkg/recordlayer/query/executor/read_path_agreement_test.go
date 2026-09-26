package executor

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// Java funnels EVERY proto field read through ONE helper,
// MessageHelpers.getFieldOnMessage (MessageHelpers.java:124-143), whose FIRST
// branch is `if (field.isRepeated())` — the list is built before any presence
// test, so an empty repeated field reads back as the EMPTY ARRAY and never as
// null (the intent is stated at MessageHelpers.java:591-592).
//
// Go has THREE independent implementations of that one rule:
//
//	1. protoToPositional            (query_result.go)  — the base-scan row
//	2. protoFieldByName             (values.go)        — struct descent, reached
//	                                                     here via FieldValue.Evaluate
//	                                                     over a message context
//	3. rowstruct.MessageStruct      (rowstruct.go)     — the driver-visible STRUCT
//
// The third is Java's MessageTuple.getObject (MessageTuple.java:57-77), the
// driver's struct read. All three give Java's query read, which is
// MessageTuple's, not getFieldOnMessage's: the two differ only on an unset
// proto2 field that declares a default, which getFieldOnMessage reads as the
// default and MessageTuple (hasField false) as null, and a query reads a copy
// of the record that declares no default (TestReadPathsMatchJavasQueryRead).
//
// They agree today, but nothing forced them to: path 1 was wrong until
// recently, which made the answer depend on which path a plan happened to
// take. This test is that gate — it reads the SAME message through all three
// and fails naming whichever one drifted.
//
// SCOPE OF THE COMPARISON. The three layers legitimately differ in
// REPRESENTATION: rowstruct materializes driver values (a nested message
// becomes a Struct, a UUID becomes its canonical string) while the
// executor/values layers keep the engine's row domain ([16]byte for a UUID).
// Asserting raw equality across them would pin representation, not the rule.
// So the comparison is on the property that actually matters and is shared:
// NULL-vs-not-NULL, array-vs-scalar, array LENGTH, and element values rendered
// with %v. The element types here are deliberately plain scalars (int32,
// int64, string) so that rendering is lossless and the comparison means
// something.

// readPathValue is the layer-independent shape the three paths are compared
// on: SQL NULL, a scalar, or an array of scalars.
type readPathValue struct {
	null   bool
	isArr  bool
	elems  []string
	scalar string
}

func (r readPathValue) String() string {
	switch {
	case r.null:
		return "NULL"
	case r.isArr:
		return fmt.Sprintf("array%v(len=%d)", r.elems, len(r.elems))
	default:
		return fmt.Sprintf("scalar(%s)", r.scalar)
	}
}

func (r readPathValue) equal(o readPathValue) bool {
	if r.null != o.null || r.isArr != o.isArr {
		return false
	}
	if r.null {
		return true
	}
	if !r.isArr {
		return r.scalar == o.scalar
	}
	if len(r.elems) != len(o.elems) {
		return false
	}
	for i := range r.elems {
		if r.elems[i] != o.elems[i] {
			return false
		}
	}
	return true
}

// normalizeReadPathValue projects one layer's answer onto readPathValue.
// A nested struct/message would land in the scalar arm with a
// representation-dependent rendering, which is exactly why the matrix below
// keeps every leaf a plain scalar.
func normalizeReadPathValue(v any) readPathValue {
	if v == nil {
		return readPathValue{null: true}
	}
	if list, ok := v.([]any); ok {
		elems := make([]string, len(list))
		for i, e := range list {
			elems[i] = fmt.Sprintf("%v", e)
		}
		return readPathValue{isArr: true, elems: elems}
	}
	return readPathValue{scalar: fmt.Sprintf("%v", v)}
}

// buildReadPathAgreementMessage builds one message exercising the full
// matrix: flat repeated empty/populated, singular unset/set, and the
// NullableArrayWrapper shape absent / present-but-empty / present-populated.
// The wrapper is `message W { repeated int32 values = 1; }` — the shape (not
// the name) is what identifies it, per values.IsWrappedArrayDescriptor.
func buildReadPathAgreementMessage(t *testing.T) protoreflect.Message {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("read_path_agreement.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("readpathagreement"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Wrapper"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   proto.String("values"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
				}},
			},
			{
				Name: proto.String("Rec"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:   proto.String("id"),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
					},
					{
						Name:   proto.String("note"),
						Number: proto.Int32(2),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
					{
						Name:   proto.String("unset_note"),
						Number: proto.Int32(3),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
					{
						Name:   proto.String("flat_empty"),
						Number: proto.Int32(4),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
					},
					{
						Name:   proto.String("flat_full"),
						Number: proto.Int32(5),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
					},
					{
						Name:     proto.String("wrap_absent"),
						Number:   proto.Int32(6),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".readpathagreement.Wrapper"),
					},
					{
						Name:     proto.String("wrap_empty"),
						Number:   proto.Int32(7),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".readpathagreement.Wrapper"),
					},
					{
						Name:     proto.String("wrap_full"),
						Number:   proto.Int32(8),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".readpathagreement.Wrapper"),
					},
				},
			},
		},
	}
	file, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build file descriptor: %v", err)
	}
	recDesc := file.Messages().ByName("Rec")
	fields := recDesc.Fields()

	m := dynamicpb.NewMessage(recDesc)
	m.Set(fields.ByName("id"), protoreflect.ValueOfInt64(7))
	m.Set(fields.ByName("note"), protoreflect.ValueOfString("hello"))
	// unset_note and flat_empty stay untouched — the two absences the matrix
	// must keep apart (one is real NULL, the other is the empty array).
	full := m.NewField(fields.ByName("flat_full")).List()
	full.Append(protoreflect.ValueOfInt32(10))
	full.Append(protoreflect.ValueOfInt32(20))
	m.Set(fields.ByName("flat_full"), protoreflect.ValueOfList(full))

	// wrap_absent stays unset. wrap_empty is a PRESENT wrapper holding zero
	// values — the case that only exists because the wrapper makes presence
	// real, and the one that separates [] from NULL for a nullable array.
	emptyWrapper, _ := values.NewWrappedArrayMessage(fields.ByName("wrap_empty"))
	m.Set(fields.ByName("wrap_empty"), protoreflect.ValueOfMessage(emptyWrapper))

	fullWrapper, fullList := values.NewWrappedArrayMessage(fields.ByName("wrap_full"))
	fullList.Append(protoreflect.ValueOfInt32(30))
	m.Set(fields.ByName("wrap_full"), protoreflect.ValueOfMessage(fullWrapper))

	return m
}

// TestReadPathAgreement_EmptyArrayAndAbsence is the gate over Go's three
// record→row read paths. It asserts (a) each path's answer matches the one
// Java's single helper would give, and (b) the three paths agree with each
// other — a regression in any ONE of them shows up here naming the offender.
func TestReadPathAgreement_EmptyArrayAndAbsence(t *testing.T) {
	t.Parallel()

	msg := buildReadPathAgreementMessage(t)
	desc := msg.Descriptor()
	fields := desc.Fields()

	// The positional row (path 1) is built once — it is a whole-message
	// materialization, read below by ordinal.
	posRow := protoToPositional(msg.Interface())
	if posRow == nil {
		t.Fatal("protoToPositional returned nil for a non-nil message")
	}
	if got := len(posRow.Slots); got != fields.Len() {
		t.Fatalf("protoToPositional produced %d slots for %d descriptor fields", got, fields.Len())
	}

	// The struct view (path 3) is likewise built once.
	rs, err := rowstruct.New(msg)
	if err != nil {
		t.Fatalf("rowstruct.New: %v", err)
	}

	cases := []struct {
		field string
		want  readPathValue
		why   string
	}{
		{"flat_empty", readPathValue{isArr: true}, "an EMPTY flat repeated field is the empty array, never NULL: repeated is read before any presence test"},
		{"flat_full", readPathValue{isArr: true, elems: []string{"10", "20"}}, "a populated flat repeated field is its elements"},
		{"unset_note", readPathValue{null: true}, "an UNSET singular field is SQL NULL — the empty-array rule must not flatten real absence"},
		{"note", readPathValue{scalar: "hello"}, "a SET singular field is its value"},
		{"id", readPathValue{scalar: "7"}, "a SET singular numeric field is its value"},
		{"wrap_absent", readPathValue{null: true}, "an ABSENT NullableArrayWrapper is SQL NULL — presence is real for the wrapper shape"},
		{"wrap_empty", readPathValue{isArr: true}, "a PRESENT NullableArrayWrapper holding zero values is the empty array, distinct from NULL"},
		{"wrap_full", readPathValue{isArr: true, elems: []string{"30"}}, "a PRESENT NullableArrayWrapper is its elements"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			fd := fields.ByName(protoreflect.Name(tc.field))
			if fd == nil {
				t.Fatalf("descriptor has no field %q", tc.field)
			}

			// Path 1: the base-scan positional row, read by ordinal.
			got1 := normalizeReadPathValue(posRow.Slots[fd.Index()])

			// Path 2: the exact struct descent in the values package. The
			// descriptor-shaped QOV is bound to the message and the resolved
			// ordinal reads through the same presence-aware proto authority.
			corr := values.NamedCorrelationIdentifier("read_path_proto")
			qov := mustTestQOV(t, corr, PositionalTypeForDescriptor(desc))
			field := mustTestFieldOrdinal(t, qov, fd.Index())
			raw2, err := field.Evaluate(&values.RowEvalContext{
				Correlations: stubBinder{corr: msg.Interface()},
			})
			if err != nil {
				t.Fatalf("values.FieldValue.Evaluate(%q): %v", tc.field, err)
			}
			got2 := normalizeReadPathValue(raw2)

			// Path 3: the driver-visible struct.
			raw3, err := rs.AttributeByName(tc.field)
			if err != nil {
				t.Fatalf("rowstruct AttributeByName(%q): %v", tc.field, err)
			}
			got3 := normalizeReadPathValue(raw3)

			if !got1.equal(got2) || !got1.equal(got3) {
				t.Fatalf("read-path divergence on field %q: protoToPositional=%s, protoFieldByName=%s, rowstruct=%s — "+
					"Java funnels every field read through one helper (MessageHelpers.getFieldOnMessage); Go has three and they must agree",
					tc.field, got1, got2, got3)
			}

			// Agreement alone is not enough: all three could drift together.
			// Pin the answer Java's single helper gives.
			for _, p := range []struct {
				name string
				got  readPathValue
			}{
				{"protoToPositional", got1},
				{"protoFieldByName", got2},
				{"rowstruct", got3},
			} {
				if !p.got.equal(tc.want) {
					t.Fatalf("read path %s on field %q = %s, want %s — %s",
						p.name, tc.field, p.got, tc.want, tc.why)
				}
			}
		})
	}
}

// TestReadPathsMatchJavasQueryRead runs Go's three field readers over the cases
// the JVM spec "RFC-257 a query reads a field as Java's query reads it"
// measures (conformance/null_standin_conformance_test.go), and asserts each
// reader gives Java's query read: that spec pins the shared helper
// (values.ProtoFieldReadsValue) against the JVM, and this pins every reader to
// the same measured table, so a reader that stops going through the helper
// cannot agree with the other two and still differ from Java. An unset field
// with a declared default is null in all three: Java's query reads a copy of
// the record in the plan's type (QueryResult.fromQueriedRecord), and the
// driver's struct reader, MessageTuple, reads hasField (MessageTuple.java:73-77);
// only Java's raw MessageHelpers.getFieldOnMessage, which no query applies to
// a stored record, reads the default.
func TestReadPathsMatchJavasQueryRead(t *testing.T) {
	t.Parallel()
	file := func(syntax string) protoreflect.MessageDescriptor {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		field := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
			return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}
		}
		repeated := field("r", 3)
		repeated.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		sub := field("s", 4)
		sub.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
		sub.TypeName = proto.String(".reads.Sub")
		fields := []*descriptorpb.FieldDescriptorProto{field("a", 1), repeated, sub}
		if syntax == "proto2" {
			withDefault := field("d", 2)
			withDefault.DefaultValue = proto.String("7")
			fields = append(fields, withDefault)
		}
		fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name: proto.String("reads_" + syntax + ".proto"), Package: proto.String("reads"), Syntax: proto.String(syntax),
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Sub"), Field: []*descriptorpb.FieldDescriptorProto{field("x", 1)}},
				{Name: proto.String("Rec"), Field: fields},
			},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fd.Messages().ByName("Rec")
	}
	// Java's measured reads, the JVM spec's table.
	for _, c := range []struct {
		syntax string
		set    map[string]int64
		field  string
		java   readPathValue
		// tuple is MessageTuple's read where it differs from java.
		tuple *readPathValue
	}{
		{"proto2", nil, "a", readPathValue{null: true}, nil},
		{"proto2", map[string]int64{"a": 0}, "a", readPathValue{scalar: "0"}, nil},
		{"proto2", nil, "d", readPathValue{null: true}, nil},
		{"proto2", map[string]int64{"d": 0}, "d", readPathValue{scalar: "0"}, nil},
		{"proto2", nil, "r", readPathValue{isArr: true}, nil},
		{"proto2", nil, "s", readPathValue{null: true}, nil},
		{"proto3", nil, "a", readPathValue{null: true}, nil},
		{"proto3", map[string]int64{"a": 0}, "a", readPathValue{null: true}, nil},
		{"proto3", map[string]int64{"a": 5}, "a", readPathValue{scalar: "5"}, nil},
		{"proto3", nil, "r", readPathValue{isArr: true}, nil},
		{"proto3", nil, "s", readPathValue{null: true}, nil},
	} {
		t.Run(fmt.Sprintf("%s %v %s", c.syntax, c.set, c.field), func(t *testing.T) {
			t.Parallel()
			desc := file(c.syntax)
			built := dynamicpb.NewMessage(desc)
			for name, v := range c.set {
				built.Set(desc.Fields().ByName(protoreflect.Name(name)), protoreflect.ValueOfInt64(v))
			}
			b, err := proto.Marshal(built)
			if err != nil {
				t.Fatal(err)
			}
			msg := dynamicpb.NewMessage(desc)
			if err := proto.Unmarshal(b, msg); err != nil {
				t.Fatal(err)
			}
			fd := desc.Fields().ByName(protoreflect.Name(c.field))

			got1 := normalizeReadPathValue(protoToPositional(msg.Interface()).Slots[fd.Index()])
			corr := values.NamedCorrelationIdentifier("java_reads")
			qov := mustTestQOV(t, corr, PositionalTypeForDescriptor(desc))
			raw2, err := mustTestFieldOrdinal(t, qov, fd.Index()).Evaluate(&values.RowEvalContext{
				Correlations: stubBinder{corr: msg.Interface()},
			})
			if err != nil {
				t.Fatal(err)
			}
			got2 := normalizeReadPathValue(raw2)
			rs, err := rowstruct.New(msg)
			if err != nil {
				t.Fatal(err)
			}
			raw3, err := rs.AttributeByName(c.field)
			if err != nil {
				t.Fatal(err)
			}
			got3 := normalizeReadPathValue(raw3)
			tuple := c.java
			if c.tuple != nil {
				tuple = *c.tuple
			}
			for _, p := range []struct {
				name      string
				got, java readPathValue
			}{{"protoToPositional", got1, c.java}, {"FieldValue", got2, c.java}, {"rowstruct (MessageTuple)", got3, tuple}} {
				if !p.got.equal(p.java) {
					t.Errorf("%s reads %s, Java's %s", p.name, p.got, p.java)
				}
			}
		})
	}
}
