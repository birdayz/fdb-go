package testkit

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/recordlayer"
)

// buildChainedUnnestMetadata constructs a RecordMetaData with a THREE-level
// nested struct/array shape for the class-4 CHAINED lateral unnest
// (`FROM t, t.arr AS x, x.sub AS y`). The record T4 carries:
//
//	ID       int64 (pk)
//	SARR     repeated ELEM     — struct-array (the first unnest's array)
//	SCARR    repeated int32    — SCALAR-array (a chained-owner-is-scalar decline)
//
//	ELEM     { SUB repeated int32; K int64; SUBSTRUCT repeated ELEM2 }
//	ELEM2    { DEEP repeated int32; LEAF int64 }
//
// So a 2-chain (`T4.SARR AS x, x.SUB AS y`) unnests the struct-array element's
// own int-array SUB; a 3-chain (`… x.SUBSTRUCT AS y, y.DEEP AS z`) descends one
// struct level deeper — exercising chainedOwnerElementMessage's recursion. The
// SQL schema builder cannot express message/struct columns, so the proto is
// built dynamically (descriptorpb + protodesc.NewFile) exactly as the metadata
// builder does; records are genuine dynamicpb messages and the chained unnest
// runs the full Cascades path against real FDB.
func BuildChainedUnnestMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("chained_unnest_test.proto"),
		Package: proto.String("fdb.test.chainedunnest"),
		Syntax:  proto.String("proto2"),
	}
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()

	elem2 := &descriptorpb.DescriptorProto{
		Name: proto.String("ELEM2"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("DEEP"), Number: proto.Int32(1), Label: rep, Type: i32},
			{Name: proto.String("LEAF"), Number: proto.Int32(2), Label: opt, Type: i64},
		},
	}
	elem := &descriptorpb.DescriptorProto{
		Name: proto.String("ELEM"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("SUB"), Number: proto.Int32(1), Label: rep, Type: i32},
			{Name: proto.String("K"), Number: proto.Int32(2), Label: opt, Type: i64},
			{
				Name: proto.String("SUBSTRUCT"), Number: proto.Int32(3), Label: rep, Type: msg,
				TypeName: proto.String(".fdb.test.chainedunnest.ELEM2"),
			},
		},
	}
	t4 := &descriptorpb.DescriptorProto{
		Name: proto.String("T4"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("ID"), Number: proto.Int32(1), Label: opt, Type: i64},
			{
				Name: proto.String("SARR"), Number: proto.Int32(2), Label: rep, Type: msg,
				TypeName: proto.String(".fdb.test.chainedunnest.ELEM"),
			},
			{Name: proto.String("SCARR"), Number: proto.Int32(3), Label: rep, Type: i32},
			// A TOP-LEVEL scalar deliberately NAMED "SUB" — the SAME bare name as
			// the ELEM element's sub-array field. The shadow-precedence pin below
			// proves `x.SUB` reads the ELEMENT's SUB (via the two-level accessor
			// [X, SUB]), never this outer-row column.
			{Name: proto.String("SUB"), Number: proto.Int32(4), Label: opt, Type: i64},
		},
	}
	union := &descriptorpb.DescriptorProto{
		Name: proto.String("RecordTypeUnion"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("_T4"), Number: proto.Int32(1), Label: opt, Type: msg,
				TypeName: proto.String(".fdb.test.chainedunnest.T4"),
			},
		},
	}
	fdp.MessageType = []*descriptorpb.DescriptorProto{elem2, elem, t4, union}

	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(false)
	mdBuilder.SetStoreRecordVersions(false)
	mdBuilder.SetVersion(1)
	mdBuilder.SetRecordCountKey(recordlayer.RecordTypeKey())
	rt := mdBuilder.GetRecordType("T4")
	if rt == nil {
		t.Fatalf("record type T4 not found after SetRecords")
	}
	rt.SetPrimaryKey(recordlayer.Field("ID"))
	md, err := mdBuilder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}
