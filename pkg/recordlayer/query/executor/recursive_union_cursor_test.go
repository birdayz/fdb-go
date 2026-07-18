package executor

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// TestRecursiveRow_SiblingStructDescriptorResolves pins the recursive
// continuation's descriptor resolution for a dynamic STRUCT slot whose
// message type is a TOP-LEVEL SIBLING of the record types (demo proto's
// Flower — referenced as Order's struct field, never a record type and
// never nested inside one). The retired record-type-only walk (record
// types + their nested messages) could not find it, so resuming a valid
// continuation failed with "message type not found"; the resolver now
// indexes whole parent files — top-level siblings, nested messages, and
// transitive imports (metadataMessageResolver).
func TestRecursiveRow_SiblingStructDescriptorResolves(t *testing.T) {
	t.Parallel()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resolve := metadataMessageResolver(md)

	// The sibling type resolves directly.
	flowerDesc := (&gen.Flower{}).ProtoReflect().Descriptor()
	got, err := resolve(string(flowerDesc.FullName()))
	if err != nil {
		t.Fatalf("sibling type %s must resolve from store metadata: %v", flowerDesc.FullName(), err)
	}
	if got.FullName() != flowerDesc.FullName() {
		t.Fatalf("resolved %s, want %s", got.FullName(), flowerDesc.FullName())
	}

	// And a buffered row carrying a dynamic Flower slot round-trips through
	// the recursive codec with that resolver — the exact resume path.
	flower := dynamicpb.NewMessage(flowerDesc)
	flower.Set(flowerDesc.Fields().ByName("type"), protoreflect.ValueOfString("rose"))
	qr := dorder([]string{"N", "F"}, []any{int64(7), flower})
	b, err := encodeRecursiveRow(qr)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeRecursiveRow(b, resolve)
	if err != nil {
		t.Fatalf("decode with sibling struct slot: %v", err)
	}
	if back.Positional == nil || len(back.Positional.Slots) != 2 {
		t.Fatalf("round-trip lost the row: %+v", back)
	}
	if back.Positional.Slots[0] != int64(7) {
		t.Fatalf("scalar slot: got %v", back.Positional.Slots[0])
	}
	msg, ok := back.Positional.Slots[1].(interface{ String() string })
	if !ok || !strings.Contains(msg.String(), "rose") {
		t.Fatalf("struct slot did not round-trip: %T %v", back.Positional.Slots[1], back.Positional.Slots[1])
	}
}
