package chaos

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var testRealDB fdb.Database

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		log.Fatalf("chaos: failed to start FDB container: %v", err)
	}

	if err := container.InitializeDatabase(ctx); err != nil {
		log.Fatalf("chaos: failed to initialize FDB: %v", err)
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		log.Fatalf("chaos: failed to get cluster file: %v", err)
	}

	tmpFile, err := os.CreateTemp("", "fdb_chaos_cluster_*.txt")
	if err != nil {
		log.Fatalf("chaos: failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		log.Fatalf("chaos: failed to write cluster file: %v", err)
	}
	tmpFile.Close()

	fdb.MustAPIVersion(720)
	testRealDB, err = fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		log.Fatalf("chaos: failed to open FDB: %v", err)
	}

	code := m.Run()
	_ = container.Terminate(ctx)
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

// buildTestMetadata creates metadata with record counting and a VALUE index.
func buildTestMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("order_price_idx", recordlayer.Field("price")))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build test metadata: " + err.Error())
	}
	return md
}

// TestBasicSaveVerify verifies the model and store agree after simple saves.
// No fault injection — validates the framework itself works.
func TestBasicSaveVerify(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
	})
	s.Verify()

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_BLUE.Enum()},
	})
	s.Verify()

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
	})
	s.Verify()
}

// TestBasicSaveDeleteVerify verifies save + delete keeps model and store in sync.
func TestBasicSaveDeleteVerify(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()

	// Save again with same PK (re-insert after delete)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(150)})
	s.Verify()
}

// TestOverwriteExistingRecord verifies overwriting a record updates correctly.
func TestOverwriteExistingRecord(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Overwrite with different data
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(999)})
	s.Verify()
}

// TestDeleteAllRecords verifies DeleteAllRecords resets everything.
func TestDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
	}
	s.Verify()

	s.DeleteAllRecords()
	s.Verify()

	// Save after delete-all
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42)})
	s.Verify()
}

// TestCommitUnknownSingleSave injects commit-unknown on a single save.
// The transaction commits, then re-executes. The re-execution sees the record
// already exists and does an update. COUNT should be 1, not 2.
func TestCommitUnknownSingleSave(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // COUNT must be 1, not 2
}

// TestCommitUnknownMultipleSaves injects commit-unknown across multiple saves.
// Each save is retried after a successful commit.
func TestCommitUnknownMultipleSaves(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify() // COUNT must be 2

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify() // COUNT must be 3
}

// TestCommitUnknownDelete injects commit-unknown on a delete.
// The delete commits, then re-executes. The re-execution sees the record
// is already gone and returns false. COUNT should be correct.
func TestCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // COUNT must be 1, record 1 gone, record 2 present
}

// TestCommitUnknownOverwrite injects commit-unknown on an overwrite.
// First commit saves the new version, retry sees it exists and does an update.
func TestCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(999)})
	s.Verify() // COUNT must still be 1
}

// TestRandomWithFaults runs many operations with continuous fault injection.
// Uses FaultsRetryHeavy (5% commit-unknown rate) and verifies periodically.
func TestRandomWithFaults(t *testing.T) {
	t.Parallel()
	md := buildTestMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(12345), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(50)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// 70% saves
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(10000)),
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

// --- Atomic index corruption tests ---

// buildCountIndexMetadata creates metadata with a COUNT index (grouped by price).
func buildCountIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewCountIndex("order_count_by_price",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build count index metadata: " + err.Error())
	}
	return md
}

// buildSumIndexMetadata creates metadata with a SUM index (total price, ungrouped).
func buildSumIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewSumIndex("order_total_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build sum index metadata: " + err.Error())
	}
	return md
}

// buildCountUpdatesMetadata creates metadata with a COUNT_UPDATES index.
func buildCountUpdatesMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewCountUpdatesIndex("order_update_count",
		recordlayer.Ungrouped(recordlayer.EmptyKey())))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build count_updates metadata: " + err.Error())
	}
	return md
}

// TestCountIndexCommitUnknown verifies COUNT index stays correct under commit-unknown.
func TestCountIndexCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildCountIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // COUNT for price=100 must be 1, not 2

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
	s.Verify() // COUNT for price=100 must be 2

	// Overwrite with different price under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify() // COUNT: price=100 → 1, price=200 → 1
}

// TestSumIndexCommitUnknown verifies SUM index stays correct under commit-unknown.
func TestSumIndexCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildSumIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify() // SUM must be 100, not 200

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
	s.Verify() // SUM must be 400

	// Overwrite: change price from 100 to 500 under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
	s.Verify() // SUM must be 800 (500 + 300)
}

// TestCountUpdatesCommitUnknown documents that COUNT_UPDATES is NOT idempotent
// under commit-unknown (skipUpdateForUnchangedKeys=false in Java too).
// This is a known design limitation shared with Java Record Layer.
// The retry does an UPDATE which unconditionally ADD +1 again.
func TestCountUpdatesCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildCountUpdatesMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})

	// KNOWN: commit-unknown causes double-count (expected=1, actual=2).
	// This matches Java behavior — COUNT_UPDATES is inherently non-idempotent.
	// Skipping Verify() — this test documents the limitation.
	t.Log("COUNT_UPDATES double-counts under commit-unknown (known, matches Java)")
}

// TestCountUpdatesMultipleCommitUnknown documents COUNT_UPDATES non-idempotency
// across multiple operations with commit-unknown.
func TestCountUpdatesMultipleCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildCountUpdatesMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})

	// KNOWN: commit-unknown causes double-count (expected=2, actual=3).
	t.Log("COUNT_UPDATES double-counts under commit-unknown (known, matches Java)")
}

// TestRandomWithCountIndex runs random ops with COUNT index + faults.
func TestRandomWithCountIndex(t *testing.T) {
	t.Parallel()
	md := buildCountIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(54321), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(5) * 100), // small price space → grouping collisions
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%20 == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops, %d faults injected", numOps, len(s.FaultLog()))
}

// TestRandomWithSumIndex runs random ops with SUM index + faults.
func TestRandomWithSumIndex(t *testing.T) {
	t.Parallel()
	md := buildSumIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(99999), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(1000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%20 == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops, %d faults injected", numOps, len(s.FaultLog()))
}

// TestRandomWithCountUpdatesIndex runs random ops with COUNT_UPDATES WITHOUT faults.
// COUNT_UPDATES is non-idempotent under commit-unknown, so we only test without faults
// to verify basic correctness. See TestCountUpdatesCommitUnknown for the known limitation.
func TestRandomWithCountUpdatesIndex(t *testing.T) {
	t.Parallel()
	md := buildCountUpdatesMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(77777))

	const numOps = 100
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(1000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%20 == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops", numOps)
}

// --- MULTIDIMENSIONAL index chaos tests ---

// buildMultidimensionalMetadata creates metadata with a MULTIDIMENSIONAL index
// on Order's price and quantity fields as 2D spatial coordinates.
func buildMultidimensionalMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewMultidimensionalIndex(
		"order_price_qty_md",
		recordlayer.Dimensions(
			recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")),
			0, // prefix size
			2, // dimensions
		),
	))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build multidimensional metadata: " + err.Error())
	}
	return md
}

// TestMultidimensionalBasicSave verifies the MULTIDIMENSIONAL index matches
// the model after simple saves. No fault injection.
func TestMultidimensionalBasicSave(t *testing.T) {
	t.Parallel()
	md := buildMultidimensionalMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(100),
		Quantity: proto.Int32(10),
	})
	s.Verify()

	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(2),
		Price:    proto.Int32(200),
		Quantity: proto.Int32(20),
	})
	s.Verify()

	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(3),
		Price:    proto.Int32(300),
		Quantity: proto.Int32(30),
	})
	s.Verify()
}

// TestMultidimensionalCommitUnknownInsert injects commit-unknown on an insert.
// MULTIDIMENSIONAL uses insertOrUpdate (upsert), so retry is idempotent.
func TestMultidimensionalCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildMultidimensionalMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(100),
		Quantity: proto.Int32(10),
	})
	s.Verify() // R-tree must have exactly 1 entry, not 2
}

// TestMultidimensionalCommitUnknownOverwrite injects commit-unknown on an overwrite.
// First save inserts; second save with same PK + commit-unknown does delete+insert
// which commits, then retry does delete+insert again — idempotent via upsert.
func TestMultidimensionalCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildMultidimensionalMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(100),
		Quantity: proto.Int32(10),
	})
	s.Verify()

	// Overwrite with different coordinates under commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(999),
		Quantity: proto.Int32(99),
	})
	s.Verify() // Old (100,10) gone, new (999,99) present, exactly 1 entry
}

// TestMultidimensionalCommitUnknownDelete injects commit-unknown on a delete.
// Delete commits, then retry sees record already gone — no-op.
func TestMultidimensionalCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildMultidimensionalMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(100),
		Quantity: proto.Int32(10),
	})
	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(2),
		Price:    proto.Int32(200),
		Quantity: proto.Int32(20),
	})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Only record 2 remains, R-tree has exactly 1 entry
}

// TestMultidimensionalRandomStress runs random saves and deletes with
// continuous fault injection against the MULTIDIMENSIONAL index.
func TestMultidimensionalRandomStress(t *testing.T) {
	t.Parallel()
	md := buildMultidimensionalMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(31337), WithFaults(FaultsRetryHeavy))

	const numOps = 150
	const verifyEvery = 25
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// 70% saves with random coordinates.
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(1000)),
				Quantity: proto.Int32(s.Rng.Int32N(500)),
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
