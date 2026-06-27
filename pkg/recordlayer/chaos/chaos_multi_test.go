package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildMultiIndexMetadata creates metadata with ALL of:
// - VALUE index on price
// - VALUE index on quantity
// - COUNT index grouped by price
// - SUM index on price (ungrouped)
// - Record counting enabled
//
// This exercises the multi-index code path in updateSecondaryIndexes() where
// ALL indexes are iterated in the SAME transaction. Under commit-unknown,
// every index's skip/dedup logic (removeCommonEntries, removeCommonGroupingKeys,
// removeCommonSumEntries) runs in the retry transaction.
func buildMultiIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	// VALUE indexes
	builder.AddIndex("Order", recordlayer.NewIndex("multi_price_idx", recordlayer.Field("price")))
	builder.AddIndex("Order", recordlayer.NewIndex("multi_qty_idx", recordlayer.Field("quantity")))

	// COUNT index grouped by price
	builder.AddIndex("Order", recordlayer.NewCountIndex("multi_count_by_price",
		recordlayer.GroupAll(recordlayer.Field("price"))))

	// SUM index on price (ungrouped)
	builder.AddIndex("Order", recordlayer.NewSumIndex("multi_sum_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))

	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build multi-index metadata: " + err.Error())
	}
	return md
}

// buildMultiTypeMetadata creates metadata with Order and Customer, each with indexes:
// - Order: VALUE index on price
// - Customer: VALUE index on name
// - Universal: COUNT index (ungrouped, counts all records regardless of type)
func buildMultiTypeMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	builder.AddIndex("Order", recordlayer.NewIndex("mtype_order_price", recordlayer.Field("price")))
	builder.AddIndex("Customer", recordlayer.NewIndex("mtype_cust_name", recordlayer.Field("name")))

	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build multi-type metadata: " + err.Error())
	}
	return md
}

// --- Multi-index tests ---

// TestMultiIndexBasicVerify validates the multi-index metadata works at all
// (no faults). Saves records with various prices and quantities, verifies ALL
// indexes are correct.
func TestMultiIndexBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(10)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100), Quantity: proto.Int32(3)})
	s.Verify()

	// Overwrite — changes both price and quantity
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(300), Quantity: proto.Int32(7)})
	s.Verify()

	// Delete
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify()
}

// TestMultiIndexCommitUnknown saves a record under commit-unknown with ALL
// index types active. The retry transaction must correctly handle:
// - VALUE: removeCommonEntries skips identical entries
// - COUNT: removeCommonGroupingKeys skips unchanged keys
// - SUM: removeCommonSumEntries skips identical (key, value) pairs
// All in the SAME transaction.
func TestMultiIndexCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(10)})
	s.Verify()

	// Same price group — COUNT(price=100) must be 2, SUM must be 300
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100), Quantity: proto.Int32(3)})
	s.Verify()
}

// TestMultiIndexOverwriteCommitUnknown changes BOTH price AND quantity under
// commit-unknown. This is the hardest case: VALUE indexes for price and
// quantity both need old entries removed and new entries added. COUNT and SUM
// also change grouping/values.
func TestMultiIndexOverwriteCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	// Overwrite: price 100→200, quantity 5→10
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200), Quantity: proto.Int32(10)})
	s.Verify()

	// Overwrite again: same PK, different values
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50), Quantity: proto.Int32(1)})
	s.Verify()
}

// TestMultiIndexDeleteCommitUnknown deletes a record under commit-unknown
// with ALL index types active. The retry sees the record is already gone.
func TestMultiIndexDeleteCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(10)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()

	// Delete the other one too
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify()
}

// TestMultiIndexDeleteAllCommitUnknown runs DeleteAllRecords under
// commit-unknown. First commit clears all subspaces. Retry sees empty store
// and does another round of clears. This must be safe: clearing empty ranges
// is a no-op in FDB, and the record count reset to 0 is idempotent.
func TestMultiIndexDeleteAllCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Populate several records across different price groups
	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(i),
			Price:    proto.Int32(int32(i%3) * 100),
			Quantity: proto.Int32(int32(i)),
		})
	}
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteAllRecords()
	s.Verify()
}

// TestMultiIndexDeleteAllThenSaveCommitUnknown does delete-all then a save,
// both with faults. This tests the interaction: after delete-all committed,
// the retry re-deletes (idempotent). Then a new save also commits and retries.
func TestMultiIndexDeleteAllThenSaveCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 5; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(i),
			Price:    proto.Int32(int32(i * 50)),
			Quantity: proto.Int32(int32(i * 2)),
		})
	}
	s.Verify()

	// Delete all with fault
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteAllRecords()
	s.Verify()

	// Save new record with fault on clean store
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(777), Quantity: proto.Int32(42)})
	s.Verify()
}

// TestMultiIndexRandomHeavyStress runs 500 random ops across 50 PKs with
// continuous 5% commit-unknown fault injection and ALL four index types active.
// Verifies every 50 ops. Seed is fixed for reproducibility.
func TestMultiIndexRandomHeavyStress(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(31337), WithFaults(FaultsRetryHeavy))

	const numOps = 500
	const verifyEvery = 50
	const maxPK = int64(50)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.65 {
			// 65% saves — random price + quantity
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(5) * 100), // small price space → collisions
				Quantity: proto.Int32(s.Rng.Int32N(20) + 1),
			})
		} else if s.Rng.Float64() < 0.85 {
			// ~20% deletes
			s.DeleteRecord(tuple.Tuple{pk})
		} else {
			// ~15% delete-all
			s.DeleteAllRecords()
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestMultiIndexRapidOverwrite overwrites the SAME PK 50 times with different
// price and quantity values under continuous fault injection. This hammers the
// removeCommonEntries/removeCommonGroupingKeys/removeCommonSumEntries logic
// in the retry path — each retry must correctly detect "old record was already
// updated by the first commit" and skip the duplicate work.
func TestMultiIndexRapidOverwrite(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(11111), WithFaults(FaultsRetryHeavy))

	for i := 0; i < 50; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(1),
			Price:    proto.Int32(int32(i) * 10),
			Quantity: proto.Int32(int32(i) + 1),
		})
	}
	s.Verify()

	t.Logf("50 overwrites completed, %d faults injected", len(s.FaultLog()))
}

// TestMultiIndexDeleteAndReinsert cycles save→delete→save→delete on the SAME PK
// under fault injection. Each transition touches all 4 index types.
func TestMultiIndexDeleteAndReinsert(t *testing.T) {
	t.Parallel()
	md := buildMultiIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(22222), WithFaults(FaultsRetryHeavy))

	for i := 0; i < 25; i++ {
		// Save
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(1),
			Price:    proto.Int32(int32(i)*100 + 50),
			Quantity: proto.Int32(int32(i) + 1),
		})
		// Delete
		s.DeleteRecord(tuple.Tuple{int64(1)})
	}
	s.Verify()

	// Final save to leave one record
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(999), Quantity: proto.Int32(99)})
	s.Verify()

	t.Logf("25 save-delete cycles + final save, %d faults injected", len(s.FaultLog()))
}

// --- Multi-type tests ---

// TestMultiTypeSaveCommitUnknown interleaves Order and Customer saves under
// commit-unknown. Order has a price VALUE index, Customer has a name VALUE index.
// The retry must correctly handle two different record types with different indexes.
func TestMultiTypeSaveCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiTypeMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save Order under fault
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Save Customer under fault
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")})
	s.Verify()

	// Interleaved saves — different types, different PKs
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Bob")})
	s.Verify()
}

// TestMultiTypeDeleteAllCommitUnknown runs DeleteAllRecords with both Order
// and Customer records present under commit-unknown. Verifies all type-specific
// indexes are properly cleared.
func TestMultiTypeDeleteAllCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiTypeMetadata()
	s := NewScenario(t, testRealDB, md)

	// Populate both types
	for i := int64(1); i <= 5; i++ {
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
		s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Customer")})
	}
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteAllRecords()
	s.Verify()

	// Save after delete-all
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42)})
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(99), Name: proto.String("NewCustomer")})
	s.Verify()
}

// TestMultiTypeRandomStress runs random interleaved Order and Customer operations
// with fault injection. Tests the multi-type code path in updateSecondaryIndexes().
func TestMultiTypeRandomStress(t *testing.T) {
	t.Parallel()
	md := buildMultiTypeMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(44444), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 25

	for i := 0; i < numOps; i++ {
		if s.Rng.Float64() < 0.5 {
			// Order operation
			pk := s.Rng.Int64N(30) + 1
			if s.Rng.Float64() < 0.7 {
				s.SaveRecord(&gen.Order{
					OrderId: proto.Int64(pk),
					Price:   proto.Int32(s.Rng.Int32N(1000)),
				})
			} else {
				s.DeleteRecord(tuple.Tuple{pk})
			}
		} else {
			// Customer operation
			pk := s.Rng.Int64N(30) + 1
			if s.Rng.Float64() < 0.7 {
				s.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(pk),
					Name:       proto.String("Cust"),
				})
			} else {
				s.DeleteRecord(tuple.Tuple{pk})
			}
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()

	t.Logf("completed %d multi-type ops, %d faults injected", numOps, len(s.FaultLog()))
}
