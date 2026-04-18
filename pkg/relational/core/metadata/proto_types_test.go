package metadata

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

func TestIsUUIDDescriptor(t *testing.T) {
	t.Parallel()

	uuidMD := (&gen.UUID{}).ProtoReflect().Descriptor()
	if string(uuidMD.FullName()) != uuidFullName {
		t.Fatalf("gen.UUID FullName = %q, want %q (proto regen?)",
			uuidMD.FullName(), uuidFullName)
	}
	if !isUUIDDescriptor(uuidMD) {
		t.Error("isUUIDDescriptor(gen.UUID) = false, want true")
	}

	// Non-UUID message must fail the check.
	orderMD := gen.File_record_layer_demo_proto.Messages().ByName("Order")
	if orderMD == nil {
		t.Fatal("Order descriptor missing from demo proto")
	}
	if isUUIDDescriptor(orderMD) {
		t.Error("isUUIDDescriptor(Order) = true, want false")
	}
}

func TestMessageTypeFromDescriptor_UUIDFallbackStructShape(t *testing.T) {
	t.Parallel()

	// If anyone ever routes a UUID descriptor through
	// messageTypeFromDescriptor directly (bypassing the UUID
	// short-circuit), the result is a 2-field struct — this pins the
	// expected shape so callers know what the non-short-circuit path
	// produces.
	uuidMD := (&gen.UUID{}).ProtoReflect().Descriptor()
	st, err := messageTypeFromDescriptor(uuidMD, true)
	if err != nil {
		t.Fatalf("messageTypeFromDescriptor(UUID): %v", err)
	}
	if st.NumFields() != 2 {
		t.Errorf("UUID struct field count = %d, want 2", st.NumFields())
	}
}
