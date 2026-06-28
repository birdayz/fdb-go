package chaos

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildUniqueIndexMetadata creates metadata with a unique VALUE index on Order.price.
// This means no two orders can share the same price.
func buildUniqueIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("order_price_unique", recordlayer.Field("price")).SetUnique())
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build unique index metadata: " + err.Error())
	}
	return md
}

// TestUniqueIndexBasicVerify validates the chaos framework works with a unique index.
// No fault injection — just saves records with distinct prices and verifies.
func TestUniqueIndexBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()

	// Overwrite record 1 with a different (still unique) price
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(150)})
	s.Verify()
}

// TestUniqueIndexCommitUnknown injects commit-unknown on a single save with a unique index.
// The transaction commits, then re-executes. The retry sees the record already exists
// (same PK) via loadExisting. removeCommonEntries skips the unchanged entry.
// The uniqueness check must NOT falsely fire — the entry belongs to the same record.
func TestUniqueIndexCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // Must not have false uniqueness violation. COUNT must be 1.
}

// TestUniqueIndexOverwriteCommitUnknown changes a record's price under commit-unknown.
// First commit: old entry (100, pk=1) cleared, new entry (200, pk=1) written.
// Retry: loadExisting sees price=200 (already committed), new record also price=200.
// removeCommonEntries should eliminate the unchanged (200, pk=1) entry.
// No uniqueness violation should occur.
func TestUniqueIndexOverwriteCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Initial save: price=100
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Overwrite to price=200 under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify() // Index entry should be (200, pk=1), old (100, pk=1) should be gone.
}

// TestUniqueIndexSwapPricesCommitUnknown tests a price swap scenario.
// Record A starts at price=100. We change A to price=200, then save B at price=100.
// With commit-unknown on each save, the retry logic must handle this correctly:
// no false violations, correct index entries.
func TestUniqueIndexSwapPricesCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save A with price=100
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Change A to price=200 (under commit-unknown)
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify() // price=100 entry gone, price=200 entry exists for pk=1

	// Save B with price=100 (now free, under commit-unknown)
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.Verify() // price=100 → pk=2, price=200 → pk=1
}

// TestUniqueIndexViolationDetection verifies that saving two records with the same
// price correctly returns a RecordIndexUniquenessViolationError.
// This is NOT a chaos test — it validates the constraint works at all.
func TestUniqueIndexViolationDetection(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save first record with price=100
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Try to save second record with same price — must fail
	err := s.TrySaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	if err == nil {
		t.Fatal("expected uniqueness violation error, got nil")
	}

	var violation *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("expected RecordIndexUniquenessViolationError, got %T: %v", err, err)
	}

	if violation.IndexName != "order_price_unique" {
		t.Fatalf("expected index name 'order_price_unique', got %q", violation.IndexName)
	}

	// Verify store state: only record 1 should exist with price=100
	s.Verify()
}

// TestUniqueIndexDeleteAndReuseValue deletes a record, then saves a new record
// with the same price value, under commit-unknown.
// Attack: delete record A (price=100), then save record B (price=100).
// Commit-unknown on delete: delete commits, retry sees record already gone → no-op.
// Commit-unknown on save: save commits, retry sees B exists → update (same PK) → OK.
func TestUniqueIndexDeleteAndReuseValue(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save A with price=100
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Delete A under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Record gone, index entry gone

	// Save B with the now-free price=100 under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.Verify() // price=100 → pk=2
}

// TestUniqueIndexRandomStress runs random operations with commit-unknown faults.
// All prices are kept unique (price = PK * 1000) to avoid intentional violations.
// Any RecordIndexUniquenessViolationError during this test is a bug — the unique
// constraint should never fire when all values are genuinely distinct.
func TestUniqueIndexRandomStress(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(12345), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// Save with unique price (price = pk * 1000 guarantees no collision
			// since each PK maps to exactly one price).
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(int32(pk * 1000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestUniqueIndexMultipleSavesCommitUnknown saves multiple records with distinct
// prices, each under commit-unknown, then verifies all index entries are correct.
func TestUniqueIndexMultipleSavesCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildUniqueIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Each save gets commit-unknown
	for i := int64(1); i <= 5; i++ {
		s.InjectOnce(FaultCommitUnknown)
		s.SaveRecord(&gen.Order{
			OrderId: proto.Int64(i),
			Price:   proto.Int32(int32(i * 100)),
		})
	}
	s.Verify() // 5 records, 5 unique index entries
}
