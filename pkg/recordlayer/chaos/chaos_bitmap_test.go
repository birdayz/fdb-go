package chaos

import (
	"testing"

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
