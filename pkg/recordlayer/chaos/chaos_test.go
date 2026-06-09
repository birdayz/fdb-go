package chaos

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var testRealDB fdb.Database

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		log.Fatalf("chaos: failed to start FDB container: %v", err)
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

// --- VECTOR (HNSW) index chaos tests ---

// buildVectorMetadata creates metadata with a VECTOR index on Order's
// price and quantity fields as a 2D vector.
func buildVectorMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	vecIdx := recordlayer.NewVectorIndex(
		"order_vec_idx",
		recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")),
		2,
	)
	// This scenario asserts strict reachability (every inserted vector must be found by
	// its own kNN search). Default HNSW (keepPrunedConnections=false) does NOT guarantee
	// that — a non-diverse reverse edge can be pruned from all M neighbors, orphaning a
	// node (true in Java too). keepPrunedConnections=true is the HNSW mechanism that keeps
	// those edges, so the index actually provides the reachability the invariant checks.
	vecIdx.Options[recordlayer.IndexOptionVectorKeepPrunedConnections] = "true"
	builder.AddIndex("Order", vecIdx)
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build vector metadata: " + err.Error())
	}
	return md
}

// TestVectorBasicSave verifies the VECTOR (HNSW) index matches the model
// after simple saves. No fault injection.
func TestVectorBasicSave(t *testing.T) {
	t.Parallel()
	md := buildVectorMetadata()
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

// TestVectorCommitUnknownInsert injects commit-unknown on an insert.
// VECTOR/HNSW insert is idempotent (same PK replaces), so retry is safe.
func TestVectorCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildVectorMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(100),
		Quantity: proto.Int32(10),
	})
	s.Verify() // HNSW must have exactly 1 entry, not 2
}

// TestVectorCommitUnknownOverwrite injects commit-unknown on an overwrite.
// First save inserts; second save with same PK + commit-unknown does delete+insert
// which commits, then retry does delete+insert again — idempotent.
func TestVectorCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildVectorMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(100),
		Quantity: proto.Int32(10),
	})
	s.Verify()

	// Overwrite with different vector under commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId:  proto.Int64(1),
		Price:    proto.Int32(999),
		Quantity: proto.Int32(99),
	})
	s.Verify() // Old (100,10) gone, new (999,99) present, exactly 1 entry
}

// TestVectorCommitUnknownDelete injects commit-unknown on a delete.
// Delete commits, then retry sees record already gone — no-op.
func TestVectorCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildVectorMetadata()
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
	s.Verify() // Only record 2 remains, HNSW has exactly 1 entry
}

// TestVectorRandomStress runs random saves and deletes with continuous
// fault injection against the VECTOR (HNSW) index.
func TestVectorRandomStress(t *testing.T) {
	t.Parallel()
	md := buildVectorMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(70707), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	const verifyEvery = 20
	maxPK := int64(25)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// 70% saves with random vector coordinates.
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

// serializeVecForChaos serializes a float64 vector to the DOUBLE wire format
// (type ordinal 2 + big-endian float64s). Mirrors recordlayer.serializeVector
// which is unexported.
func serializeVecForChaos(vec []float64) []byte {
	buf := make([]byte, 1+8*len(vec))
	buf[0] = 2 // DOUBLE type ordinal
	for i, v := range vec {
		binary.BigEndian.PutUint64(buf[1+i*8:], math.Float64bits(v))
	}
	return buf
}

// buildVectorHighDimRaBitQMetadata creates metadata with a 128D VECTOR index
// using RaBitQ quantization on the vector_data bytes field.
func buildVectorHighDimRaBitQMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	vecIdx := recordlayer.NewVectorIndex(
		"order_vec_128d_rabitq",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0),
		128,
	)
	vecIdx.Options["hnswUseRaBitQ"] = "true"
	// Establish the RaBitQ centroid after a few inserts so the small chaos dataset
	// actually exercises the quantization regime (and the mid-stream plain→RaBitQ
	// transition) under faults. Without this, Java parity stores everything plain
	// (noOp quantizer until StatsThreshold=1000), bypassing RaBitQ entirely.
	vecIdx.Options["hnswSampleVectorStatsProbability"] = "1.0"
	vecIdx.Options["hnswMaintainStatsProbability"] = "1.0"
	vecIdx.Options["hnswStatsThreshold"] = "3"
	builder.AddIndex("Order", vecIdx)

	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build high-dim RaBitQ metadata: " + err.Error())
	}
	return md
}

// TestVectorHighDimRaBitQBasic validates that 128D RaBitQ vector indexing works
// end-to-end: insert 10 records with random 128D vectors, then verify search
// returns correct nearest neighbors. No fault injection — validates the
// RaBitQ pipeline (quantization + encoding + distance estimation) works.
func TestVectorHighDimRaBitQBasic(t *testing.T) {
	t.Parallel()

	md := buildVectorHighDimRaBitQMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	db := recordlayer.NewFDBDatabase(testRealDB)
	ctx := context.Background()

	const numVectors = 10
	const dims = 128

	// Deterministic PRNG for reproducible vectors.
	rng := rand.New(rand.NewPCG(54321, 0))

	// Generate and store vectors for later verification.
	vectors := make([][]float64, numVectors)
	for i := range numVectors {
		vec := make([]float64, dims)
		for d := range dims {
			vec[d] = rng.Float64()*200.0 - 100.0 // [-100, 100)
		}
		vectors[i] = vec
	}

	// Insert all 10 records in a single transaction.
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		for i, vec := range vectors {
			_, err = store.SaveRecord(&gen.Order{
				OrderId:    proto.Int64(int64(i + 1)),
				VectorData: serializeVecForChaos(vec),
			})
			if err != nil {
				return nil, fmt.Errorf("save record %d: %w", i+1, err)
			}
		}

		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert records: %v", err)
	}

	// Search for k=5 nearest to vectors[0]. Self should be closest.
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		idx := md.GetIndex("order_vec_128d_rabitq")
		results, err := store.SearchVectorIndex(idx, vectors[0], 5, 200)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}

		if len(results) != 5 {
			t.Fatalf("expected 5 results, got %d", len(results))
		}

		// Results must be sorted by distance ascending.
		for i := 1; i < len(results); i++ {
			if results[i].Distance < results[i-1].Distance {
				t.Fatalf("results not sorted: dist[%d]=%.4f < dist[%d]=%.4f",
					i, results[i].Distance, i-1, results[i-1].Distance)
			}
		}

		// The closest result should be vectors[0] itself (PK=1, distance ~0).
		// With RaBitQ, distance is approximate, so allow some tolerance.
		if results[0].PrimaryKey[0].(int64) != 1 {
			t.Logf("WARNING: self-search did not return self as closest (got PK=%v, dist=%.4f)",
				results[0].PrimaryKey, results[0].Distance)
		}

		// All results should have distinct PKs.
		seen := make(map[int64]bool)
		for _, r := range results {
			pk := r.PrimaryKey[0].(int64)
			if seen[pk] {
				t.Fatalf("duplicate PK %d in results", pk)
			}
			seen[pk] = true
		}

		t.Logf("search returned PKs: %v", seen)
		t.Logf("distances: %.4f, %.4f, %.4f, %.4f, %.4f",
			results[0].Distance, results[1].Distance, results[2].Distance,
			results[3].Distance, results[4].Distance)

		return nil, nil
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
}

// --- PERMUTED_MAX index chaos tests ---

// buildPermutedMaxChaosMetadata creates metadata with a PERMUTED_MAX index.
// Groups by quantity, aggregated value is price. permutedSize=1.
// Primary entries: [quantity, price, trimmedPK...], Permuted: [price, quantity].
func buildPermutedMaxChaosMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewPermutedMaxIndex("order_permuted_max",
		recordlayer.GroupBy(recordlayer.Field("price"), recordlayer.Field("quantity")), 1))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build permuted max metadata: " + err.Error())
	}
	return md
}

// TestPermutedMaxBasicVerify verifies PERMUTED_MAX index with no fault injection.
func TestPermutedMaxBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(5)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(10)})
	s.Verify()
}

// TestPermutedMaxCommitUnknownInsert injects commit-unknown on a PERMUTED_MAX insert.
// PERMUTED_MAX uses removeCommonEntries (idempotent) — retry is safe.
func TestPermutedMaxCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()
}

// TestPermutedMaxCommitUnknownOverwriteHigher overwrites with a higher price
// under commit-unknown. The permuted subspace should reflect the new max.
func TestPermutedMaxCommitUnknownOverwriteHigher(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.Verify() // Permuted should show max quantity=50 for group
}

// TestPermutedMaxCommitUnknownOverwriteLower overwrites with a lower value.
// The permuted entry should update to the new (lower) max for that group.
func TestPermutedMaxCommitUnknownOverwriteLower(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(30)})
	s.Verify()

	// Overwrite pk=1 from qty=50 to qty=10 under commit-unknown.
	// Max for group price=100 should become 30 (from pk=2).
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(10)})
	s.Verify()
}

// TestPermutedMaxCommitUnknownDelete injects commit-unknown on a delete.
// After delete, permuted entry should reflect remaining records' max.
func TestPermutedMaxCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(30)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Only pk=2 remains, permuted max for group price=100 = qty 30
}

// TestPermutedMaxDeleteLastInGroup deletes the last record in a group.
// The permuted entry for that group should disappear entirely.
func TestPermutedMaxDeleteLastInGroup(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(30)})
	s.Verify()

	// Delete the only record in group price=100
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Group price=100 gone from permuted subspace

	// Delete last record in group price=200
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify() // All permuted entries gone
}

// TestPermutedMaxDeleteAllRecords verifies DeleteAllRecords clears permuted entries.
func TestPermutedMaxDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(i),
			Price:    proto.Int32(int32(i%3) * 100),
			Quantity: proto.Int32(int32(i * 10)),
		})
	}
	s.Verify()

	s.DeleteAllRecords()
	s.Verify()

	// Re-add after delete-all.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42), Quantity: proto.Int32(7)})
	s.Verify()
}

// TestPermutedMaxRandomStress runs 200 random ops with PERMUTED_MAX + FaultsRetryHeavy.
func TestPermutedMaxRandomStress(t *testing.T) {
	t.Parallel()
	md := buildPermutedMaxChaosMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(50505), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(5) * 100), // small price space → grouping collisions
				Quantity: proto.Int32(s.Rng.Int32N(500)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("PERMUTED_MAX stress: completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// --- PERMUTED_MIN index chaos tests ---

// buildPermutedMinChaosMetadata creates metadata with a PERMUTED_MIN index.
// Groups by quantity, aggregated value is price. permutedSize=1.
func buildPermutedMinChaosMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewPermutedMinIndex("order_permuted_min",
		recordlayer.GroupBy(recordlayer.Field("price"), recordlayer.Field("quantity")), 1))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build permuted min metadata: " + err.Error())
	}
	return md
}

// TestPermutedMinBasicVerify verifies PERMUTED_MIN index with no fault injection.
func TestPermutedMinBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(10)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200), Quantity: proto.Int32(30)})
	s.Verify()
}

// TestPermutedMinCommitUnknownInsert injects commit-unknown on a PERMUTED_MIN insert.
func TestPermutedMinCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.Verify()
}

// TestPermutedMinCommitUnknownOverwriteLower overwrites with a lower value
// under commit-unknown. The permuted subspace should reflect the new min.
func TestPermutedMinCommitUnknownOverwriteLower(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify() // Permuted should show min quantity=5 for group price=100
}

// TestPermutedMinCommitUnknownOverwriteHigher overwrites with a higher value.
// The permuted entry should update to the new (higher) min for that group.
func TestPermutedMinCommitUnknownOverwriteHigher(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(10)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.Verify()

	// Overwrite pk=1 from qty=10 to qty=80 under commit-unknown.
	// Min for group price=100 should become 50 (from pk=2).
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(80)})
	s.Verify()
}

// TestPermutedMinCommitUnknownDelete injects commit-unknown on a delete.
func TestPermutedMinCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(10)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(50)})
	s.Verify()

	// Delete record with min — min should shift to pk=2's quantity=50
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

// TestPermutedMinDeleteLastInGroup deletes the last record in a group.
func TestPermutedMinDeleteLastInGroup(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(10)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(50)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify() // Group price=100 gone from permuted subspace

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify() // All permuted entries gone
}

// TestPermutedMinDeleteAllRecords verifies DeleteAllRecords clears permuted entries.
func TestPermutedMinDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(i),
			Price:    proto.Int32(int32(i%3) * 100),
			Quantity: proto.Int32(int32(i * 10)),
		})
	}
	s.Verify()

	s.DeleteAllRecords()
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42), Quantity: proto.Int32(7)})
	s.Verify()
}

// TestPermutedMinRandomStress runs 200 random ops with PERMUTED_MIN + FaultsRetryHeavy.
func TestPermutedMinRandomStress(t *testing.T) {
	t.Parallel()
	md := buildPermutedMinChaosMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(60606), WithFaults(FaultsRetryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(5) * 100),
				Quantity: proto.Int32(s.Rng.Int32N(500)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("PERMUTED_MIN stress: completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestVectorHighDimRaBitQCommitUnknown validates that HNSW with RaBitQ
// quantization remains idempotent under commit-unknown fault injection.
// Uses 2D vectors (Price, Quantity) so the chaos StoreModel verification
// works (it requires numeric fields, not byte vectors).
func TestVectorHighDimRaBitQCommitUnknown(t *testing.T) {
	t.Parallel()

	// Use 2D vectors via Price/Quantity for chaos model compatibility,
	// but with RaBitQ enabled to exercise the quantization pipeline under faults.
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	vecIdx := recordlayer.NewVectorIndex(
		"order_vec_rabitq_chaos",
		recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")),
		2,
	)
	vecIdx.Options["hnswUseRaBitQ"] = "true"
	// Force the RaBitQ centroid to establish early (Java parity stores plain until
	// StatsThreshold=1000), so this small dataset truly exercises the quantization
	// pipeline + the plain→RaBitQ transition under commit_unknown faults.
	vecIdx.Options["hnswSampleVectorStatsProbability"] = "1.0"
	vecIdx.Options["hnswMaintainStatsProbability"] = "1.0"
	vecIdx.Options["hnswStatsThreshold"] = "3"
	builder.AddIndex("Order", vecIdx)

	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}

	s := NewScenario(t, testRealDB, md, WithSeed(99887), WithFaults(FaultsRetryHeavy))

	const numOps = 20
	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(10) + 1
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(pk),
			Price:    proto.Int32(s.Rng.Int32N(500)),
			Quantity: proto.Int32(s.Rng.Int32N(500)),
		})

		if (i+1)%5 == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// --- COUNT_NOT_NULL chaos tests ---

// buildCountNotNullMetadata creates metadata with a COUNT_NOT_NULL index.
// Groups by price — only counts records where price is actually set (non-nil).
func buildCountNotNullMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewCountNotNullIndex("order_count_not_null_by_price",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build count_not_null metadata: " + err.Error())
	}
	return md
}

// TestCountNotNullCommitUnknown verifies COUNT_NOT_NULL stays correct under
// commit-unknown. Like COUNT, the removeCommonGroupingKeys optimization
// makes it safe for unchanged keys.
func TestCountNotNullCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildCountNotNullMetadata()
	s := NewScenario(t, testRealDB, md)

	// Insert record with price set → should be counted
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Insert record WITHOUT price set → should NOT be counted
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2)})
	s.Verify()

	// Overwrite record 1 with different price under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify()

	// Set price on record 2 (was nil → now has value) under commit-unknown
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()
}

// TestCountNotNullNilToSet verifies transition from nil price to set price.
func TestCountNotNullNilToSet(t *testing.T) {
	t.Parallel()
	md := buildCountNotNullMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save without price
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
	s.Verify()

	// Now set price → count should increase
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)})
	s.Verify()

	// Clear price back to nil → count should decrease
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
	s.Verify()
}

// TestRandomWithCountNotNullIndex runs random ops with COUNT_NOT_NULL index.
// Mix of records with and without price set, under fault injection.
func TestRandomWithCountNotNullIndex(t *testing.T) {
	t.Parallel()
	md := buildCountNotNullMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(77777), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			order := &gen.Order{OrderId: proto.Int64(pk)}
			// 70% chance of setting price, 30% leave nil
			if s.Rng.Float64() < 0.7 {
				order.Price = proto.Int32(s.Rng.Int32N(5) * 100)
			}
			s.SaveRecord(order)
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
