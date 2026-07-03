package simfdb_test

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// buildOrderMetadata builds record metadata with the demo Order/Customer/TypedRecord types —
// the same shape the chaos suite uses.
func buildOrderMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder()
	b.SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.SetRecordCountKey(recordlayer.EmptyKey())
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// newSimDatabase builds a record-layer database over SimFDB with a fault-free seeded env
// (deterministic clock + randomness, Buggify OFF) — a clean deterministic run. Fault injection
// (Buggify) is exercised by its own test so these tests isolate the store/version determinism.
func newSimDatabase(seed uint64) (*recordlayer.FDBDatabase, subspace.Subspace) {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	return db, subspace.FromBytes(tuple.Tuple{"simfdb-itest"}.Pack())
}

func openStore(rtx *recordlayer.FDBRecordContext, md *recordlayer.RecordMetaData, sub subspace.Subspace) (*recordlayer.FDBRecordStore, error) {
	return recordlayer.NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(md).
		SetSubspace(sub).
		CreateOrOpen()
}

// TestRecordLayerOverSimFDB is the RFC-179 keystone payoff: the record layer saves and loads a
// record with SimFDB as the fdb.BackendDatabase — no cluster, no Docker, single goroutine.
func TestRecordLayerOverSimFDB(t *testing.T) {
	t.Parallel()
	db, sub := newSimDatabase(1)
	md := buildOrderMetadata(t)
	ctx := context.Background()

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		for i := 0; i < 5; i++ {
			if _, err := store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(int64(i)),
				Price:    proto.Int32(int32(i * 100)),
				Quantity: proto.Int32(int32(i)),
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("save workload: %v", err)
	}

	// Load back in a fresh transaction — proves persistence + read path.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		rec, err := store.LoadRecord(tuple.Tuple{int64(3)})
		if err != nil {
			return nil, err
		}
		if rec == nil {
			t.Fatal("record 3 not found after save")
		}
		order, ok := rec.Record.(*gen.Order)
		if !ok {
			t.Fatalf("loaded record has type %T, want *gen.Order", rec.Record)
		}
		if order.GetPrice() != 300 {
			t.Fatalf("record 3 price = %d, want 300", order.GetPrice())
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("load: %v", err)
	}
}

// TestSimFDBByteReproducibleAcrossRuns proves the RFC-179 payoff: the same seeded workload
// produces byte-identical persisted state across independent runs — the record layer's stack
// over SimFDB is fully deterministic (store header time via the sim clock, versions via the
// logical counter). This is the byte-reproducibility that real-FDB chaos cannot deliver.
func TestSimFDBByteReproducibleAcrossRuns(t *testing.T) {
	t.Parallel()
	dump := func() []fdb.KeyValue {
		db, sub := newSimDatabase(42)
		md := buildOrderMetadata(t)
		ctx := context.Background()
		if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := openStore(rtx, md, sub)
			if err != nil {
				return nil, err
			}
			for i := 0; i < 8; i++ {
				if _, err := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32(i)),
					Quantity: proto.Int32(int32(i % 3)),
				}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("workload: %v", err)
		}
		var kvs []fdb.KeyValue
		if _, err := db.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
			kvs = rtx.GetRange(fdb.KeyRange{Begin: fdb.Key{}, End: fdb.Key{0xff}}, fdb.RangeOptions{}).GetSliceOrPanic()
			return nil, nil
		}); err != nil {
			t.Fatalf("dump: %v", err)
		}
		return kvs
	}

	a, b := dump(), dump()
	if len(a) == 0 {
		t.Fatal("no keys persisted — workload did nothing")
	}
	if len(a) != len(b) {
		t.Fatalf("same seed produced %d vs %d keys", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i].Key, b[i].Key) || !bytes.Equal(a[i].Value, b[i].Value) {
			t.Fatalf("byte divergence at index %d:\n  A %x = %x\n  B %x = %x",
				i, a[i].Key, a[i].Value, b[i].Key, b[i].Value)
		}
	}
}
