package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildFanOutMetadata creates metadata with a FanOut("tags") VALUE index on Order.
func buildFanOutMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("order_tags_idx", recordlayer.FanOut("tags")))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build fanout metadata: " + err.Error())
	}
	return md
}

// buildFanOutCrossProductMetadata creates metadata with Concat(FanOut("tags"), Field("price")).
// This produces a cross-product: one index entry per tag, each including the price.
func buildFanOutCrossProductMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("order_tags_price_idx",
		recordlayer.Concat(recordlayer.FanOut("tags"), recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build fanout cross-product metadata: " + err.Error())
	}
	return md
}

// TestFanOutBasicVerify saves records with tags, verifies index entries match model.
// No fault injection — validates the framework handles fan-out correctly.
func TestFanOutBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	// Single record with 3 tags → 3 index entries
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"alpha", "beta", "gamma"},
	})
	s.Verify()

	// Second record with overlapping tags → additional entries
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Tags:    []string{"beta", "delta"},
	})
	s.Verify()

	// Third record with single tag
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
		Tags:    []string{"epsilon"},
	})
	s.Verify()
}

// TestFanOutCommitUnknown injects commit-unknown on a save with tags.
// Under retry: first commit creates entries, retry sees identical entries,
// removeCommonEntries should skip all → no corruption.
func TestFanOutCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"alpha", "beta", "gamma"},
	})
	s.Verify() // Must have exactly 3 index entries, not 6
}

// TestFanOutChangeTagsCommitUnknown overwrites tags under commit-unknown.
// Old tags=["a","b"], new tags=["b","c"]. Expected: entry "a" removed, "c" added, "b" kept.
// Under commit-unknown retry: first commit does the mutation, retry sees new record
// with tags=["b","c"] as both old and new → removeCommonEntries skips all.
func TestFanOutChangeTagsCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	// First: save with tags ["a","b"]
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"a", "b"},
	})
	s.Verify()

	// Now overwrite with tags ["b","c"] under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"b", "c"},
	})
	s.Verify() // Must have entries for "b" and "c" only
}

// TestFanOutEmptyTags verifies empty repeated field produces no index entries.
func TestFanOutEmptyTags(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{}, // empty
	})
	s.Verify()

	// Also test nil tags (zero value)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
	})
	s.Verify()
}

// TestFanOutEmptyToNonEmptyCommitUnknown saves with empty tags, then overwrites
// to non-empty tags under commit-unknown. Tests the edge case where old record
// produces no index entries but new record produces entries.
func TestFanOutEmptyToNonEmptyCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	// Start with empty tags
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{},
	})
	s.Verify()

	// Overwrite to non-empty under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"x", "y", "z"},
	})
	s.Verify() // Must have exactly 3 entries

	// And the reverse: non-empty to empty under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{},
	})
	s.Verify() // Must have 0 entries
}

// TestFanOutNonEmptyToEmptyCommitUnknown saves with tags, then overwrites
// to empty tags under commit-unknown. Tests that entries are cleaned up correctly.
func TestFanOutNonEmptyToEmptyCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"alpha", "beta"},
	})
	s.Verify()

	// Clear all tags under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{},
	})
	s.Verify() // Must have 0 index entries for this record
}

// TestFanOutDeleteRecordCommitUnknown deletes a record with fan-out index
// entries under commit-unknown. All entries must be cleaned up.
func TestFanOutDeleteRecordCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"a", "b", "c"},
	})
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Tags:    []string{"b", "d"},
	})
	s.Verify()

	// Delete record 1 under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Only record 2's entries ("b","d") should remain
}

// TestFanOutDuplicateTags tests records with duplicate tags (e.g., tags=["a","a"]).
// Fan-out produces 2 entries for "a" but FDB deduplicates by key, so only 1 entry
// exists in the store. The model must handle this correctly.
func TestFanOutDuplicateTags(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"a", "a", "b"},
	})
	s.Verify() // FDB has 2 entries ("a" deduped), model evaluates to 3 entries
}

// TestFanOutCrossProduct tests Concat(FanOut("tags"), Field("price")) —
// cross-product of fan-out tags with a scalar field.
func TestFanOutCrossProduct(t *testing.T) {
	t.Parallel()
	md := buildFanOutCrossProductMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"x", "y"},
	})
	s.Verify() // 2 entries: (x,100) and (y,100)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Tags:    []string{"x", "z"},
	})
	s.Verify() // 4 entries total

	// Overwrite record 1 with different price
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(999),
		Tags:    []string{"x", "y"},
	})
	s.Verify() // (x,100) and (y,100) replaced with (x,999) and (y,999)
}

// TestFanOutCrossProductCommitUnknown tests cross-product under commit-unknown.
func TestFanOutCrossProductCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildFanOutCrossProductMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Tags:    []string{"x", "y", "z"},
	})
	s.Verify() // 3 entries under retry

	// Update both tags and price under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(200),
		Tags:    []string{"y", "z", "w"},
	})
	s.Verify() // Old: (x,100),(y,100),(z,100) → New: (y,200),(z,200),(w,200)
}

// TestFanOutRandomStress runs 200 random operations with random tag sets and
// continuous fault injection (5% commit-unknown rate).
func TestFanOutRandomStress(t *testing.T) {
	t.Parallel()
	md := buildFanOutMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(31337), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	allTags := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1

		if s.Rng.Float64() < 0.7 {
			// 70% saves with random tags
			numTags := s.Rng.IntN(5) // 0 to 4 tags
			tags := make([]string, numTags)
			for j := range tags {
				tags[j] = allTags[s.Rng.IntN(len(allTags))]
			}
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(1000)),
				Tags:    tags,
			})
		} else {
			// 30% deletes
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

// TestFanOutCrossProductRandomStress runs random ops with cross-product index + faults.
func TestFanOutCrossProductRandomStress(t *testing.T) {
	t.Parallel()
	md := buildFanOutCrossProductMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(42424), WithFaults(FaultsRetryHeavy))

	const numOps = 150
	const verifyEvery = 25
	maxPK := int64(20)

	allTags := []string{"red", "green", "blue", "yellow"}

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1

		if s.Rng.Float64() < 0.7 {
			numTags := s.Rng.IntN(4) // 0 to 3 tags
			tags := make([]string, numTags)
			for j := range tags {
				tags[j] = allTags[s.Rng.IntN(len(allTags))]
			}
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(500)),
				Tags:    tags,
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
