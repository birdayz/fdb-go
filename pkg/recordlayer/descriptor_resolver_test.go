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

// findInFile's TOP-LEVEL ENUM branch, which was reachable by no test.
//
// Measured: disabling it (`if false && e.FullName() == name`) left the whole
// pkg/recordlayer package green, while the same harness disabling the nested
// recursion reddens TestDescriptorResolverFindsNestedDeclarations and only that
// arm. Disjoint results, so the green was a real negative rather than a suite
// that notices nothing.
//
// The asymmetry is the tell: the premise guard in absolutize_scope_test.go was
// extended to check top-level enums precisely because `declared` holds enums
// too, while the resolver's own fixture declared none.
func TestDescriptorResolverFindsTopLevelEnums(t *testing.T) {
	t.Parallel()

	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("resolver_toplevel_enum.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("TopEnum"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("ZERO"), Number: proto.Int32(0)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Msg")}},
	}, nil)
	if err != nil {
		t.Fatalf("building the fixture: %v", err)
	}

	r := &descriptorResolver{files: map[string]protoreflect.FileDescriptor{fd.Path(): fd}}

	got, err := r.FindDescriptorByName("probe.TopEnum")
	if err != nil {
		t.Fatalf("FindDescriptorByName(\"probe.TopEnum\") = %v; the file declares it at the top "+
			"level, and a miss is reported to protodesc as a resolution failure", err)
	}
	if _, isEnum := got.(protoreflect.EnumDescriptor); !isEnum {
		t.Fatalf("resolved probe.TopEnum to %T, want an EnumDescriptor", got)
	}

	// The control: the message branch is a separate loop, so a fixture that only
	// ever resolved messages would not have exercised the enum one.
	if _, err := r.FindDescriptorByName("probe.Msg"); err != nil {
		t.Fatalf("FindDescriptorByName(\"probe.Msg\") = %v, want the message", err)
	}
}

// FindFileByPath must pass the registry's own error through, and it had zero
// test coverage -- `git grep FindFileByPath -- '*_test.go'` returned nothing,
// against 8 hits for FindDescriptorByName.
//
// Both directions matter and they pull opposite ways. protodesc compares against
// protoregistry.NotFound with `==` to decide whether an absent OPTION import is
// tolerable, so a genuine miss has to arrive as that exact value. But rewriting
// every failure to NotFound goes too far: the registry also reports AMBIGUITY
// when more than one descriptor sits at a path, and flattening that to "missing"
// would let protodesc treat a conflicting import as an allowed absent one. A
// miss is tolerable; a conflict is not.
//
// THIS ARM PINS THE MISS DIRECTION ONLY -- sentinel identity, that a true miss
// arrives as exactly protoregistry.NotFound. It is blind to the other half by
// construction, and measurably so: replacing the pass-through with a blanket
// `return nil, protoregistry.NotFound` leaves it green. The ambiguity direction
// is the sibling arm below, which is why they are two functions and not one.
func TestFindFileByPathReportsATrueMissAsTheSentinel(t *testing.T) {
	t.Parallel()

	r := &descriptorResolver{files: map[string]protoreflect.FileDescriptor{}}

	_, err := r.FindFileByPath("nothing/declares/this.proto")
	if err == nil {
		t.Fatal("FindFileByPath found a file for a path nothing declares")
	}
	// `==`, mirroring protodesc's own comparison rather than the looser
	// errors.Is: a wrapped sentinel would pass errors.Is and still be treated as
	// fatal by protodesc, so asserting that way would pin the wrong contract.
	if err != protoregistry.NotFound {
		t.Fatalf("FindFileByPath returned %#v for a true miss (errors.Is NotFound = %v); protodesc "+
			"compares with `==`, so anything else makes an absent option import fatal",
			err, errors.Is(err, protoregistry.NotFound))
	}
}

// stubResolver is the seam's test implementation: a protodesc.Resolver whose
// FindFileByPath returns a fixed error.
//
// It exists because the real thing cannot be made to fail this way.
// protoregistry.Files reports AMBIGUITY -- `multiple files named %q` -- only
// when it holds more than one descriptor at a path, and RegisterFile refuses a
// second file at an already-registered path for every receiver except
// GlobalFiles itself. So no registry a test can construct will ever produce the
// error the arm below is about; only a stub will.
type stubResolver struct{ err error }

func (s stubResolver) FindFileByPath(string) (protoreflect.FileDescriptor, error) {
	return nil, s.err
}

func (s stubResolver) FindDescriptorByName(protoreflect.FullName) (protoreflect.Descriptor, error) {
	return nil, protoregistry.NotFound
}

// THE DIRECTION THE SIBLING ARM CANNOT SEE: a registry error that is NOT a miss
// must survive as itself.
//
// This is the entire justification for passing the registry's error through, and
// until the resolver grew its `global` seam there was no way to reach it -- the
// resolver hardcoded protoregistry.GlobalFiles, and the one error shape that
// matters cannot be registered into any other registry (see stubResolver).
//
// Why it matters, concretely: protodesc treats NotFound on an OPTION import as
// permission to carry on without it. Flattening ambiguity into NotFound
// therefore does not merely lose detail -- it converts "two conflicting
// descriptors claim this path" into "there is nothing here, proceed", and the
// metadata loads as though the conflict were absent.
//
// The assertion is on error IDENTITY, not on the ambiguity message: the contract
// is that FindFileByPath does not substitute its own sentinel for whatever the
// registry said. Ambiguity is the motivating instance of that contract, and the
// only one reachable in production, but the property under test is the general
// one -- which is also why a stub error serves as well as the real text would.
func TestFindFileByPathDoesNotFlattenANonMissIntoNotFound(t *testing.T) {
	t.Parallel()

	// Deliberately NOT protoregistry.NotFound, and deliberately not a wrapping
	// of it either: a wrapped sentinel is a third case that protodesc also
	// treats as fatal, and conflating the two is what the sibling arm's `==`
	// check exists to prevent.
	conflict := errors.New("multiple files named \"probe/ambiguous.proto\"")

	r := &descriptorResolver{
		files:  map[string]protoreflect.FileDescriptor{},
		global: stubResolver{err: conflict},
	}

	_, err := r.FindFileByPath("probe/ambiguous.proto")
	if err == nil {
		t.Fatal("FindFileByPath returned a descriptor although the registry reported a conflict")
	}
	if err == protoregistry.NotFound {
		t.Fatal("FindFileByPath flattened a registry CONFLICT into NotFound. protodesc reads " +
			"NotFound on an option import as permission to proceed without it, so an ambiguous " +
			"import would load as though it were merely absent.")
	}
	if err != conflict {
		t.Fatalf("FindFileByPath returned %#v, want the registry's own error %#v -- the resolver "+
			"must not substitute an error of its own for what the registry reported", err, conflict)
	}
}

// The seam itself is load-bearing, so it gets an arm: an injected registry must
// actually be consulted, or every assertion made through it is vacuous.
//
// Without this, a `global` field that was silently ignored -- dropped in a
// refactor, shadowed by a stray fallback to GlobalFiles -- would leave the
// ambiguity arm above passing for the wrong reason on a path GlobalFiles also
// misses, since both would then answer NotFound and only the `err == NotFound`
// branch would fire. That is the empty-set false green wearing a seam.
func TestFindFileByPathConsultsTheInjectedRegistry(t *testing.T) {
	t.Parallel()

	const path = "probe/seam_is_consulted.proto"
	// The premise: GlobalFiles must NOT know this path, or a pass would prove
	// nothing about which registry answered.
	if _, err := protoregistry.GlobalFiles.FindFileByPath(path); err == nil {
		t.Fatalf("%s is registered globally, so this arm cannot tell the injected registry "+
			"apart from the global one", path)
	}

	sentinel := errors.New("answered by the injected registry")
	r := &descriptorResolver{
		files:  map[string]protoreflect.FileDescriptor{},
		global: stubResolver{err: sentinel},
	}

	if _, err := r.FindFileByPath(path); err != sentinel {
		t.Fatalf("FindFileByPath returned %#v, want the injected registry's %#v -- the seam is "+
			"not being consulted, which makes every assertion driven through it vacuous",
			err, sentinel)
	}
}
