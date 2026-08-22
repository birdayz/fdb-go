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
// The route is asserted in prose in two places (the RFC section and the
// recordTypeName field comment in query/plan/plans/aggregate_index.go). Prose
// cannot fail, and two earlier drafts of it were wrong in OPPOSITE directions:
// one had SetRecords "repopulating" the type map (it merges), the other had the
// orphan come from the second descriptor DROPPING the type (a dropped type
// keeps its old entry, indexes intact). So the mechanism is measured here
// rather than only described, and the scope is measured with it: which
// registration forms reach the state, and which one does not.
//
// No guard is implemented yet -- these are the values the builder HAS. When the
// booked §7f guard lands, the two orphan arms are what must move, and their
// failure messages say so.
func TestOverwriteAfterRegistrationOrphansTheIndexAssociation(t *testing.T) {
	t.Parallel()

	// setPKs re-establishes the primary keys, which the overwrite also drops.
	// Without this Build refuses and the test would measure the wrong failure.
	setPKs := func(b *RecordMetaDataBuilder) {
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	}
	build := func(t *testing.T, b *RecordMetaDataBuilder) *RecordMetaData {
		t.Helper()
		md, err := b.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return md
	}

	t.Run("control: without the second SetRecords the association is intact", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("price_ctl", Field("price"))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		b.AddIndex("Order", idx)
		md := build(t, b)

		if got := len(md.RecordTypesForIndex(idx)); got != 1 {
			t.Fatalf("RecordTypesForIndex on a normally-registered index = %d types, want 1.\n"+
				"The two orphan arms below are only meaningful if the association\n"+
				"survives when nothing overwrites it; an implementation that lost it\n"+
				"unconditionally would pass them.", got)
		}
	})

	t.Run("AddIndex then SetRecords orphans it", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("price_single", Field("price"))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		b.AddIndex("Order", idx)
		// The SAME descriptor: "Order" is still declared, so this is not a drop.
		b.SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		md := build(t, b)

		if got := md.RecordTypesForIndex(idx); len(got) != 0 {
			t.Errorf("RecordTypesForIndex = %d types, want 0 (the orphan state).\n"+
				"If a §7f guard now rejects this at Build, that is the fix landing --\n"+
				"replace this arm with the rejection rather than deleting it.", len(got))
		}
		if got := md.GetIndexesForRecordType("Order"); len(got) != 0 {
			t.Errorf("GetIndexesForRecordType(Order) = %d indexes, want 0 -- the same\n"+
				"discard is an index-maintenance hole, not only a plan-typing one.", len(got))
		}
		// The asymmetry that makes it silent: the flat registry still has it, so
		// nothing downstream sees a missing index, only a friendless one.
		if md.GetIndex("price_single") == nil {
			t.Error("GetIndex = nil; the flat index registry was expected to KEEP the entry.\n" +
				"If the overwrite now clears it too, the failure is no longer silent and\n" +
				"§7f's description of the state is stale.")
		}
	})

	t.Run("AddMultiTypeIndex over two names then SetRecords orphans it", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("multi_two", Field("id"))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
		b.SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		md := build(t, b)

		if got := md.RecordTypesForIndex(idx); len(got) != 0 {
			t.Errorf("RecordTypesForIndex = %d types, want 0.\n"+
				"The multi-type form hangs its association off rt.multiTypeIndexes, on\n"+
				"the same RecordType the setter replaces, so it reaches the orphan state\n"+
				"exactly as AddIndex does. §7f scopes the route to both forms on the\n"+
				"strength of this arm.", len(got))
		}
	})

	t.Run("AddMultiTypeIndex with an empty name list does NOT orphan", func(t *testing.T) {
		t.Parallel()
		// EmptyKey, not a field: a universal index is validated against EVERY type,
		// and this proto has no field common to all three.
		idx := NewIndex("multi_universal", EmptyKey())
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		b.AddMultiTypeIndex(nil, idx)
		b.SetRecords(gen.File_record_layer_demo_proto)
		setPKs(b)
		md := build(t, b)

		// This is the NOT-covered arm, and it is why §7f says "either
		// type-associating registration form" rather than "either form": the
		// empty list delegates to AddUniversalIndex, whose registry hangs off
		// the BUILDER and so survives an overwrite of every RecordType.
		if got := md.RecordTypesForIndex(idx); len(got) == 0 {
			t.Error("RecordTypesForIndex = 0 types for a universal index.\n" +
				"§7f asserts the universal delegation is out of reach of the overwrite;\n" +
				"if it is not, the route is wider than the RFC and the field comment say.")
		}
	})
}
