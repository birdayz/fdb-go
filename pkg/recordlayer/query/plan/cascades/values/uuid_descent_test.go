package values

import (
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// uuidHostFixture builds a message
//
//	package com.apple.foundationdb.record;
//	message UUID { sfixed64 most_significant_bits=1; sfixed64 least_significant_bits=2; }
//	message Host { UUID id=1; repeated UUID ids=2; message Rec { UUID u=1; } Rec rec=3; }
//
// with id/ids/rec.u populated — the tuple_fields.UUID shape the row layer
// surfaces as a neutral [16]byte. Returns the Host message.
func uuidHostFixture(t *testing.T) protoreflect.Message {
	t.Helper()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_SFIXED64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    protoStr("uuidhost.proto"),
		Syntax:  protoStr("proto2"),
		Package: protoStr("com.apple.foundationdb.record"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: protoStr("UUID"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: protoStr("most_significant_bits"), Number: protoInt32(1), Type: i64, Label: opt},
					{Name: protoStr("least_significant_bits"), Number: protoInt32(2), Type: i64, Label: opt},
				},
			},
			{
				Name: protoStr("Host"),
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:  protoStr("Rec"),
					Field: []*descriptorpb.FieldDescriptorProto{{Name: protoStr("u"), Number: protoInt32(1), Type: msg, TypeName: protoStr(".com.apple.foundationdb.record.UUID"), Label: opt}},
				}},
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: protoStr("id"), Number: protoInt32(1), Type: msg, TypeName: protoStr(".com.apple.foundationdb.record.UUID"), Label: opt},
					{Name: protoStr("ids"), Number: protoInt32(2), Type: msg, TypeName: protoStr(".com.apple.foundationdb.record.UUID"), Label: rep},
					{Name: protoStr("rec"), Number: protoInt32(3), Type: msg, TypeName: protoStr(".com.apple.foundationdb.record.Host.Rec"), Label: opt},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("fixture descriptor: %v", err)
	}
	uuidDesc := fd.Messages().ByName("UUID")
	mkUUID := func(most, least int64) protoreflect.Value {
		m := dynamicpb.NewMessage(uuidDesc)
		m.Set(uuidDesc.Fields().ByName("most_significant_bits"), protoreflect.ValueOfInt64(most))
		m.Set(uuidDesc.Fields().ByName("least_significant_bits"), protoreflect.ValueOfInt64(least))
		return protoreflect.ValueOfMessage(m)
	}
	hostDesc := fd.Messages().ByName("Host")
	host := dynamicpb.NewMessage(hostDesc)
	host.Set(hostDesc.Fields().ByName("id"), mkUUID(1, 2))
	lst := host.NewField(hostDesc.Fields().ByName("ids")).List()
	lst.Append(mkUUID(3, 4))
	lst.Append(mkUUID(5, 6))
	host.Set(hostDesc.Fields().ByName("ids"), protoreflect.ValueOfList(lst))
	recDesc := hostDesc.Messages().ByName("Rec")
	rec := dynamicpb.NewMessage(recDesc)
	rec.Set(recDesc.Fields().ByName("u"), mkUUID(7, 8))
	host.Set(hostDesc.Fields().ByName("rec"), protoreflect.ValueOfMessage(rec))
	return host
}

func wantUUIDBytes(most, least uint64) [16]byte {
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(most >> (56 - 8*i))
		b[8+i] = byte(least >> (56 - 8*i))
	}
	return b
}

// TestProtoFieldToRowValue_UUID pins that the shared proto converter surfaces
// a tuple_fields.UUID message as the neutral [16]byte (msb‖lsb big-endian) —
// singular, repeated (each element), AND through a STRUCT-descent path (a UUID
// nested in a record read by name). The behavior the proto-converter
// unification changed for repeated UUID (was raw message) and the descent
// path never covered.
func TestProtoFieldToRowValue_UUID(t *testing.T) {
	t.Parallel()
	host := uuidHostFixture(t)
	fields := host.Descriptor().Fields()

	// Singular UUID field → [16]byte.
	got := ProtoFieldToRowValue(fields.ByName("id"), host.Get(fields.ByName("id")))
	if got != wantUUIDBytes(1, 2) {
		t.Fatalf("singular UUID = %v (%T), want [16]byte msb=1 lsb=2", got, got)
	}

	// Repeated UUID field → []any of [16]byte (the unification's behavior
	// change: was raw proto messages).
	gotList := ProtoFieldToRowValue(fields.ByName("ids"), host.Get(fields.ByName("ids")))
	arr, ok := gotList.([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("repeated UUID = %v (%T), want []any len 2", gotList, gotList)
	}
	if arr[0] != wantUUIDBytes(3, 4) || arr[1] != wantUUIDBytes(5, 6) {
		t.Fatalf("repeated UUID elements = %v, want [{3,4} {5,6}] as [16]byte (not raw messages)", arr)
	}

	// STRUCT descent: a fused path rec.u lands on a UUID leaf via the
	// proto-message descent arm → [16]byte.
	fused := &FieldValue{
		Field: "U",
		Resolved: &FieldPath{
			Accessors:      []ResolvedAccessor{{Field: "ROOT", Ordinal: 0}, {Field: "rec", Ordinal: -1}, {Field: "u", Ordinal: -1}},
			FrontierPinned: true,
		},
	}
	descended, err := fused.descendResolvedPath(host.Interface())
	if err != nil {
		t.Fatalf("struct-descent to rec.u: %v", err)
	}
	if descended != wantUUIDBytes(7, 8) {
		t.Fatalf("rec.u via struct descent = %v (%T), want [16]byte msb=7 lsb=8", descended, descended)
	}
}
