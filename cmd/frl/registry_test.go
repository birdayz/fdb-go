package main

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "fdb.dev/pkg/relational/sqldriver"
)

// The demo schema is test data: registered in a production binary, its
// com.apple.foundationdb.record messages would stand in for a user schema's
// same-named ones (and collide with a compiled user schema at init).
func TestProductionBinaryRegistersNoDemoTypes(t *testing.T) {
	t.Parallel()
	// The binary does register the record layer's own protos, the same package.
	if _, err := protoregistry.GlobalTypes.FindMessageByName("com.apple.foundationdb.record.DataStoreInfo"); err != nil {
		t.Fatalf("record layer proto not registered: %v", err)
	}
	for _, name := range []protoreflect.FullName{
		"com.apple.foundationdb.record.Order",
		"com.apple.foundationdb.record.Customer",
		"com.apple.foundationdb.record.Flower",
		"com.apple.foundationdb.record.TypedRecord",
		"com.apple.foundationdb.record.UnionDescriptor",
	} {
		if _, err := protoregistry.GlobalTypes.FindMessageByName(name); err == nil {
			t.Errorf("demo message %s is registered in the frl binary", name)
		}
	}
	if _, err := protoregistry.GlobalFiles.FindFileByPath("record_layer_demo.proto"); err == nil {
		t.Error("record_layer_demo.proto is registered in the frl binary")
	}
}
