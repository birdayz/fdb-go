package recordlayer

import (
	"strings"
	"testing"

	"fdb.dev/gen"
)

// A SECOND SetRecords IS REFUSED, and this is the pin on a whole family of
// defects being UNREACHABLE rather than merely unobserved.
//
// Java forbids the second setter outright -- both `setRecords` overloads open
// with `if (recordsDescriptor != null) throw new MetaDataException("Records
// already set.")` -- and Go used to permit it. That one divergence was the only
// route to an index registered in the flat registry and associated with no
// record type: the second call replaced every RecordType with a fresh one whose
// index slices were nil while `b.indexes` kept its entry. Build succeeded,
// `RecordTypesForIndex` came back empty, `GetIndexesForRecordType` lost it, and
// `ToProto` emitted the index with an EMPTY RecordType list -- which a reload
// reads as UNIVERSAL, so the index returned either maintained for every record
// type or, when its key is not valid on all of them, as metadata that will not
// load at all.
//
// EVERY REGISTRATION SPELLING IS DRIVEN, and each carries its own control that
// builds WITHOUT the second call. The control is what distinguishes "the guard
// refused" from "this registration never associated anything in the first
// place" -- with a single shared control over one spelling, a registration that
// silently did nothing passed every other arm.
//
// If this test starts failing because the guard was relaxed, the family above
// is re-armed. RFC-238 §7f carries the analysis.
func TestSetRecordsRefusesASecondCall(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		register func(b *RecordMetaDataBuilder, idx *Index)
		// key is the index key. A universal index is validated against EVERY
		// record type, so that arm cannot use a field the demo proto declares
		// on only some of them.
		key KeyExpression
		// associated is how many record types the index is associated with when
		// the builder is used correctly, i.e. with one SetRecords call.
		associated int
	}{
		{
			name:       "AddIndex",
			register:   func(b *RecordMetaDataBuilder, idx *Index) { b.AddIndex("Order", idx) },
			key:        Field("price"),
			associated: 1,
		},
		{
			// Exactly ONE name RETURNS b.AddIndex(recordTypeNames[0], index), so
			// this is the arm above wearing a different call. It is driven
			// anyway: a spelling left to be INFERRED from a delegation is a
			// characterisation, and two successive characterisations of this
			// route were both wrong.
			name: "AddMultiTypeIndex with exactly one name",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order"}, idx)
			},
			key:        Field("price"),
			associated: 1,
		},
		{
			name: "AddMultiTypeIndex with two names",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			},
			key:        Field("price"),
			associated: 2,
		},
		{
			// A nil or empty name list delegates to AddUniversalIndex, whose
			// registry hangs off the BUILDER rather than any RecordType. It was
			// the one spelling the old overwrite could not reach, and it is
			// driven here so the guard is shown to cover it too.
			//
			// EmptyKey, not a field: an early version of this arm used
			// Field("id"), which only TypedRecord declares, so Build refused for
			// a reason unrelated to the claim -- a fact about the KEY chosen,
			// not about the proto, which does carry a field on all three
			// (price).
			name: "AddMultiTypeIndex with an empty name list",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex(nil, idx)
			},
			key:        EmptyKey(),
			associated: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The control: one SetRecords call, so the association must EXIST.
			// Without it, an arm cannot tell the guard's refusal from a
			// registration that associated nothing to begin with.
			ctlIdx := NewIndex("assoc_idx", tc.key)
			ctlMD, err := newDemoBuilder(tc.register, ctlIdx, false).Build()
			if err != nil {
				t.Fatalf("CONTROL Build: %v", err)
			}
			if got := len(ctlMD.RecordTypesForIndex(ctlIdx)); got != tc.associated {
				t.Fatalf("CONTROL: RecordTypesForIndex = %d types, want %d.\n"+
					"This registration associated nothing, so the refusal asserted below\n"+
					"would hold whatever the guard did.", got, tc.associated)
			}

			// The same registration, then a SECOND SetRecords over the SAME
			// descriptor. Java throws here; Go records a build error.
			idx := NewIndex("assoc_idx", tc.key)
			md, err := newDemoBuilder(tc.register, idx, true).Build()
			if err == nil {
				t.Fatalf("Build SUCCEEDED after a second SetRecords.\n"+
					"The guard is gone, and with it the only thing keeping an index from\n"+
					"being registered and associated with nothing: RecordTypesForIndex = %d.",
					len(md.RecordTypesForIndex(idx)))
			}
			if !strings.Contains(err.Error(), "Records already set.") {
				t.Errorf("Build failed with %q, want it to carry Java's wording "+
					"%q — the message is shared with Java deliberately", err, "Records already set.")
			}
		})
	}
}

// AND THE SECOND DESCRIPTOR IS NOT APPLIED, which is what makes the refusal
// safe for a caller that ignores the Build error. Java throws before touching
// any state; Go must reach the same place by returning before the assignment,
// not by recording an error after it. Nothing else pins that ordering: a guard
// written one line lower would still fail Build while leaving the builder
// holding a half-merged descriptor.
func TestRefusedSetRecordsLeavesTheFirstDescriptorInPlace(t *testing.T) {
	t.Parallel()

	idx := NewIndex("first_desc_idx", Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddIndex("Order", idx)

	before := b.GetRecordTypes()["Order"]
	if before == nil {
		t.Fatal("setup: Order missing after the first SetRecords")
	}
	if len(before.indexes) != 1 {
		t.Fatalf("setup: Order carries %d indexes, want 1", len(before.indexes))
	}

	b.SetRecords(gen.File_record_layer_demo_proto)

	after := b.GetRecordTypes()["Order"]
	if after != before {
		t.Error("the refused call REPLACED the RecordType. The guard must return before " +
			"assigning, the way Java throws before touching state; a caller that ignores " +
			"the Build error would otherwise hold a builder whose associations are gone.")
	}
	if len(after.indexes) != 1 {
		t.Errorf("Order carries %d indexes after the refused call, want 1 — the refusal "+
			"discarded the association it was added to protect", len(after.indexes))
	}
}

// THE EMPTY RecordType LIST IS JAVA'S UNIVERSAL ENCODING, not damage. It is
// pinned here because the guard above is justified by what that encoding does
// on reload, and a reader who meets only the justification could conclude the
// reload behaviour is the bug. It is not: `RecordMetaDataFromProto` maps an
// empty list to universal, and Java's proto loop reaches the same place by
// calling `addMultiTypeIndex` with an empty list, which routes to
// `universalIndexes`. Round-tripping a genuinely universal index must therefore
// preserve it, and does.
func TestUniversalIndexRoundTripsThroughAnEmptyRecordTypeList(t *testing.T) {
	t.Parallel()

	idx := NewIndex("universal_roundtrip", EmptyKey())
	md, err := newDemoBuilder(
		func(b *RecordMetaDataBuilder, i *Index) { b.AddUniversalIndex(i) }, idx, false).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := len(md.GetUniversalIndexes()); n != 1 {
		t.Fatalf("setup: %d universal indexes, want 1", n)
	}

	proto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	var emitted bool
	for _, ip := range proto.Indexes {
		if ip.GetName() != "universal_roundtrip" {
			continue
		}
		emitted = true
		if len(ip.RecordType) != 0 {
			t.Errorf("the universal index emitted %d record types, want 0 — an empty list "+
				"IS the encoding, and naming types here would change what Java reads",
				len(ip.RecordType))
		}
	}
	if !emitted {
		t.Fatal("the universal index was not emitted at all")
	}

	reloaded, err := RecordMetaDataFromProto(proto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	if !indexNamed(reloaded.GetUniversalIndexes(), "universal_roundtrip") {
		t.Error("the reloaded index is not universal; the empty-list encoding no longer " +
			"round-trips, which is a wire-compatibility break with Java")
	}
	back := reloaded.GetIndex("universal_roundtrip")
	if back == nil {
		t.Fatal("the index did not survive the round trip")
	}
	if got := len(reloaded.RecordTypesForIndex(back)); got != 3 {
		t.Errorf("RecordTypesForIndex = %d after the round trip, want 3 (a universal index "+
			"applies to every type)", got)
	}
}

// newDemoBuilder builds the three-type demo schema, runs one registration
// spelling, and optionally makes the SECOND SetRecords call the guard refuses.
// Primary keys are set once; the refused call cannot drop them because it does
// not run.
func newDemoBuilder(register func(*RecordMetaDataBuilder, *Index), idx *Index, secondSetRecords bool) *RecordMetaDataBuilder {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	register(b, idx)
	if secondSetRecords {
		b.SetRecords(gen.File_record_layer_demo_proto)
	}
	return b
}

func setDemoPrimaryKeys(b *RecordMetaDataBuilder) {
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
}

func indexNamed(indexes []*Index, name string) bool {
	for _, i := range indexes {
		if i != nil && i.Name == name {
			return true
		}
	}
	return false
}
