package executor

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// coveringTestDescriptor is T(id, a, s{x, y}, f float) and its row type.
func coveringTestDescriptor(t *testing.T) (protoreflect.MessageDescriptor, *values.RecordType) {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	field := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: &opt, Type: &typ}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("covering_reader_test.proto"),
		Package: proto.String("coveringreader"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("S"), Field: []*descriptorpb.FieldDescriptorProto{
				field("x", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
				field("y", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
			}},
			{Name: proto.String("T"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
				field("a", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
				field("s", 3, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".coveringreader.S"),
				field("f", 4, descriptorpb.FieldDescriptorProto_TYPE_FLOAT, ""),
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	desc := file.Messages().ByName("T")
	return desc, PositionalTypeForDescriptor(desc)
}

func coveringLeaf(t *testing.T, source values.TupleSource, ordinal int, typ values.Type) values.Value {
	t.Helper()
	leaf, err := values.NewIndexEntryObjectValue(values.CurrentCorrelation(), source, []int{ordinal}, typ)
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	return leaf
}

// coveringTestReader reads an entry of an index on (f, s.y) with primary key
// id: KEY (f, s.y, id).
func coveringTestReader(t *testing.T) (*coveringEntryReader, *values.RecordType) {
	t.Helper()
	desc, rowType := coveringTestDescriptor(t)
	sType := rowType.Fields[2].FieldType.(*values.RecordType)
	reader := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "id", Value: coveringLeaf(t, values.TupleSourceKey, 2, values.NullableLong)},
		values.RecordConstructorField{Name: "a", Value: values.NewNullValue(values.NullableString)},
		values.RecordConstructorField{Name: "s", Value: values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "x", Value: values.NewNullValue(sType.Fields[0].FieldType)},
			values.RecordConstructorField{Name: "y", Value: coveringLeaf(t, values.TupleSourceKey, 1, values.NullableString)},
		)},
		values.RecordConstructorField{Name: "f", Value: coveringLeaf(t, values.TupleSourceKey, 0, values.NullableFloat)},
	)
	r, err := newCoveringEntryReader(reader, desc, rowType, nil)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	return r, rowType
}

// TestCoveringIndexCursor_ReadsTheRecordFromTheEntry: covered fields land in
// their slots of the record's row (FLOAT widened to the base-scan float64), a
// nested covered field in a message of the stored nested descriptor, the rest
// unset; the full primary key rides along.
func TestCoveringIndexCursor_ReadsTheRecordFromTheEntry(t *testing.T) {
	t.Parallel()
	reader, rowType := coveringTestReader(t)
	index := recordlayer.NewIndex("covering_reader", recordlayer.Concat(recordlayer.Field("f"), recordlayer.Nest("s", recordlayer.Field("y"))))
	entry := &recordlayer.IndexEntry{Index: index, Key: tuple.Tuple{float32(2.5), "why", int64(42)}}
	cursor := &coveringIndexCursor{inner: recordlayer.FromList([]*recordlayer.IndexEntry{entry}), reader: reader}
	defer cursor.Close()

	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("covering cursor result = %#v, err = %v", result, err)
	}
	row := result.GetValue().Positional
	if row.Type != rowType {
		t.Fatal("covering row must carry the record's row type")
	}
	if id, _ := row.Get(0); id != int64(42) {
		t.Fatalf("ID = %v, want 42", id)
	}
	if a, _ := row.Get(1); a != nil {
		t.Fatalf("uncovered A = %v, want nil", a)
	}
	if f, _ := row.Get(3); f != float64(2.5) {
		t.Fatalf("FLOAT = %T:%v, want float64 2.5", f, f)
	}
	s, _ := row.Get(2)
	msg, ok := s.(proto.Message)
	if !ok {
		t.Fatalf("nested S = %T, want a message", s)
	}
	m := msg.ProtoReflect()
	if got := m.Get(m.Descriptor().Fields().ByName("y")).String(); got != "why" {
		t.Fatalf("S.Y = %q, want why", got)
	}
	if m.Has(m.Descriptor().Fields().ByName("x")) {
		t.Fatal("uncovered S.X must stay unset")
	}
	if got := result.GetValue().PrimaryKey; !bytes.Equal(got.Pack(), tuple.Tuple{int64(42)}.Pack()) {
		t.Fatalf("primary key = %v, want (42)", got)
	}
}

// TestCoveringEntryReader_RefusesAMismatchedRow: a reader that does not
// describe the record's row is refused when the scan opens, never read into
// the wrong slots.
func TestCoveringEntryReader_RefusesAMismatchedRow(t *testing.T) {
	t.Parallel()
	desc, rowType := coveringTestDescriptor(t)
	short := values.NewRecordConstructorValue(values.RecordConstructorField{Name: "id", Value: coveringLeaf(t, values.TupleSourceKey, 0, values.NullableLong)})
	if _, err := newCoveringEntryReader(short, desc, rowType, nil); err == nil {
		t.Fatal("a one-field reader over a four-field row was admitted")
	}
	if _, err := newCoveringEntryReader(nil, desc, rowType, nil); err == nil {
		t.Fatal("a covering scan without a reader was admitted")
	}
}

// A covering FLOAT and a base FLOAT of the same number must key alike, or
// DISTINCT/UNION across a covering leg and a base leg would never dedup.
func TestDistinctKey_CoveringAndBaseFloatRowsDedup(t *testing.T) {
	t.Parallel()
	reader, rowType := coveringTestReader(t)
	index := recordlayer.NewIndex("covering_reader", recordlayer.Concat(recordlayer.Field("f"), recordlayer.Nest("s", recordlayer.Field("y"))))
	pos, err := reader.row(&entryBinder{entry: &recordlayer.IndexEntry{Index: index, Key: tuple.Tuple{float32(2.5), nil, int64(1)}}})
	if err != nil {
		t.Fatal(err)
	}
	pos.Slots[2] = nil
	base := QueryResult{Positional: &PositionalRow{Type: rowType, Slots: []any{int64(1), nil, nil, float64(2.5)}}}
	if mustDistinctKey(t, QueryResult{Positional: pos}) != mustDistinctKey(t, base) {
		t.Fatal("covering row and base row of one record key differently")
	}
}

func TestTupleElementToRowValue_BytesPassThrough(t *testing.T) {
	t.Parallel()
	b := []byte{0x01}
	if got, ok := tupleElementToRowValue(b).([]byte); !ok || &got[0] != &b[0] {
		t.Fatalf("[]byte should pass through unchanged")
	}
}
