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
	// The positive control: the descriptor must still have real content, so a
	// fixture that lost its fields cannot satisfy the check above by emptiness.
	if relative+absolute == 0 {
		t.Fatal("the fixture descriptor declares no message-typed fields at all")
	}
	t.Logf("fixture type names: %d relative, %d absolute", relative, absolute)
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
	return p
}

// relativizeTypeNames rewrites `.pkg.Message` to `Message` on every
// message-typed field, recursing into nested types, and does the same for
// file-level extensions -- the two places absolutizeFieldTypeNames writes.
func relativizeTypeNames(fd *descriptorpb.FileDescriptorProto) {
	if fd == nil {
		return
	}
	var walk func(msgs []*descriptorpb.DescriptorProto)
	walk = func(msgs []*descriptorpb.DescriptorProto) {
		for _, m := range msgs {
			for _, f := range m.Field {
				f.TypeName = proto.String(lastNameComponent(f.GetTypeName()))
			}
			walk(m.NestedType)
		}
	}
	walk(fd.MessageType)
	for _, ext := range fd.Extension {
		ext.TypeName = proto.String(lastNameComponent(ext.GetTypeName()))
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
// (no leading dot) versus absolute, across messages, nested types and
// file-level extensions.
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
			walk(m.NestedType)
		}
	}
	walk(fd.MessageType)
	for _, ext := range fd.Extension {
		tally(ext.GetTypeName())
	}
	return relative, absolute
}
