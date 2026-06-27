package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// --- MAX_EVER_LONG metadata builders ---

// buildMaxEverMetadata creates metadata with a MAX_EVER_LONG index on price (ungrouped).
func buildMaxEverMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewMaxEverLongIndex("max_ever_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build max_ever metadata: " + err.Error())
	}
	return md
}

// buildMinEverMetadata creates metadata with a MIN_EVER_LONG index on price (ungrouped).
func buildMinEverMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewMinEverLongIndex("min_ever_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build min_ever metadata: " + err.Error())
	}
	return md
}

// buildGroupedMaxEverMetadata creates metadata with a grouped MAX_EVER_LONG index:
// grouped by quantity, aggregating max of price.
func buildGroupedMaxEverMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewMaxEverLongIndex("max_ever_price_by_qty",
		recordlayer.GroupBy(recordlayer.Field("price"), recordlayer.Field("quantity"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build grouped max_ever metadata: " + err.Error())
	}
	return md
}

// --- MAX_EVER_LONG tests ---

// TestMaxEverBasicVerify verifies the model and store agree after simple saves.
// No fault injection — validates the EVER tracking framework itself.
func TestMaxEverBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // MAX_EVER should be 100

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify() // MAX_EVER should be 200

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50)})
	s.Verify() // MAX_EVER should still be 200
}

// TestMaxEverCommitUnknown injects commit-unknown on a single save.
// atomic MAX is idempotent: MAX(x, x) = x. Should be safe.
func TestMaxEverCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // MAX_EVER must be 100
}

// TestMaxEverIncreasingValues saves records with increasing prices.
// MAX_EVER should track the highest value.
func TestMaxEverIncreasingValues(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
		s.Verify()
	}
	// MAX_EVER should be 1000
}

// TestMaxEverDecreasingValues saves records with decreasing prices.
// MAX_EVER should stay at the first (highest) value.
func TestMaxEverDecreasingValues(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(1000)})
	s.Verify()

	for i := int64(2); i <= 10; i++ {
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(1100 - i*100))})
		s.Verify()
	}
	// MAX_EVER should stay at 1000
}

// TestMaxEverDeleteIsNoOp verifies that deleting a record does NOT decrease MAX_EVER.
// This is the core _EVER semantic: irreversible aggregate.
func TestMaxEverDeleteIsNoOp(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.Verify() // MAX_EVER = 500

	// Delete the record with max price — MAX_EVER should NOT decrease.
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // MAX_EVER must still be 500
}

// TestMaxEverOverwriteHigherCommitUnknown overwrites with a higher value under commit-unknown.
// First commit: MAX(old=100, new=200) → 200. Retry sees new=200, does MAX(200) → 200. Safe.
func TestMaxEverOverwriteHigherCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify() // MAX_EVER must be 200
}

// TestMaxEverOverwriteLowerCommitUnknown overwrites with a lower value under commit-unknown.
// Record has price=200, overwrite with price=100. EVER means MAX stays at 200.
func TestMaxEverOverwriteLowerCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // MAX_EVER must still be 200
}

// TestMaxEverZeroValue verifies that zero is handled correctly.
// FDB atomic MAX with unsigned comparison: 0 is the minimum possible value.
func TestMaxEverZeroValue(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(0)})
	s.Verify() // MAX_EVER = 0

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(1)})
	s.Verify() // MAX_EVER = 1
}

// TestMaxEverDeleteAllResets verifies that DeleteAllRecords resets the EVER aggregate.
func TestMaxEverDeleteAllResets(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(999)})
	s.Verify() // MAX_EVER = 999

	s.DeleteAllRecords()
	s.Verify() // MAX_EVER should be gone

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
	s.Verify() // MAX_EVER = 10 (fresh start)
}

// --- MIN_EVER_LONG tests ---

// TestMinEverBasicVerify verifies MIN_EVER tracks correctly without faults.
func TestMinEverBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
	s.Verify() // MIN_EVER = 500

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.Verify() // MIN_EVER = 100

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(999)})
	s.Verify() // MIN_EVER should still be 100
}

// TestMinEverCommitUnknown injects commit-unknown on a single save.
// atomic MIN is idempotent: MIN(x, x) = x. Should be safe.
func TestMinEverCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // MIN_EVER must be 100
}

// TestMinEverOverwriteCommitUnknown changes value under commit-unknown.
// Overwrite from higher to lower: MIN ratchets down.
func TestMinEverOverwriteCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // MIN_EVER must be 100
}

// TestMinEverOverwriteHigherCommitUnknown overwrites with a higher value under commit-unknown.
// MIN_EVER = 100, then overwrite record with 500. MIN stays at 100 (EVER semantics).
func TestMinEverOverwriteHigherCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
	s.Verify() // MIN_EVER must still be 100
}

// TestMinEverDeleteIsNoOp verifies that deleting a record does NOT increase MIN_EVER.
func TestMinEverDeleteIsNoOp(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(500)})
	s.Verify() // MIN_EVER = 100

	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // MIN_EVER must still be 100
}

// TestMinEverZeroValue verifies that zero is the lowest possible value.
// Once 0 is set via MIN, nothing can go lower (non-negative constraint).
func TestMinEverZeroValue(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // MIN_EVER = 100

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(0)})
	s.Verify() // MIN_EVER = 0

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50)})
	s.Verify() // MIN_EVER = 0 (can't go lower)
}

// TestMinEverZeroCommitUnknown verifies zero under commit-unknown.
func TestMinEverZeroCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(0)})
	s.Verify() // MIN_EVER = 0
}

// --- Grouped MAX_EVER tests ---

// TestGroupedMaxEverBasicVerify verifies grouped MAX_EVER tracks per-group maxima.
func TestGroupedMaxEverBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify() // group qty=5: MAX=100

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(5)})
	s.Verify() // group qty=5: MAX=200

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50), Quantity: proto.Int32(10)})
	s.Verify() // group qty=5: MAX=200, group qty=10: MAX=50
}

// TestGroupedMaxEverCommitUnknown verifies grouped MAX_EVER under commit-unknown.
func TestGroupedMaxEverCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300), Quantity: proto.Int32(5)})
	s.Verify()
}

// --- Random stress tests ---

// TestMaxEverRandomStress runs 200 random ops with MAX_EVER + FaultsRetryHeavy.
func TestMaxEverRandomStress(t *testing.T) {
	t.Parallel()
	md := buildMaxEverMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(11111), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(10000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("MAX_EVER stress: completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestMinEverRandomStress runs 200 random ops with MIN_EVER + FaultsRetryHeavy.
func TestMinEverRandomStress(t *testing.T) {
	t.Parallel()
	md := buildMinEverMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(22222), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(10000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("MIN_EVER stress: completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestMinMaxEverMixedRandomStress uses both MIN_EVER and MAX_EVER indexes simultaneously.
func TestMinMaxEverMixedRandomStress(t *testing.T) {
	t.Parallel()
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewMaxEverLongIndex("max_ever_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	builder.AddIndex("Order", recordlayer.NewMinEverLongIndex("min_ever_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("failed to build mixed metadata: %v", err)
	}

	s := NewScenario(t, testRealDB, md, WithSeed(33333), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(10000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("Mixed MIN/MAX_EVER stress: completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}
