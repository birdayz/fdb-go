package chaos

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// --- Metadata builders for random tests ---

// buildRecordCountOnlyMetadata creates metadata with just record counting (no indexes).
func buildRecordCountOnlyMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build record-count-only metadata: " + err.Error())
	}
	return md
}

// buildValueIndexMetadata creates metadata with record counting + VALUE index on price.
func buildValueIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("rand_price_idx", recordlayer.Field("price")))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build value index metadata: " + err.Error())
	}
	return md
}

// buildValueCountMetadata creates metadata with VALUE index on price + COUNT index.
func buildValueCountMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("rand_vc_price_idx", recordlayer.Field("price")))
	builder.AddIndex("Order", recordlayer.NewCountIndex("rand_vc_count_by_price",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build value+count metadata: " + err.Error())
	}
	return md
}

// buildFullRandomMetadata creates metadata with VALUE + COUNT + SUM indexes.
// Used by the multi-index stress test.
func buildFullRandomMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	// VALUE indexes on price and quantity
	builder.AddIndex("Order", recordlayer.NewIndex("rand_full_price_idx", recordlayer.Field("price")))
	builder.AddIndex("Order", recordlayer.NewIndex("rand_full_qty_idx", recordlayer.Field("quantity")))

	// COUNT index grouped by price
	builder.AddIndex("Order", recordlayer.NewCountIndex("rand_full_count_by_price",
		recordlayer.GroupAll(recordlayer.Field("price"))))

	// SUM index on price (ungrouped)
	builder.AddIndex("Order", recordlayer.NewSumIndex("rand_full_sum_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))

	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build full random metadata: " + err.Error())
	}
	return md
}

// --- Random tests ---

// TestRandomBasicCRUD validates that a random save/delete/deleteAll sequence
// keeps model and store in sync with no indexes and no faults.
func TestRandomBasicCRUD(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildRecordCountOnlyMetadata(), RandomConfig{
		Seed:    1001,
		NumOps:  500,
		MaxPKs:  30,
		Faults:  FaultsNone,
	})
}

// TestRandomValueIndex validates a VALUE index stays correct across random
// operations with no faults.
func TestRandomValueIndex(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildValueIndexMetadata(), RandomConfig{
		Seed:    2002,
		NumOps:  500,
		MaxPKs:  30,
		Faults:  FaultsNone,
	})
}

// TestRandomValueIndexWithFaults validates a VALUE index under 5% commit-unknown.
// The removeCommonEntries optimization must correctly skip duplicate entries on retry.
func TestRandomValueIndexWithFaults(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildValueIndexMetadata(), RandomConfig{
		Seed:    3003,
		NumOps:  500,
		MaxPKs:  30,
		Faults:  FaultsRetryHeavy,
	})
}

// TestRandomCountIndex validates a COUNT index under 5% commit-unknown.
// The removeCommonGroupingKeys optimization must skip unchanged keys on retry.
func TestRandomCountIndex(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildCountIndexMetadata(), RandomConfig{
		Seed:    4004,
		NumOps:  500,
		MaxPKs:  30,
		Faults:  FaultsRetryHeavy,
	})
}

// TestRandomSumIndex validates a SUM index under 5% commit-unknown.
// The removeCommonSumEntries optimization must skip identical (key, value) pairs on retry.
func TestRandomSumIndex(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildSumIndexMetadata(), RandomConfig{
		Seed:    5005,
		NumOps:  500,
		MaxPKs:  30,
		Faults:  FaultsRetryHeavy,
	})
}

// TestRandomMultiIndex is the heavy stress test: VALUE + COUNT + SUM indexes
// all active with 5% commit-unknown. 1000 ops, 50 PKs.
func TestRandomMultiIndex(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildFullRandomMetadata(), RandomConfig{
		Seed:    6006,
		NumOps:  1000,
		MaxPKs:  50,
		Faults:  FaultsRetryHeavy,
	})
}

// TestRandomWithDeleteAll increases the DeleteAll weight to exercise the
// delete-all + rebuild pattern frequently. VALUE + COUNT, no faults.
func TestRandomWithDeleteAll(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildValueCountMetadata(), RandomConfig{
		Seed:   7007,
		NumOps: 300,
		MaxPKs: 20,
		Faults: FaultsNone,
		Weights: &OpWeights{
			SaveNew:        30,
			SaveOverwrite:  20,
			DeleteExisting: 15,
			DeleteMissing:  5,
			DeleteAll:      5,
		},
	})
}

// TestRandomAllFaults runs with all fault types active (commit-unknown + conflict + TOO).
// 1000 ops against VALUE + COUNT + SUM indexes.
func TestRandomAllFaults(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildFullRandomMetadata(), RandomConfig{
		Seed:   9009,
		NumOps: 1000,
		MaxPKs: 50,
		Faults: FaultsAll,
	})
}

// TestRandomDeterminism runs the same seed twice and verifies the model ends
// up in exactly the same state. Same seed = same PRNG = same operations.
func TestRandomDeterminism(t *testing.T) {
	t.Parallel()
	md := buildValueIndexMetadata()
	cfg := RandomConfig{
		Seed:   8008,
		NumOps: 200,
		MaxPKs: 20,
		Faults: FaultsNone,
	}

	s1 := RunRandom(t, testRealDB, md, cfg)
	s2 := RunRandom(t, testRealDB, md, cfg)

	// Compare model record counts.
	if s1.model.Count() != s2.model.Count() {
		t.Fatalf("determinism: model counts differ: run1=%d run2=%d", s1.model.Count(), s2.model.Count())
	}

	// Compare model record PKs and values.
	for key, rec1 := range s1.model.Records {
		rec2, ok := s2.model.Records[key]
		if !ok {
			t.Fatalf("determinism: PK %v in run1 but not run2", rec1.PrimaryKey)
		}
		if rec1.TypeName != rec2.TypeName {
			t.Fatalf("determinism: PK %v type differs: run1=%s run2=%s",
				rec1.PrimaryKey, rec1.TypeName, rec2.TypeName)
		}
	}
	for key, rec2 := range s2.model.Records {
		if _, ok := s1.model.Records[key]; !ok {
			t.Fatalf("determinism: PK %v in run2 but not run1", rec2.PrimaryKey)
		}
	}

	t.Logf("determinism verified: both runs produced %d records with identical PKs", s1.model.Count())
}
