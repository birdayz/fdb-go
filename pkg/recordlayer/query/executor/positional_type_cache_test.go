package executor

import (
	"fmt"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The positionalTypeCache is BOUNDED (S4 kill-list obligation): a long-lived
// multi-tenant process that keeps loading schemas mints a fresh dynamicpb
// descriptor per load, and pre-fix every miss Stored forever — an unbounded
// sync.Map leak. Churn far past the cap must leave the entry count bounded
// and the derivation correct after wipes.
//
// NOT t.Parallel(): the test measures the PROCESS-GLOBAL cache while driving
// it through wipes; a concurrent test's Stores would race the count assert.
func TestPositionalTypeCacheBounded(t *testing.T) {
	freshDynamicMessage := func(i int) proto.Message {
		fdp := &descriptorpb.FileDescriptorProto{
			Name:    proto.String(fmt.Sprintf("cache_churn_%d.proto", i)),
			Syntax:  proto.String("proto2"),
			Package: proto.String("cachechurn"),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Rec"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:   proto.String("id"),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
					},
					{
						Name:   proto.String("name"),
						Number: proto.Int32(2),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
				},
			}},
		}
		fd, err := protodesc.NewFile(fdp, nil)
		if err != nil {
			// panic, not t.Fatalf: the builder also runs on worker
			// goroutines in the concurrent-churn phase.
			panic(fmt.Sprintf("build file descriptor %d: %v", i, err))
		}
		md := fd.Messages().Get(0)
		m := dynamicpb.NewMessage(md)
		m.Set(md.Fields().ByName("id"), protoreflect.ValueOfInt64(int64(i)))
		m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString("x"))
		return m
	}

	// Churn well past the cap: each iteration is a DISTINCT descriptor
	// instance (fresh protodesc.NewFile), the leak class.
	churn := positionalTypeCacheCap + positionalTypeCacheCap/2
	for i := 0; i < churn; i++ {
		row := protoToPositional(freshDynamicMessage(i))
		if row == nil || row.Type == nil {
			t.Fatalf("iteration %d: positional row not built", i)
		}
		// Correctness through wipes: the derived type keeps declaration
		// order regardless of cache state.
		if got := row.Type.Fields[0].Name; got != "ID" {
			t.Fatalf("iteration %d: field 0 = %q, want ID", i, got)
		}
		if got := row.Type.Fields[1].Name; got != "NAME" {
			t.Fatalf("iteration %d: field 1 = %q, want NAME", i, got)
		}
	}

	entries := 0
	positionalTypeCache.Range(func(_, _ any) bool {
		entries++
		return true
	})
	// The miss path is serialized, so the bound is EXACT — never the churn
	// count: after 1.5x-cap distinct descriptors the map must have wiped.
	if entries > positionalTypeCacheCap {
		t.Fatalf("cache holds %d entries after %d-descriptor churn — the wipe-at-cap bound is not holding (cap %d)",
			entries, churn, positionalTypeCacheCap)
	}

	// CONCURRENT churn (the review finding): misses racing a wipe must not
	// re-store past the reset and overshoot the cap. Fan out well past the
	// cap across goroutines; the miss lock makes the bound hold exactly at
	// every instant, asserted after the join.
	const workers = 8
	perWorker := (positionalTypeCacheCap/workers + 64)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				row := protoToPositional(freshDynamicMessage(1_000_000 + w*perWorker + i))
				if row == nil || row.Type == nil {
					t.Errorf("worker %d iteration %d: positional row not built", w, i)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	entries = 0
	positionalTypeCache.Range(func(_, _ any) bool {
		entries++
		return true
	})
	if entries > positionalTypeCacheCap {
		t.Fatalf("cache holds %d entries after concurrent churn — the miss-path lock is not enforcing the bound (cap %d)",
			entries, positionalTypeCacheCap)
	}
}
