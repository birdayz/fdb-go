package executor

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// churnDescriptor mints a FRESH MessageDescriptor instance on every call, the
// way a long-lived multi-tenant process does when it keeps loading schemas
// through dynamicpb. Descriptor identity is the cache key, so each one is a
// distinct entry.
func churnDescriptor(t *testing.T, i int) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(fmt.Sprintf("churn_%d.proto", i)),
		Package: proto.String(fmt.Sprintf("churn%d", i)),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("R"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("ID"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build descriptor %d: %v", i, err)
	}
	return fd.Messages().Get(0)
}

// TestPositionalTypeCache_RowVersionShapeSharesTheBound pins that the
// __ROW_VERSION-extended row shape is bounded by the SAME wipe as the plain one.
//
// It used to live in its own sync.Map whose comment claimed the "same lifecycle"
// as the base cache. It had none: the base cache's wipe-at-cap ranges over the
// base map only, so under descriptor churn in a version-storing store the
// version map grew without bound — one retained descriptor and one retained
// RecordType per schema load, forever.
//
// The assertion is EVICTION, by pointer identity, because that is what the
// bound actually means and it is the only thing observable from outside: a
// version-extended type minted before the cap is passed must NOT still be
// handed back afterwards. Asserting on the shared entry counter proves nothing
// — the counter never covered the separate map, so it stayed inside the cap
// while the leak grew beside it. (That mistake is why this comment exists.)
func TestPositionalTypeCache_RowVersionShapeSharesTheBound(t *testing.T) {
	// Not parallel: it asserts on process-global cache state.
	positionalTypeCacheMu.Lock()
	positionalTypeCache.Range(func(k, _ any) bool {
		positionalTypeCache.Delete(k)
		return true
	})
	positionalTypeCacheSize.Store(0)
	positionalTypeCacheMu.Unlock()

	// The canary: minted FIRST, then buried under enough churn to force a wipe.
	// The descriptor is kept alive for the whole test so the only reason its
	// entry can disappear is eviction.
	canary := churnDescriptor(t, 0)
	firstMint := positionalTypeWithRowVersion(canary)

	// Each churned descriptor mints TWO entries through this path (the base
	// shape and the version-extended one), so half the cap of descriptors
	// already reaches it; go well past.
	const descriptors = positionalTypeCacheCap*2 + 64
	for i := 1; i <= descriptors; i++ {
		desc := churnDescriptor(t, i)
		rt := positionalTypeWithRowVersion(desc)
		if rt == nil {
			t.Fatalf("descriptor %d produced no row type", i)
		}
		last := rt.Fields[len(rt.Fields)-1]
		if last.Name != "__ROW_VERSION" {
			t.Fatalf("descriptor %d: last field is %q, want the __ROW_VERSION "+
				"pseudo-slot — the bound must not change the shape", i, last.Name)
		}
	}

	if reMint := positionalTypeWithRowVersion(canary); reMint == firstMint {
		t.Fatalf("the __ROW_VERSION-extended type minted before %d descriptors of "+
			"churn (cap %d) is STILL cached — it is not covered by the wipe, so "+
			"under descriptor churn in a version-storing store it retains one "+
			"descriptor and one RecordType per schema load, forever",
			descriptors, positionalTypeCacheCap)
	}

	// A live descriptor must still resolve correctly after a wipe: the entries
	// are pure derivations, so re-warming is the whole point.
	desc := churnDescriptor(t, 999999)
	base := PositionalTypeForDescriptor(desc)
	ext := positionalTypeWithRowVersion(desc)
	if len(ext.Fields) != len(base.Fields)+1 {
		t.Fatalf("extended shape has %d fields, base has %d — want exactly one more",
			len(ext.Fields), len(base.Fields))
	}
	if ext.Fields[len(ext.Fields)-1].Ordinal != len(base.Fields) {
		t.Fatalf("pseudo-slot ordinal is %d, want %d",
			ext.Fields[len(ext.Fields)-1].Ordinal, len(base.Fields))
	}
	// The two shapes are DISTINCT cache entries — a single key would have the
	// version-extended type shadow the plain one (or vice versa).
	if base == ext {
		t.Fatal("the plain and version-extended shapes resolved to the SAME " +
			"RecordType; they must be separate cache entries")
	}
	_ = dynamicpb.NewMessage(desc)
}
