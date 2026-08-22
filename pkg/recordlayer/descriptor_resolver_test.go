package recordlayer

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// descriptorResolver IS PART OF protodesc's NAME-RESOLUTION LOOP, and the
// contract it has to honour is not the usual Go one.
//
// protodesc resolves a relative reference by walking outward through enclosing
// scopes, and it decides whether to keep walking by comparing this resolver's
// error to protoregistry.NotFound with `!=` — a direct equality in
// desc_resolve.go's findDescriptor, NOT errors.Is. So a descriptive error, or
// even a correctly wrapped sentinel, is treated as FATAL and stops the walk at
// the first candidate. "Not here, try the enclosing scope" becomes "cannot
// resolve type".
//
// Neither of the two fixes below was reachable from any test: the scope tests
// call protodesc.NewFile(..., nil), which installs protodesc's own resolver, and
// the immutability fixture's names are absolutized before resolution runs. Both
// production fixes could therefore have regressed in silence, which is what
// these arms exist to stop.
func TestDescriptorResolverReturnsTheNotFoundSentinel(t *testing.T) {
	t.Parallel()

	r := &descriptorResolver{}
	_, err := r.FindDescriptorByName("nothing.declares.This")

	if err == nil {
		t.Fatal("FindDescriptorByName found a descriptor for a name nothing declares")
	}
	// The `==` is deliberate and mirrors protodesc's own comparison. errors.Is
	// would pass for a wrapped sentinel that protodesc rejects, so asserting it
	// that way would pin a contract looser than the one that matters.
	if err != protoregistry.NotFound {
		t.Fatalf("FindDescriptorByName returned %#v (errors.Is NotFound = %v).\n"+
			"protodesc compares this error to protoregistry.NotFound with `!=`, so anything else — "+
			"including a %%w-wrapped sentinel — aborts its outward walk at the first candidate and "+
			"turns a miss into `cannot resolve type`.", err, errors.Is(err, protoregistry.NotFound))
	}
}

// findInFile must see NESTED declarations, not just top-level ones. It scanned
// only fd.Messages() and fd.Enums(), so a legitimate `.probe.Outer.Inner` was
// reported missing even though the file declares it — and, before the sentinel
// fix, reported as fatal rather than as a miss, killing the caller's walk too.
func TestDescriptorResolverFindsNestedDeclarations(t *testing.T) {
	t.Parallel()

	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("resolver_nested.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Outer"),
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Inner"),
				NestedType: []*descriptorpb.DescriptorProto{
					{Name: proto.String("Deepest")},
				},
			}},
			EnumType: []*descriptorpb.EnumDescriptorProto{{
				Name: proto.String("NestedEnum"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("ZERO"), Number: proto.Int32(0)},
				},
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("building the fixture: %v", err)
	}

	r := &descriptorResolver{files: map[string]protoreflect.FileDescriptor{fd.Path(): fd}}

	for _, name := range []string{
		"probe.Outer",               // top level, which always worked
		"probe.Outer.Inner",         // one level down
		"probe.Outer.Inner.Deepest", // two levels down
		"probe.Outer.NestedEnum",    // an enum declared inside a message
	} {
		got, err := r.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			t.Errorf("FindDescriptorByName(%q) = %v; the file declares it, and a miss here is "+
				"reported to protodesc as a resolution failure", name, err)
			continue
		}
		if string(got.FullName()) != name {
			t.Errorf("FindDescriptorByName(%q) returned %q", name, got.FullName())
		}
	}

	// The negative side: a name the file does NOT declare must still miss, or
	// the recursion has started matching by suffix rather than by full name.
	if _, err := r.FindDescriptorByName("probe.Inner"); err != protoregistry.NotFound {
		t.Fatalf("FindDescriptorByName(\"probe.Inner\") = %v, want NotFound. Only `probe.Outer.Inner` "+
			"is declared; matching this would mean the walk compares suffixes rather than full names.", err)
	}
}
