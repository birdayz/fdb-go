package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// --- BITMAP_VALUE metadata builder ---

// buildBitmapValueMetadata creates metadata with a BITMAP_VALUE index where
// price is the position column (ungrouped). Entry size defaults to 10000.
func buildBitmapValueMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewBitmapValueIndex("bitmap_price",
		recordlayer.GroupBy(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build bitmap_value metadata: " + err.Error())
	}
	return md
}

// --- Targeted tests ---

// TestBitmapBasicSave saves one record and verifies the correct bit is set.
func TestBitmapBasicSave(t *testing.T) {
	t.Parallel()
	md := buildBitmapValueMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
	s.Verify() // bit 42 in aligned entry at position 0 should be set
}

// TestBitmapMultipleRecords saves records with different prices and verifies each bit.
func TestBitmapMultipleRecords(t *testing.T) {
	t.Parallel()
	md := buildBitmapValueMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(500)})
	s.Verify() // bits 10, 100, 500 should all be set
}

// TestBitmapDelete saves a record then deletes it, verifying the bit is cleared.
func TestBitmapDelete(t *testing.T) {
	t.Parallel()
	md := buildBitmapValueMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(77)})
	s.Verify()

	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // bit 77 should be cleared, entry may be gone entirely
}

// TestBitmapOverwrite saves, then overwrites with a different price.
// Old bit should be cleared, new bit should be set.
func TestBitmapOverwrite(t *testing.T) {
	t.Parallel()
	md := buildBitmapValueMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)})
	s.Verify() // bit 50 set

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify() // bit 50 cleared, bit 200 set
}

// TestBitmapCommitUnknown injects commit-unknown on a save.
// BIT_OR is idempotent: OR(x, x) = x. Double-apply should be safe.
func TestBitmapCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildBitmapValueMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()
}

// TestBitmapDeleteCommitUnknown saves, then injects commit-unknown on delete.
// BIT_AND is idempotent: AND(x, x) = x. Double-apply should be safe.
func TestBitmapDeleteCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildBitmapValueMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

// --- Random stress tests ---

// TestRandomBitmapNoFaults runs 200 random ops with BITMAP_VALUE and no faults.
func TestRandomBitmapNoFaults(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildBitmapValueMetadata(), RandomConfig{
		Seed:   50050,
		NumOps: 200,
		MaxPKs: 30,
		Faults: FaultsNone,
	})
}

// TestRandomBitmapWithFaults runs 200 random ops with BITMAP_VALUE under 5% commit-unknown.
// BIT_OR and BIT_AND are both idempotent, so bitmap indexes should survive retries.
func TestRandomBitmapWithFaults(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildBitmapValueMetadata(), RandomConfig{
		Seed:   51051,
		NumOps: 200,
		MaxPKs: 30,
		Faults: FaultsRetryHeavy,
	})
}

// TestBitmapRefusedEntrySizeHoldsNoEntry drives both arms of the model's check
// of an index whose entry size the maintainer refuses: such an index refuses
// every write, so an empty index verifies clean and any entry in it is a
// violation.
func TestBitmapRefusedEntrySizeHoldsNoEntry(t *testing.T) {
	t.Parallel()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	idx := recordlayer.NewBitmapValueIndex("bitmap_price", recordlayer.GroupBy(recordlayer.Field("price")))
	idx.SetOption(recordlayer.IndexOptionBitmapValueEntrySize, "0")
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(testRealDB)
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	ctx, cancel := chaosRunContext(0)
	defer cancel()
	verify := func(write bool) []Violation {
		var got []Violation
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(4)}); err == nil {
				t.Fatal("a bitmap index of entry size 0 maintained a write")
			}
			if write {
				rtx.Transaction().Set(store.IndexSubspace(md.GetIndex("bitmap_price")).Pack(tuple.Tuple{int64(0)}), []byte{1})
			}
			got = verifyBitmapValueIndexes(ctx, store, NewStoreModel(md))
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := verify(false); len(got) != 0 {
		t.Fatalf("an empty index of a refused entry size: %v, want no violation", got)
	}
	got := verify(true)
	if len(got) != 1 || got[0].Invariant != "bitmap_refused_size_written" {
		t.Fatalf("an entry in an index of a refused entry size: %v, want bitmap_refused_size_written", got)
	}
}
