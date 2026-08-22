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
// The two arms are the two directions this can go wrong: the rewrite must move a
// nested reference to the NESTED type, and must still reach the top-level type
// when the declaring scope has no candidate of its own.
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
