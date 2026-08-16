package query

// The LOGICAL scan row must be the STORED row.
//
// A scan flows materialized records, so the type derived for it here and the
// type the physical scan and executor build (executor.queryResult uses
// values.FieldNameForProtoField + values.FieldTypeForProtoField) describe the
// same bytes. FieldTypeForFD's own contract states they must not be able to
// disagree — it is the same function.
//
// The derivation used TargetTypeForFD, which answers a different question:
// what an INSERT target ACCEPTS. Measured over every descriptor registered in
// this process, the two answers differed on 153 of 522 comparable fields, and
// a further 660 fields sat inside 203 self-referencing messages where
// TargetTypeForFD does not return an answer at all — it has no cycle guard and
// blows the stack. Each arm below is one of those axes, driven off a
// descriptor built for it rather than off whichever registered message
// happened to have the shape.
//
// The recursion arm is deliberately blunt: a stack overflow is a fatal runtime
// error that no recover() can catch, so its failure mode is the whole test
// binary dying rather than a reported failure. That is still the regression
// signal, and it is the only one available for this class of defect.

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// scanRowFixture builds a catalog whose one record type exercises every axis
// on which the stored-row and DML-target mappers disagree, plus a self
// reference.
func scanRowFixture(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	req := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	u64 := descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	pkg := "fdb.test.scanrow"
	tn := func(name string) *string { return proto.String("." + pkg + "." + name) }
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("scanrow_test.proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			// Self-referencing: NODE.CHILD is a NODE.
			{Name: proto.String("NODE"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("LABEL"), Number: proto.Int32(1), Label: opt, Type: str},
				{Name: proto.String("CHILD"), Number: proto.Int32(2), Label: opt, Type: msg, TypeName: tn("NODE")},
			}},
			{Name: proto.String("NESTED"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("INNER_ID"), Number: proto.Int32(1), Label: req, Type: i64},
			}},
			{Name: proto.String("T"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: opt, Type: i64},
				// required: presence-tracked by the target mapper, NOT NULL in storage.
				{Name: proto.String("REQ_ID"), Number: proto.Int32(2), Label: req, Type: i64},
				// repeated scalar: the ELEMENT nullability is the axis.
				{Name: proto.String("TAGS"), Number: proto.Int32(3), Label: rep, Type: str},
				// nested message: field NAMES and record identity are the axis.
				{Name: proto.String("N"), Number: proto.Int32(4), Label: opt, Type: msg, TypeName: tn("NESTED")},
				// unsigned: the authoritative mapper carries it as LONG.
				{Name: proto.String("U"), Number: proto.Int32(5), Label: opt, Type: u64},
				// self-referencing message: no cycle guard blows the stack.
				{Name: proto.String("TREE"), Number: proto.Int32(6), Label: opt, Type: msg, TypeName: tn("NODE")},
			}},
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("_T"), Number: proto.Int32(1), Label: opt, Type: msg, TypeName: tn("T")},
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("T").SetPrimaryKey(recordlayer.Field("ID"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

func TestLogicalScanRowIsTheStoredRow(t *testing.T) {
	t.Parallel()
	md := scanRowFixture(t)

	// This call is itself the recursion regression: with an unguarded mapper
	// the TREE column recurses until the stack dies, and nothing below runs.
	typ, err := ExactLogicalResultType(logical.NewScan("T", "T"), md)
	if err != nil {
		t.Fatalf("ExactLogicalResultType(scan): %v", err)
	}
	row, ok := typ.(*values.RecordType)
	if !ok {
		t.Fatalf("scan row = %T, want *values.RecordType", typ)
	}
	byName := make(map[string]values.Field, len(row.Fields))
	for _, f := range row.Fields {
		byName[f.Name] = f
	}
	if len(byName) != len(row.Fields) {
		t.Fatalf("scan row has duplicate column names: %v", row.Fields)
	}

	// Every column must be exactly what the authoritative stored-row mapper
	// says for its descriptor. This is the whole contract; the named arms
	// below exist so a failure says WHICH axis moved.
	descriptor := md.GetRecordType("T").Descriptor
	if descriptor.Fields().Len() != len(row.Fields) {
		t.Fatalf("scan row has %d columns, descriptor has %d", len(row.Fields), descriptor.Fields().Len())
	}
	for i := 0; i < descriptor.Fields().Len(); i++ {
		fd := descriptor.Fields().Get(i)
		want := values.FieldTypeForProtoField(fd)
		got := row.Fields[i]
		if got.Name != values.FieldNameForProtoField(fd) {
			t.Errorf("column %d name = %q, want %q", i, got.Name, values.FieldNameForProtoField(fd))
		}
		if got.Ordinal != i {
			t.Errorf("column %d ordinal = %d, want %d", i, got.Ordinal, i)
		}
		if !got.FieldType.Equals(want) {
			t.Errorf("column %q type = %v, want the stored-row type %v", got.Name, got.FieldType, want)
		}
	}

	t.Run("required_scalar_is_not_null", func(t *testing.T) {
		f, ok := byName["REQ_ID"]
		if !ok {
			t.Fatalf("no REQ_ID column in %v", row.Fields)
		}
		if f.FieldType.IsNullable() {
			t.Error("a proto2 `required` column typed as NULLABLE — that is the" +
				" DML target mapper's presence answer, not the stored shape")
		}
	})

	t.Run("repeated_element_is_not_null", func(t *testing.T) {
		f, ok := byName["TAGS"]
		if !ok {
			t.Fatalf("no TAGS column in %v", row.Fields)
		}
		array, ok := f.FieldType.(*values.ArrayType)
		if !ok {
			t.Fatalf("TAGS = %v, want an array type", f.FieldType)
		}
		if array.IsNullable() {
			t.Error("a flat repeated column typed as a NULLABLE array — a flat" +
				" repeated field materializes as an empty list, never nil")
		}
		if array.ElementType == nil || array.ElementType.IsNullable() {
			t.Errorf("TAGS element = %v, want a NOT NULL element", array.ElementType)
		}
	})

	t.Run("nested_record_carries_stored_names", func(t *testing.T) {
		f, ok := byName["N"]
		if !ok {
			t.Fatalf("no N column in %v", row.Fields)
		}
		nested, ok := f.FieldType.(*values.RecordType)
		if !ok {
			t.Fatalf("N = %v, want a record type", f.FieldType)
		}
		if len(nested.Fields) != 1 || nested.Fields[0].Name != "INNER_ID" {
			t.Errorf("nested fields = %v, want one column named INNER_ID —"+
				" the target mapper emits the raw descriptor spelling instead of"+
				" the user identifier", nested.Fields)
		}
		if nested.RecordName != "fdb.test.scanrow.NESTED" {
			t.Errorf("nested record name = %q, want the full descriptor name —"+
				" the target mapper emits the SHORT message name, so two"+
				" same-named messages in different packages become one type",
				nested.RecordName)
		}
	})

	t.Run("unsigned_scalar_is_long", func(t *testing.T) {
		f, ok := byName["U"]
		if !ok {
			t.Fatalf("no U column in %v", row.Fields)
		}
		if f.FieldType.Code() != values.TypeCodeLong {
			t.Errorf("uint64 column = %v, want LONG (the runtime materializer"+
				" emits int64 for it)", f.FieldType)
		}
	})

	t.Run("self_reference_terminates", func(t *testing.T) {
		f, ok := byName["TREE"]
		if !ok {
			t.Fatalf("no TREE column in %v", row.Fields)
		}
		// Reaching this line at all is the assertion. The value is checked so
		// the arm cannot pass on a nil that silently means "declined".
		if f.FieldType == nil {
			t.Error("a self-referencing column produced no type")
		}
	})
}
