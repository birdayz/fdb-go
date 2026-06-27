package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildVersionIndexMetadata creates metadata with a VERSION index.
func buildVersionIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", recordlayer.NewVersionIndex("order_version_idx",
		recordlayer.VersionKey()))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build version index metadata: " + err.Error())
	}
	return md
}

// buildCompositeVersionIndexMetadata creates metadata with a composite VERSION index
// that includes both version and a field (price).
func buildCompositeVersionIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", recordlayer.NewVersionIndex("order_version_price_idx",
		recordlayer.Concat(recordlayer.VersionKey(), recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build composite version index metadata: " + err.Error())
	}
	return md
}

// --- Basic verification (no faults) ---

func TestVersionIndexBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
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
	})
	s.Verify()

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
	})
	s.Verify()
}

func TestVersionIndexOverwriteVerify(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Overwrite same PK with different data — version index entry should update.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify()

	// Overwrite again.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(300)})
	s.Verify()
}

func TestVersionIndexDeleteVerify(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	// Delete one record — its VERSION index entry must be removed.
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()

	// Delete the other.
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify()
}

func TestVersionIndexDeleteAllVerify(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()

	s.DeleteAllRecords()
	s.Verify()
}

// --- Commit-unknown fault injection (targeted) ---

func TestVersionIndexCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Inject commit-unknown on first save. The retry should be idempotent:
	// the second transaction sees the existing record and does an update,
	// properly clearing the first tx's VERSION index entry and adding a new one.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()
}

func TestVersionIndexCommitUnknownMultipleSaves(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Commit-unknown on second save (new PK).
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	// Commit-unknown on third save.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()
}

func TestVersionIndexCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Commit-unknown on overwrite. The retry must:
	// 1. Load existing record (with tx1's version)
	// 2. Clear tx1's VERSION index entry (at tx1's versionstamp key)
	// 3. Add new entry (at tx2's versionstamp key)
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify()
}

func TestVersionIndexCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Commit-unknown on delete. The retry should be idempotent:
	// first tx deletes the record + clears VERSION entry,
	// second tx finds no record → delete is a no-op.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

func TestVersionIndexCommitUnknownDeleteAll(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	// Commit-unknown on DeleteAllRecords.
	// First tx range-clears everything. Second tx range-clears again (no-op).
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteAllRecords()
	s.Verify()
}

func TestVersionIndexSaveDeleteSaveCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save, delete, re-save same PK — each with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify()
}

// --- Composite VERSION index tests ---

func TestCompositeVersionIndexBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildCompositeVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()
}

func TestCompositeVersionIndexCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildCompositeVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
	s.Verify()
}

// --- Random stress tests ---

func TestVersionIndexRandomStress(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(12345), WithFaults(FaultsRetryHeavy))

	for i := 0; i < 200; i++ {
		pk := s.Rng.Int64N(30) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(1000)),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}
		if (i+1)%25 == 0 {
			s.Verify()
		}
	}
	s.Verify()
}

func TestVersionIndexHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(99999), WithFaults(FaultsRetryVeryHeavy))

	for i := 0; i < 200; i++ {
		pk := s.Rng.Int64N(20) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(500)),
			})
		} else if s.Rng.Float64() < 0.9 {
			s.DeleteRecord(tuple.Tuple{pk})
		} else {
			s.DeleteAllRecords()
		}
		if (i+1)%20 == 0 {
			s.Verify()
		}
	}
	s.Verify()
}

func TestVersionIndexAllFaultsStress(t *testing.T) {
	t.Parallel()
	md := buildVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(77777), WithFaults(FaultsAll))

	for i := 0; i < 150; i++ {
		pk := s.Rng.Int64N(25) + 1
		if s.Rng.Float64() < 0.65 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(800)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}
		if (i+1)%30 == 0 {
			s.Verify()
		}
	}
	s.Verify()
}

func TestCompositeVersionIndexRandomStress(t *testing.T) {
	t.Parallel()
	md := buildCompositeVersionIndexMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(54321), WithFaults(FaultsRetryHeavy))

	for i := 0; i < 150; i++ {
		pk := s.Rng.Int64N(25) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(500)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}
		if (i+1)%25 == 0 {
			s.Verify()
		}
	}
	s.Verify()
}
