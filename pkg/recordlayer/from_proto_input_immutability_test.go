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

	// THE MESSAGE-SCOPED EXTENSION, guarded BY POSITION rather than by the
	// aggregate count above. `depRelative` is satisfied by the plain field
	// alone, so deleting the extension would leave this test green with the
	// third write site unprobed again — which is the defect the extension was
	// added to close, one level up. Count the position itself.
	extRelative := 0
	for _, d := range md.Dependencies {
		for _, m := range d.GetMessageType() {
			for _, ext := range m.GetExtension() {
				if tn := ext.GetTypeName(); tn != "" && tn[0] != '.' {
					extRelative++
				}
			}
		}
	}
	if extRelative == 0 {
		t.Fatal("no dependency declares a MESSAGE-SCOPED extension with a relative type name. " +
			"absolutizeFieldTypeNames writes at three POSITIONS -- message fields, message-scoped " +
			"extensions, file-level extensions -- and this fixture would reach only two, so " +
			"TestRecordMetaDataFromProtoDoesNotMutateItsInput would not cover the third")
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
					// ABSOLUTE, and overwritten to the same value below before
					// relativizeTypeNames runs. An earlier revision wrote
					// "Inner" here under a comment calling it "relative on
					// purpose" -- dead, because line ~208 assigns this field
					// before anything reads it. Setting it to garbage left the
					// package green. That is the trap the comment twenty lines
					// down was written to warn about, reintroduced beside it.
					TypeName: proto.String(".probe.Inner"),
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
					// ABSOLUTE here, and relativized below by the helper. An
					// earlier revision wrote it relative and appended the
					// dependency AFTER the only relativizeTypeNames call, so the
					// helper's message-scoped-extension loop was never executed:
					// deleting that loop left every test green while the comment
					// claimed the mirror was in step with the function it
					// mirrors. Dead code in a fixture builder reads exactly like
					// working code.
					TypeName: proto.String(".probe.Inner"),
				}},
			},
		},
	}
	// The dependency goes through the SAME helper as the records descriptor, so
	// its relative names are produced by the code under discussion rather than
	// typed in already-relative.
	dep.MessageType[1].Field[0].TypeName = proto.String(".probe.Inner")
	relativizeTypeNames(dep)

	// Position-specific: the aggregate relative count is satisfied by the plain
	// field alone, so it cannot tell whether the extension arm ran.
	if got := dep.MessageType[1].Extension[0].GetTypeName(); got != "Inner" {
		t.Fatalf("relativizeTypeNames left the dependency's message-scoped extension type name as "+
			"%q, want %q -- the helper's DescriptorProto.Extension loop did not run, so this "+
			"fixture cannot exercise the third position absolutizeFieldTypeNames writes", got, "Inner")
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
		// EXTENDEE TOO, because absolutizeFieldTypeNames writes it. This mirror
		// lagged that by a lap: the production pass gained an Extendee write and
		// this kept rewriting TypeName only, so every fixture reaching it left
		// extendees ALREADY ABSOLUTE, `resolveName` returned early on them, and
		// deleting the production `f.Extendee = &abs` line reddened nothing —
		// 177 tests, 0 failures. A mirror that lags its subject does not merely
		// under-test; it makes the subject's new code unreachable, which is the
		// failure the "keep the two walks in step" note below exists to prevent.
		if e := f.GetExtendee(); e != "" {
			f.Extendee = proto.String(lastNameComponent(e))
		}
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

// countTypeNameShapes reports how many type references are relative (no leading
// dot) versus absolute, over everything relativizeTypeNames rewrites, so the
// vacuity guard counts what the fixture actually built.
//
// THAT IS THREE POSITIONS TIMES TWO NAME FIELDS. The positions are message
// fields, message-scoped extensions and file-level extensions -- nested types
// are recursion into those, not a fourth. The name fields are `type_name` AND
// `extendee`, because absolutizeFieldTypeNames rewrites both.
//
// The extendee half was missing here for a lap after the production pass gained
// it, so a fixture could carry a relative extendee and the guard would report it
// as no relative names at all. Two earlier revisions of this enumeration were
// wrong in the other direction, counting nested types as a fourth position and
// saying "four" twelve lines under a comment saying "three"; the distinction
// between a POSITION and a NAME FIELD is what both were missing.
func countTypeNameShapes(fd *descriptorpb.FileDescriptorProto) (relative, absolute int) {
	if fd == nil {
		return 0, 0
	}
	tallyName := func(name string) {
		if name == "" {
			return
		}
		if strings.HasPrefix(name, ".") {
			absolute++
		} else {
			relative++
		}
	}
	// BOTH name fields, because absolutizeFieldTypeNames rewrites both.
	tally := func(f *descriptorpb.FieldDescriptorProto) {
		tallyName(f.GetTypeName())
		tallyName(f.GetExtendee())
	}
	var walk func(msgs []*descriptorpb.DescriptorProto)
	walk = func(msgs []*descriptorpb.DescriptorProto) {
		for _, m := range msgs {
			for _, f := range m.Field {
				tally(f)
			}
			for _, ext := range m.Extension {
				tally(ext)
			}
			walk(m.NestedType)
		}
	}
	walk(fd.MessageType)
	for _, ext := range fd.Extension {
		tally(ext)
	}
	return relative, absolute
}
