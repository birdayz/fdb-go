package recordlayer

import (
	"testing"

	"fdb.dev/gen"
)

// OVERWRITE-AFTER-REGISTRATION: registering an index against a record type and
// THEN calling SetRecords over a descriptor that still declares that type
// discards the association, silently. RFC-238 §7f names this as the one route
// to an index that is neither universal nor associated -- the state that makes
// RecordTypesForIndex return nothing and the aggregate-index plan derive its
// result columns from a nil descriptor.
//
// The route is asserted in prose in three places (the RFC section, the setter
// in metadata.go, and the recordTypeName field comment in
// query/plan/plans/aggregate_index.go). Prose cannot fail, and two early drafts
// of it were wrong in OPPOSITE directions: one had SetRecords "repopulating"
// the type map (it merges), the other had the orphan come from the second
// descriptor DROPPING the type (a dropped type keeps its old entry, indexes
// intact). So the mechanism is measured here, and the SCOPE is measured with
// it: which registration spellings reach the state, and which one does not.
//
// EVERY ARM CARRIES ITS OWN CONTROL, built from the same registration function
// without the second SetRecords. Without it an arm cannot tell "orphaned by the
// overwrite" from "never associated at all" -- a registration that silently did
// nothing satisfies the after-assertion on its own, and one shared control over
// a single spelling does not cover the others.
//
// No guard is implemented yet -- these are the values the builder HAS. When the
// booked §7f guard lands, the three orphan arms are what must move, and their
// failure messages say so.
func TestOverwriteAfterRegistrationOrphansTheIndexAssociation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// register performs one registration spelling against a fresh index.
		register func(b *RecordMetaDataBuilder, idx *Index)
		// key is the index key expression. A universal index is validated
		// against EVERY record type, so that arm cannot use a field the demo
		// proto declares on only some of them.
		key KeyExpression
		// before is how many record types the index is associated with when
		// nothing overwrites; after is how many once SetRecords runs again.
		before int
		after  int
		// universal marks the arm whose index is registered on the BUILDER
		// rather than on any RecordType, which changes what
		// GetIndexesForRecordType can see.
		universal bool
	}{
		{
			name:     "AddIndex",
			register: func(b *RecordMetaDataBuilder, idx *Index) { b.AddIndex("Order", idx) },
			key:      Field("price"),
			before:   1,
			after:    0,
		},
		{
			// Exactly ONE name RETURNS b.AddIndex(recordTypeNames[0], index), so
			// this is the arm above wearing a different call. It is driven
			// anyway: a spelling left to be INFERRED from a delegation is a
			// characterisation, and the two characterisations this route already
			// had were both wrong.
			name: "AddMultiTypeIndex with exactly one name",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order"}, idx)
			},
			key:    Field("price"),
			before: 1,
			after:  0,
		},
		{
			name: "AddMultiTypeIndex with two names",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			},
			key:    Field("price"),
			before: 2,
			after:  0,
		},
		{
			// The spelling the overwrite does NOT reach, and why §7f says
			// "either type-associating registration form" rather than "either
			// form": an empty name list delegates to AddUniversalIndex, whose
			// registry hangs off the BUILDER and so survives an overwrite of
			// every RecordType.
			//
			// EmptyKey, not a field: the first version of this arm used
			// Field("id"), which only TypedRecord declares, so Build refused
			// before the claim was reached -- a fact about the KEY chosen, not
			// about the proto, which does carry a field on all three (price).
			name: "AddMultiTypeIndex with an empty name list",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex(nil, idx)
			},
			key:       EmptyKey(),
			before:    3,
			after:     3,
			universal: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The control: the same registration, no overwrite.
			ctlIdx := NewIndex("assoc_idx", tc.key)
			ctlMD, err := newDemoBuilder(tc.register, ctlIdx, false).Build()
			if err != nil {
				t.Fatalf("control Build: %v", err)
			}
			if got := len(ctlMD.RecordTypesForIndex(ctlIdx)); got != tc.before {
				t.Fatalf("CONTROL: RecordTypesForIndex = %d types, want %d.\n"+
					"The registration associated nothing, so the overwrite arm below would\n"+
					"pass without the overwrite doing a thing — it cannot distinguish\n"+
					"\"orphaned by the overwrite\" from \"never associated\".", got, tc.before)
			}

			// The same registration, then SetRecords over a descriptor that
			// STILL DECLARES the type. Not a drop: it is the same descriptor.
			idx := NewIndex("assoc_idx", tc.key)
			md, err := newDemoBuilder(tc.register, idx, true).Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := len(md.RecordTypesForIndex(idx)); got != tc.after {
				t.Errorf("RecordTypesForIndex = %d types, want %d.\n"+
					"If a §7f guard now rejects this at Build, that is the fix landing --\n"+
					"replace this arm with the rejection rather than deleting it.", got, tc.after)
			}

			// The asymmetry that makes the loss silent: the flat registry keeps
			// the entry, so nothing downstream sees a MISSING index, only a
			// friendless one.
			if md.GetIndex("assoc_idx") == nil {
				t.Error("GetIndex = nil; the flat index registry was expected to KEEP the entry.\n" +
					"If the overwrite now clears it too, the failure is no longer silent and\n" +
					"§7f's description of the state is stale.")
			}

			// The same discard removes the index from GetIndexesForRecordType, so
			// it is an index-maintenance hole and not only a plan-typing one. The
			// universal arm is false in BOTH states rather than tracking `after`:
			// that accessor is the analog of Java getIndexes()+getMultiTypeIndexes()
			// and deliberately excludes universal indexes, which every caller adds
			// back via GetUniversalIndexes (store.go:1079). Deriving the
			// expectation from `after` asserted the opposite and reddened here.
			wantOnOrder := tc.after > 0 && !tc.universal
			if got := indexNamed(md.GetIndexesForRecordType("Order"), "assoc_idx"); got != wantOnOrder {
				t.Errorf("GetIndexesForRecordType(Order) contains the index = %v, want %v", got, wantOnOrder)
			}
		})
	}
}

// AND THE ORPHAN DOES NOT STAY AN ORPHAN: it round-trips through the metadata
// proto as a UNIVERSAL index, which WIDENS the index's scope rather than losing
// it.
//
// ToProto walks the FLAT registry, so the orphaned index is emitted -- with an
// empty RecordType list, because that list is built from the associations that
// were discarded. RecordMetaDataFromProto maps an empty list to "universal".
// Java reads the same bytes the same way: its proto loop calls
// addMultiTypeIndex(recordTypeBuilders, ...) with an EMPTY list, which its
// addMultiTypeIndex routes to universalIndexes. So this is a shared reading of
// one encoding rather than a Go divergence -- and an index meant for one record
// type comes back maintained for all of them.
func TestOrphanedIndexRoundTripsAsUniversal(t *testing.T) {
	t.Parallel()

	idx := NewIndex("orphan_roundtrip", Field("price"))
	md, err := newDemoBuilder(
		func(b *RecordMetaDataBuilder, i *Index) { b.AddIndex("Order", i) }, idx, true).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := len(md.RecordTypesForIndex(idx)); got != 0 {
		t.Fatalf("setup: RecordTypesForIndex = %d, want 0 — the orphan state this test "+
			"round-trips was never reached, so the assertions below say nothing", got)
	}
	if n := len(md.GetUniversalIndexes()); n != 0 {
		t.Fatalf("setup: %d universal indexes before the round trip, want 0", n)
	}

	proto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	reloaded, err := RecordMetaDataFromProto(proto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}

	back := reloaded.GetIndex("orphan_roundtrip")
	if back == nil {
		t.Fatal("the index did not survive the round trip at all")
	}
	if got := len(reloaded.RecordTypesForIndex(back)); got != 3 {
		t.Errorf("after the round trip RecordTypesForIndex = %d types, want 3 (every type).\n"+
			"The orphan is re-read as UNIVERSAL because the RecordType list it emitted is\n"+
			"empty. If a §7f guard now refuses to build or to serialize this metadata, that\n"+
			"is the fix landing — pin the refusal here rather than deleting the arm.", got)
	}
	if !indexNamed(reloaded.GetUniversalIndexes(), "orphan_roundtrip") {
		t.Error("the reloaded index is not in GetUniversalIndexes; the promotion described " +
			"in §7f and at metadata.go's setter no longer happens, and both must be updated")
	}
}

// newDemoBuilder builds the three-type demo schema, runs one registration
// spelling, and optionally performs the OVERWRITE: a second SetRecords over the
// SAME descriptor, which still declares every type. Primary keys are re-set
// afterwards because the overwrite drops those too; without that Build refuses
// and the caller would measure the wrong failure.
func newDemoBuilder(register func(*RecordMetaDataBuilder, *Index), idx *Index, overwrite bool) *RecordMetaDataBuilder {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	register(b, idx)
	if overwrite {
		b.SetRecords(gen.File_record_layer_demo_proto)
		setDemoPrimaryKeys(b)
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
