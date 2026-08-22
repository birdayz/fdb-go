package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// absolutizeFieldTypeNames HAD A HOLE AT MESSAGE-SCOPED EXTENSIONS.
//
// The function rewrites relative type names to fully-qualified ones so
// rebuildFileDescriptor can resolve them. It recursed into
// `DescriptorProto.NestedType` and handled FILE-level extensions
// (`FileDescriptorProto.Extension`), but never visited
// `DescriptorProto.Extension` -- field 6, the extension block declared INSIDE a
// message, which proto2 allows.
//
// Nothing caught it because the corpus cannot express it: every other fixture
// builds its descriptor from a compiled Go file via
// protodesc.ToFileDescriptorProto, which emits absolute names everywhere, so
// absolutization is a no-op on all of them. The producer that emits relative
// names is the relational DDL builder.
//
// The arms below drive the three extension positions separately -- file-level,
// message-scoped, and nested-message-scoped -- because the first was already
// covered and passing, and a table that lumped them together would have gone
// green on its strength.
func TestAbsolutizeFieldTypeNamesReachesEveryExtensionPosition(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// build returns a descriptor carrying exactly one relative type name,
		// in the position under test.
		build func() *descriptorpb.FileDescriptorProto
		// read returns the type name the arm is about, after absolutization.
		read func(*descriptorpb.FileDescriptorProto) string
	}{
		{
			name: "a field on a top-level message",
			build: func() *descriptorpb.FileDescriptorProto {
				fd := extensionProbeFile()
				fd.MessageType[1].Field = []*descriptorpb.FieldDescriptorProto{
					relativeMessageField("inner", 1),
				}
				return fd
			},
			read: func(fd *descriptorpb.FileDescriptorProto) string {
				return fd.MessageType[1].Field[0].GetTypeName()
			},
		},
		{
			name: "a file-level extension",
			build: func() *descriptorpb.FileDescriptorProto {
				fd := extensionProbeFile()
				fd.Extension = []*descriptorpb.FieldDescriptorProto{
					relativeMessageField("ext_inner", 1000),
				}
				fd.Extension[0].Extendee = proto.String(".probe.Host")
				return fd
			},
			read: func(fd *descriptorpb.FileDescriptorProto) string {
				return fd.Extension[0].GetTypeName()
			},
		},
		{
			// THE HOLE. Legal proto2, and the arm that was silently skipped.
			name: "an extension declared inside a message",
			build: func() *descriptorpb.FileDescriptorProto {
				fd := extensionProbeFile()
				fd.MessageType[1].Extension = []*descriptorpb.FieldDescriptorProto{
					relativeMessageField("ext_inner", 1001),
				}
				fd.MessageType[1].Extension[0].Extendee = proto.String(".probe.Host")
				return fd
			},
			read: func(fd *descriptorpb.FileDescriptorProto) string {
				return fd.MessageType[1].Extension[0].GetTypeName()
			},
		},
		{
			// One level down, so the recursion is shown to carry the fix rather
			// than the top level happening to be handled.
			name: "an extension declared inside a NESTED message",
			build: func() *descriptorpb.FileDescriptorProto {
				fd := extensionProbeFile()
				fd.MessageType[1].NestedType = []*descriptorpb.DescriptorProto{{
					Name: proto.String("Deep"),
					Extension: []*descriptorpb.FieldDescriptorProto{
						relativeMessageField("ext_inner", 1002),
					},
				}}
				fd.MessageType[1].NestedType[0].Extension[0].Extendee = proto.String(".probe.Host")
				return fd
			},
			read: func(fd *descriptorpb.FileDescriptorProto) string {
				return fd.MessageType[1].NestedType[0].Extension[0].GetTypeName()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fd := tc.build()
			if got := tc.read(fd); got != "Inner" {
				t.Fatalf("fixture setup: type name is %q, want the RELATIVE %q — this arm would "+
					"prove nothing, because absolutization is a no-op on an already-absolute name", got, "Inner")
			}

			absolutizeFieldTypeNames(fd)

			if got := tc.read(fd); got != ".probe.Inner" {
				t.Fatalf("absolutizeFieldTypeNames left the type name %q; want %q.\n"+
					"An unabsolutized name reaches the descriptor resolver as written, so it either "+
					"fails to resolve or resolves against the wrong scope.", got, ".probe.Inner")
			}
		})
	}
}

// extensionProbeFile is a proto2 file with a package, an `Inner` message to
// point at, and a `Host` message to extend. Callers attach exactly one relative
// reference in the position they are testing.
func extensionProbeFile() *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:    proto.String("extension_probe.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Inner")},
			{
				Name: proto.String("Host"),
				ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
					{Start: proto.Int32(1000), End: proto.Int32(2000)},
				},
			},
		},
	}
}

// relativeMessageField builds an optional message-typed field whose TypeName is
// RELATIVE — the shape absolutization exists to rewrite.
func relativeMessageField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(number),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String("Inner"),
	}
}
