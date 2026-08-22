package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ABSOLUTIZATION MUST NOT CHANGE WHAT A NAME BINDS TO.
//
// Protobuf resolves a relative type name by walking OUTWARD from the declaring
// scope and taking the first match, so `Inner` written inside `Host` means
// `Host.Inner` whenever `Host` declares one — even with a top-level `Inner` also
// present. Rewriting every relative name to `.pkg.` + name ignores the scope and
// silently REBINDS to a different message: measured on the previous version,
// `probe.Host.Inner` became `probe.Inner`.
//
// Nothing errors when that happens, which is why it needs a test rather than a
// panic. The rewritten name is a perfectly valid reference to a real type, so
// protodesc builds the file happily and the field simply points somewhere else.
// Records written through it would carry the wrong message's fields.
//
// Two of the three arms are the two directions this can go wrong: the rewrite
// must move a nested reference to the NESTED type, and must still reach the
// top-level type when the declaring scope has no candidate of its own.
//
// The third is the MULTI-COMPONENT axis, and it is where this resolver
// deliberately departs from the `protodesc` it feeds. For `A.B`, protobuf's
// language rule — protoc's — resolves the FIRST component outward and then
// requires the rest beneath it, so `A.B` inside `Host` means `Host.A.B` when
// `Host.A` exists, and FAILS rather than falling back to `.pkg.A.B`. protoc
// says so out loud: "is resolved to probe.Host.A.B, which is not defined. The
// innermost scope is searched first in name resolution." protodesc instead
// retries the whole reference at each level and would bind `.probe.A.B`.
//
// This follows protoc, because protoc is what compiled every descriptor that
// can reach here and Java resolves the same way — a shape protoc rejects cannot
// come out of a .proto file, so matching protodesc's laxer walk would only
// paper over a descriptor no compiler produced. The arm pins the strict answer
// so the departure is a decision on record rather than an accident.
func TestAbsolutizeFieldTypeNamesPreservesLexicalScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// hostDeclaresInner controls whether the shadowing nested type exists.
		hostDeclaresInner bool
		wantTypeName      string
		wantBinding       string
	}{
		{
			// The shadowing case. `.probe.Inner` exists too, so a scope-blind
			// rewrite produces a valid name for the WRONG message.
			name:              "a nested type shadows the top-level one",
			hostDeclaresInner: true,
			wantTypeName:      ".probe.Host.Inner",
			wantBinding:       "probe.Host.Inner",
		},
		{
			// The control. With nothing to shadow it, the same relative name must
			// still resolve outward to the package-level type — otherwise the fix
			// would have traded a rebinding bug for a resolution failure.
			name:              "no nested type, so the outward walk reaches the package",
			hostDeclaresInner: false,
			wantTypeName:      ".probe.Inner",
			wantBinding:       "probe.Inner",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			build := func() *descriptorpb.FileDescriptorProto {
				host := &descriptorpb.DescriptorProto{
					Name: proto.String("Host"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name:   proto.String("f"),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						// RELATIVE, which is the whole subject.
						TypeName: proto.String("Inner"),
					}},
				}
				if tc.hostDeclaresInner {
					host.NestedType = []*descriptorpb.DescriptorProto{
						{Name: proto.String("Inner")},
					}
				}
				return &descriptorpb.FileDescriptorProto{
					Name:    proto.String("absolutize_scope.proto"),
					Package: proto.String("probe"),
					Syntax:  proto.String("proto2"),
					MessageType: []*descriptorpb.DescriptorProto{
						{Name: proto.String("Inner")},
						host,
					},
				}
			}

			// What protobuf itself binds the untouched relative name to. This is
			// the authority the rewrite must agree with, rather than a value
			// hard-coded here and asserted against itself.
			untouched, err := protodesc.NewFile(build(), nil)
			if err != nil {
				t.Fatalf("building the untouched descriptor: %v", err)
			}
			wantBinding := untouched.Messages().ByName("Host").Fields().ByName("f").Message().FullName()
			if string(wantBinding) != tc.wantBinding {
				t.Fatalf("protobuf binds the untouched name to %s, but this arm is written against %s — "+
					"the fixture no longer expresses the shape it is named for", wantBinding, tc.wantBinding)
			}

			rewritten := build()
			absolutizeFieldTypeNames(rewritten)

			if got := rewritten.MessageType[1].Field[0].GetTypeName(); got != tc.wantTypeName {
				t.Fatalf("absolutizeFieldTypeNames produced type_name %q, want %q", got, tc.wantTypeName)
			}

			after, err := protodesc.NewFile(rewritten, nil)
			if err != nil {
				t.Fatalf("building the absolutized descriptor: %v", err)
			}
			got := after.Messages().ByName("Host").Fields().ByName("f").Message().FullName()
			if got != wantBinding {
				t.Fatalf("absolutization REBOUND the field: %s -> %s.\n"+
					"The rewritten name is valid, so protodesc reports no error and the field simply "+
					"points at a different message — records written through it carry the wrong "+
					"message's fields.", wantBinding, got)
			}
		})
	}
}

// THE MULTI-COMPONENT AXIS, kept out of the table above because its assertion is
// about the rewritten NAME rather than about a binding that survives: the shape
// it pins is one protodesc refuses to build, by design.
//
// `A.B` written inside `Host`, where `Host.A` exists but declares no `B`, and a
// top-level `.probe.A.B` does exist. protoc resolves the first component
// outward, finds `Host.A`, requires `B` beneath it, and ERRORS -- it does not
// continue outward to `.probe.A`. protodesc retries the whole reference per
// level and would bind `.probe.A.B` instead.
//
// This resolver follows protoc, so it produces `.probe.Host.A.B`. Feeding that
// to protodesc then fails loudly, which is the correct outcome for a descriptor
// no compiler could have emitted -- and is why the arm asserts the NAME and the
// resulting error rather than a successful binding.
func TestAbsolutizeFieldTypeNamesResolvesTheFirstComponentOutward(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("absolutize_multicomponent.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			// .probe.A.B exists at the top level -- the binding protodesc's
			// laxer walk would have chosen.
			{
				Name:       proto.String("A"),
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("B")}},
			},
			{
				Name: proto.String("Host"),
				// Host.A exists but has NO nested B, so the first component
				// matches here and the rest does not.
				NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("A")}},
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("f"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String("A.B"),
				}},
			},
		},
	}

	absolutizeFieldTypeNames(fd)

	got := fd.MessageType[1].Field[0].GetTypeName()
	if got != ".probe.Host.A.B" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q.\n"+
			"The first component `A` resolves to Host.A in the innermost scope that declares it, "+
			"and the remainder is required beneath it -- protoc does not continue outward to "+
			"`.probe.A.B`, and neither does this. Producing `.probe.A.B` would silently bind a "+
			"reference protoc rejects.", got, ".probe.Host.A.B")
	}

	// And the consequence, asserted rather than described: protodesc refuses it.
	// That is the intended outcome -- a loud failure on a descriptor no compiler
	// emits, rather than a quiet binding to a different message.
	if _, err := protodesc.NewFile(fd, nil); err == nil {
		t.Fatal("protodesc accepted `.probe.Host.A.B`, so either Host.A gained a nested B or the " +
			"resolver stopped following protoc's first-component rule. Re-read the doc comment " +
			"before relaxing this: the departure from protodesc is deliberate")
	}
}

// THE FALLBACK ARM: a name this file does not declare.
//
// This is the half of the resolver that repaired twelve cross-engine SQL
// scenarios, and for two commits it was reached by NO test in this package --
// proven, not assumed: a panic() planted at the fallback left the whole package
// green, while the same panic on the walk branch reddened it. The scope walk had
// two arms and the fallback had none, so the arm that actually broke production
// was the untested one.
//
// The shape is the relational DDL builder's, which is the producer that emits
// relative names at all: a file with NO package, a message-scoped field, and a
// dotted name binding into a dependency. Nothing in the file declares `com`, so
// the outward walk finds no candidate and the package prefix -- here just "." --
// is the answer. That is exactly the rewrite the old blanket code performed, and
// getting it wrong made protodesc resolve against the enclosing message and fail
// with `cannot resolve type "T_UUID.com.apple.foundationdb.record.UUID"`.
func TestAbsolutizeFieldTypeNamesFallsBackForNamesTheFileDoesNotDeclare(t *testing.T) {
	t.Parallel()

	fd := &descriptorpb.FileDescriptorProto{
		Name:   proto.String("absolutize_fallback.proto"),
		Syntax: proto.String("proto2"),
		// NO package, which is what makes the prefix a bare "." and is the shape
		// the DDL builder emits.
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("T_UUID"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("u"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				// Dotted, and binding into a DEPENDENCY: no scope in this file
				// declares `com`, so the walk must fall through.
				TypeName: proto.String("com.apple.foundationdb.record.UUID"),
			}},
		}},
	}

	// The premise, asserted so this cannot pass because the walk happened to
	// find something: the file declares no `com`.
	for _, m := range fd.MessageType {
		if m.GetName() == "com" {
			t.Fatal("the fixture declares a top-level `com`, so the outward walk would resolve it " +
				"and this arm would exercise the walk rather than the fallback")
		}
	}

	absolutizeFieldTypeNames(fd)

	got := fd.MessageType[0].Field[0].GetTypeName()
	if got != ".com.apple.foundationdb.record.UUID" {
		t.Fatalf("absolutizeFieldTypeNames produced %q, want %q.\n"+
			"A name no scope in this file declares must fall back to the package prefix. Leaving it "+
			"RELATIVE is what broke twelve cross-engine SQL scenarios, and rewriting it to anything "+
			"else would bind a different type.", got, ".com.apple.foundationdb.record.UUID")
	}
}
