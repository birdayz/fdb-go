package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildRankIndexMetadata creates metadata with a RANK index on Order.price.
// No grouping — the ranked set contains all prices globally.
func buildRankIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewRankIndex("order_price_rank",
		recordlayer.Field("price")))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build rank index metadata: " + err.Error())
	}
	return md
}

// TestRankIndexBasicVerify verifies the RANK index with no fault injection.
// Validates the verification framework itself works for RANK.
func TestRankIndexBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()

	// Delete one and re-verify.
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify()

	// Re-insert with different price.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)})
	s.Verify()
}

// TestRankIndexCommitUnknown injects commit-unknown on a single RANK index save.
// The B-tree uses removeCommonEntries (idempotent). The ranked set Add should be
// idempotent for !CountDuplicates (default).
func TestRankIndexCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // B-tree: 1 entry. Ranked set: score=100, rank=0.

	// Second save, also with fault.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify() // B-tree: 2 entries. Ranked set: 100→rank 0, 200→rank 1.

	// Third save, no fault.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50)})
	s.Verify() // Ranked set: 50→0, 100→1, 200→2.
}

// TestRankIndexCommitUnknownOverwrite injects commit-unknown on a record overwrite
// that changes the indexed price. This is the most dangerous scenario for RANK:
// the first commit removes old score, adds new score. The retry must be a no-op.
func TestRankIndexCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	// Overwrite pk=1: price 100→500 with commit-unknown.
	// First commit: remove score=100 from ranked set, add score=500.
	// Retry: old={pk=1, price=500}, new={pk=1, price=500} → removeCommonEntries filters.
	// No ranked set operations on retry.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
	s.Verify() // Ranked set: 200→0, 500→1.
}

// TestRankIndexCommitUnknownDelete injects commit-unknown on a delete.
func TestRankIndexCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()

	// Delete pk=2 with commit-unknown.
	// First commit: clear B-tree (200, pk=2), remove score=200 from ranked set.
	// Retry: old=nil (already deleted), no entries to process.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify() // Ranked set: 100→0, 300→1.

	// Delete another with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Ranked set: 300→0.
}

// TestRankIndexDuplicateScores tests multiple records with the same score.
// With !CountDuplicates (default), the ranked set has one entry per distinct score.
// The B-tree has one entry per record.
func TestRankIndexDuplicateScores(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Three records, all price=100.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
	s.Verify() // B-tree: 3 entries. Ranked set: 1 distinct score (100→rank 0).

	// Delete one — score should remain in ranked set (two more records have it).
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify() // B-tree: 2 entries. Ranked set: 100→rank 0.

	// Delete another — score should remain.
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // B-tree: 1 entry. Ranked set: 100→rank 0.

	// Delete last — score should be removed from ranked set.
	s.DeleteRecord(tuple.Tuple{int64(3)})
	s.Verify() // Everything empty.
}

// TestRankIndexDuplicateScoresCommitUnknown tests duplicate scores + commit-unknown.
// This is the key scenario: the "only remove from ranked set when LAST B-tree entry
// cleared" logic must be idempotent under retry.
func TestRankIndexDuplicateScoresCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Setup: three records with same score.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
	s.Verify()

	// Save a new duplicate score with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(100)})
	s.Verify() // B-tree: 4 entries. Ranked set: 100→0, 200→1.

	// Delete one of the duplicates with commit-unknown.
	// First commit: clear B-tree (100, pk=1). Check remaining: pk=2 and pk=4 still have score 100 → don't remove from ranked set.
	// Retry: pk=1 already gone → no old entries → no ranked set ops.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // B-tree: 3. Ranked set: 100→0, 200→1.

	// Overwrite pk=2 from 100→300 with commit-unknown.
	// First commit: remove B-tree (100, pk=2), add (300, pk=2). Ranked set: check remaining 100 entries (pk=4 still there) → don't remove 100, add 300.
	// Retry: old={pk=2, price=300}, new={pk=2, price=300} → removeCommonEntries filters. No-op.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
	s.Verify() // Ranked set: 100→0, 200→1, 300→2.

	// Delete the LAST record with score 100 (pk=4) under commit-unknown.
	// This is the most interesting case: the first commit removes the B-tree entry
	// AND removes score=100 from the ranked set. The retry sees pk=4 is gone → no-op.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(4)})
	s.Verify() // Ranked set: 200→0, 300→1.
}

// TestRankIndexOverwriteChangesRank verifies that overwriting a record with a
// different price correctly updates both B-tree and ranked set.
func TestRankIndexOverwriteChangesRank(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()

	// Move pk=1 from lowest (rank 0) to highest (rank 2).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(400)})
	s.Verify() // Ranked set: 200→0, 300→1, 400→2.

	// Move pk=2 down to lowest.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)})
	s.Verify() // Ranked set: 50→0, 300→1, 400→2.
}

// TestRankIndexDeleteAllRecords verifies DeleteAllRecords clears both B-tree and ranked set.
func TestRankIndexDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
	}
	s.Verify()

	s.DeleteAllRecords()
	s.Verify()

	// Re-add after delete-all.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42)})
	s.Verify()
}

// --- CountDuplicates=true tests ---
// With CountDuplicates, the ranked set uses atomic ADD (not idempotent).
// This should reveal corruption under commit-unknown.

// buildRankIndexCountDuplicatesMetadata creates metadata with a RANK index
// that has CountDuplicates=true. This means duplicate scores increment the
// ranked set count, and ranks account for each duplicate individually.
func buildRankIndexCountDuplicatesMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	idx := recordlayer.NewRankIndex("order_price_rank_cd",
		recordlayer.Field("price"))
	idx.Options[recordlayer.IndexOptionRankCountDuplicates] = "true"
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build rank count-duplicates metadata: " + err.Error())
	}
	return md
}

// TestRankIndexCountDuplicatesBasic verifies CountDuplicates=true without faults.
func TestRankIndexCountDuplicatesBasic(t *testing.T) {
	t.Parallel()
	md := buildRankIndexCountDuplicatesMetadata()
	s := NewScenario(t, testRealDB, md)

	// Three records with same score — each gets its own rank.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
	s.Verify()

	// Delete one duplicate.
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

// TestRankIndexCountDuplicatesCommitUnknown tests CountDuplicates=true with
// commit-unknown. With atomic ADD for duplicate counts, the retry may cause
// double-counting in the ranked set.
func TestRankIndexCountDuplicatesCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildRankIndexCountDuplicatesMetadata()
	s := NewScenario(t, testRealDB, md)

	// Insert with commit-unknown. The ranked set uses tx.Add for duplicates,
	// but on first insert (no prior key), it uses tx.Set for level 0.
	// The retry should see the B-tree entry already exists (removeCommonEntries
	// filters it out) so no ranked set operation on retry.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Second record with same score and commit-unknown.
	// First commit: B-tree entry (100, pk=2) set. Ranked set: key exists, duplicate=true,
	// tx.Add(level0, +1) → count becomes 2. Higher levels: tx.Add(prevKey, +1).
	// Retry: old={pk=2, price=100}, new={pk=2, price=100} → removeCommonEntries filters.
	// No ranked set operations on retry. Should be safe.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.Verify()
}

// TestRankIndexCountDuplicatesDeleteCommitUnknown tests delete with
// CountDuplicates=true under commit-unknown.
func TestRankIndexCountDuplicatesDeleteCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildRankIndexCountDuplicatesMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
	s.Verify()

	// Delete one of the duplicates with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

// TestRankIndexCountDuplicatesRandomStress runs random ops with CountDuplicates=true
// and fault injection.
func TestRankIndexCountDuplicatesRandomStress(t *testing.T) {
	t.Parallel()
	md := buildRankIndexCountDuplicatesMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(54321), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(5) * 100), // small price space, lots of duplicates
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

// TestRankIndexRandomStress runs many random operations with continuous fault
// injection and periodic verification of both B-tree and ranked set.
func TestRankIndexRandomStress(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(31337), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// 70% saves with a small price space to force duplicate scores.
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(10) * 100), // 0-900, lots of duplicates
			})
		} else {
			// 30% deletes.
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

// TestRankIndexHeavyFaultStress runs with a very high fault injection rate
// to maximize the chance of catching non-idempotent behavior.
func TestRankIndexHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md := buildRankIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(99887), WithFaults(FaultsRetryVeryHeavy))

	const numOps = 300
	const verifyEvery = 10
	maxPK := int64(20) // smaller key space = more overwrites

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(5) * 100), // very small price space
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

// TestRankIndexCountDuplicatesHeavyFaultStress same but with CountDuplicates=true.
func TestRankIndexCountDuplicatesHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md := buildRankIndexCountDuplicatesMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(11223), WithFaults(FaultsRetryVeryHeavy))

	const numOps = 300
	const verifyEvery = 10
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(5) * 100),
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
