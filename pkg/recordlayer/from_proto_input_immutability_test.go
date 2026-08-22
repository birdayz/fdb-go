package recordlayer

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
)

// RecordMetaDataFromProto MUST NOT MUTATE THE PROTO IT IS HANDED, because
// SaveRecordMetaData calls it and then marshals the SAME pointer: a mutation
// here is written to FDB.
//
// It did. `rebuildFileDescriptor` opens with `absolutizeFieldTypeNames`, which
// rewrites `f.TypeName` and `ext.TypeName` IN PLACE, and it was handed
// `md.Records` and `md.Dependencies` directly while a comment above claimed it
// operated on a clone. A descriptor carrying RELATIVE type names -- which the
// relational DDL builder emits -- was therefore persisted absolutized.
//
// WHY NOTHING CAUGHT IT, which is the part worth keeping: the adjacent
// round-trip test in metadata_proto_fidelity_test.go asserts
// `proto.Equal(original, got)` where `original` is the very object passed to
// FromProto. Pre-fix the absolutization landed on BOTH sides, so the two moved
// together and the equality held -- a paired assertion whose sides share a
// derivation cannot detect a change common to both. And the corpus made it
// worse: every other fixture builds `Records` from a compiled Go descriptor via
// protodesc.ToFileDescriptorProto, which already emits ABSOLUTE names, so
// absolutization is a no-op on all of them and the bug had no reachable input
// to fire on.
//
// This test closes both gaps: it snapshots the input into an independent clone
// (so the two sides do not share a derivation), and it supplies the one shape
// the corpus lacks (a relative type name).
func TestRecordMetaDataFromProtoDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	input := relativeTypeNameMetaData(t)

	// The independent side. proto.Clone, not a second call to the builder: the
	// point is a snapshot that cannot move when `input` does.
	before := proto.Clone(input).(*gen.MetaData)

	if _, err := RecordMetaDataFromProto(input); err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}

	if !proto.Equal(before, input) {
		t.Fatalf("RecordMetaDataFromProto MUTATED the proto it was given.\n"+
			"SaveRecordMetaData marshals this same pointer after calling this function, so the "+
			"mutation reaches FDB.\n  before: %v\n  after:  %v", before, input)
	}
}

// The guard that keeps the test above from passing vacuously. If the fixture
// stops carrying a relative type name, the function under test has nothing to
// absolutize, `proto.Equal` holds trivially, and the green above becomes a
// statement about the fixture rather than about the code.
func TestRelativeTypeNameFixtureActuallyCarriesARelativeName(t *testing.T) {
	t.Parallel()

	md := relativeTypeNameMetaData(t)
	relative, absolute := countTypeNameShapes(md.Records)
	if relative == 0 {
		t.Fatalf("the fixture carries %d absolute and 0 RELATIVE type names, so "+
			"TestRecordMetaDataFromProtoDoesNotMutateItsInput would exercise no absolutization "+
			"at all and pass no matter what the function does", absolute)
	}
	// (There is no `relative+absolute == 0` check here. An earlier revision had
	// one, labelled a positive control; the check above already fatals unless
	// `relative >= 1`, so the sum could never be zero and the assertion could
	// never fire. A control that is unreachable is worse than no control,
	// because it reads as coverage.)

	// THE SECOND ARM, guarded separately because it was silently empty. The
	// function clones md.Records AND every element of md.Dependencies; with no
	// dependencies the second loop never runs and reverting it changes nothing.
	if len(md.Dependencies) == 0 {
		t.Fatal("the fixture carries no dependencies, so the dependency-clone loop in " +
			"RecordMetaDataFromProto is never executed and TestRecordMetaDataFromProtoDoesNotMutateItsInput " +
			"covers only the records descriptor")
	}
	depRelative, depAbsolute := 0, 0
	for _, d := range md.Dependencies {
		r, a := countTypeNameShapes(d)
		depRelative += r
		depAbsolute += a
	}
	if depRelative == 0 {
		t.Fatalf("the fixture's %d dependencies carry %d absolute and 0 relative type names, so "+
			"absolutization is a no-op on all of them and the dependency arm is covered in name only",
			len(md.Dependencies), depAbsolute)
	}

	t.Logf("fixture type names: records %d relative / %d absolute; %d dependencies, %d relative / %d absolute",
		relative, absolute, len(md.Dependencies), depRelative, depAbsolute)
}

// relativeTypeNameMetaData returns a metadata proto whose records descriptor
// carries at least one RELATIVE field type name.
//
// The demo descriptor is built from a compiled Go file descriptor, which always
// emits fully-qualified `.pkg.Message` names, so the relative shape is created
// here by stripping the leading dot and package from every message-typed field.
// That is the shape the relational DDL builder produces natively; constructing
// it by hand keeps this test inside pkg/recordlayer rather than importing the
// SQL layer.
func relativeTypeNameMetaData(t *testing.T) *gen.MetaData {
	t.Helper()

	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.AddIndex("Order", NewIndex("by_price", Field("price")))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("fixture ToProto: %v", err)
	}
	relativizeTypeNames(p.Records)

	// A DEPENDENCY, because RecordMetaDataFromProto clones TWO things and only
	// one of them was reachable. The demo descriptor's single import is
	// record_metadata_options.proto, which defaultExcludedDependencies strips,
	// so `p.Dependencies` came out EMPTY and the loop that clones each
	// dependency never executed -- reverting it left this whole test green.
	// That is the same untested-arm defect one level over from the one this file
	// exists to close, so the fixture carries a dependency of its own.
	dep := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("input_immutability_probe.proto"),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Inner"),
				ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
					{Start: proto.Int32(1000), End: proto.Int32(2000)},
				},
			},
			{
				Name: proto.String("Outer"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   proto.String("inner"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					// Relative on purpose: this is the byte absolutization
					// rewrites, and the only reason this dependency exists.
					TypeName: proto.String("Inner"),
				}},
				// A MESSAGE-SCOPED EXTENSION, so this fixture reaches the THIRD
				// write site. absolutizeFieldTypeNames gained one when it started
				// visiting DescriptorProto.Extension, and for a lap afterwards
				// this fixture declared none — so the input-immutability arm for
				// that site was unprobed while the mirror functions above claimed
				// to cover it. A mutation site added without a fixture that
				// reaches it is an untested write, and this file's whole subject
				// is writes reaching the caller's proto.
				Extension: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("ext_inner"),
					Number:   proto.Int32(1001),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					Extendee: proto.String(".probe.Inner"),
					TypeName: proto.String("Inner"),
				}},
			},
		},
	}
	p.Dependencies = append(p.Dependencies, dep)
	return p
}

// relativizeTypeNames rewrites `.pkg.Message` to `Message` in the THREE places
// absolutizeFieldTypeNames writes: message fields (recursing through nested
// types), message-scoped extensions, and file-level extensions.
//
// It was two until message-scoped extensions were added to the function it
// mirrors, and this comment said "the two places" for a lap afterwards. A
// mirror that lags its subject silently under-relativizes: a fixture growing an
// extension inside a message would arrive already-absolute, absolutization
// would be a no-op on it, and the arm covering it would pass without exercising
// anything. Keep the two walks in step.
func relativizeTypeNames(fd *descriptorpb.FileDescriptorProto) {
	if fd == nil {
		return
	}
	relativize := func(f *descriptorpb.FieldDescriptorProto) {
		f.TypeName = proto.String(lastNameComponent(f.GetTypeName()))
	}
	var walk func(msgs []*descriptorpb.DescriptorProto)
	walk = func(msgs []*descriptorpb.DescriptorProto) {
		for _, m := range msgs {
			for _, f := range m.Field {
				relativize(f)
			}
			for _, ext := range m.Extension {
				relativize(ext)
			}
			walk(m.NestedType)
		}
	}
	walk(fd.MessageType)
	for _, ext := range fd.Extension {
		relativize(ext)
	}
}

// lastNameComponent turns ".pkg.Outer.Inner" into "Inner" and leaves an empty
// or already-relative name alone.
func lastNameComponent(name string) string {
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// countTypeNameShapes reports how many message-typed field names are relative
// (no leading dot) versus absolute, across messages, nested types,
// message-scoped extensions and file-level extensions -- the same four
// positions relativizeTypeNames rewrites, so the guard counts what the fixture
// actually built.
func countTypeNameShapes(fd *descriptorpb.FileDescriptorProto) (relative, absolute int) {
	if fd == nil {
		return 0, 0
	}
	tally := func(name string) {
		if name == "" {
			return
		}
		if strings.HasPrefix(name, ".") {
			absolute++
		} else {
			relative++
		}
	}
	var walk func(msgs []*descriptorpb.DescriptorProto)
	walk = func(msgs []*descriptorpb.DescriptorProto) {
		for _, m := range msgs {
			for _, f := range m.Field {
				tally(f.GetTypeName())
			}
			for _, ext := range m.Extension {
				tally(ext.GetTypeName())
			}
			walk(m.NestedType)
		}
	}
	walk(fd.MessageType)
	for _, ext := range fd.Extension {
		tally(ext.GetTypeName())
	}
	return relative, absolute
}
