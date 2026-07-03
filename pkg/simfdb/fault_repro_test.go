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

// TestSimFDB_ByteReproducibleUnderFaults proves RFC-179's central claim: one seed reproduces
// the entire run identically — INCLUDING injected faults and the retries they trigger. The
// same workload runs twice with Buggify ON (seed-chosen 1020/1007/1021 injected at commits),
// and the persisted state is byte-identical across the two runs, because the fault sequence and
// the record layer's retry behavior are a deterministic function of the seed. This is the
// reproducibility real-FDB chaos (wall-clock, server-assigned versions) can never give.
func TestSimFDB_ByteReproducibleUnderFaults(t *testing.T) {
	t.Parallel()
	dump := func() []fdb.KeyValue {
		env := dst.NewSim(99) // Buggify ON — faults injected at seed-chosen commit points
		db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
		md := buildOrderMetadata(t)
		sub := subspace.FromBytes(tuple.Tuple{"simfdb-faults"}.Pack())
		ctx := context.Background()
		for i := 0; i < 12; i++ {
			pk := int64(i % 6)
			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rtx, md, sub)
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(pk),
					Price:    proto.Int32(int32(i * 10)),
					Quantity: proto.Int32(int32(i % 3)),
				})
				return nil, err
			}); err != nil {
				t.Fatalf("save %d under faults: %v", i, err)
			}
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
		t.Fatal("no keys persisted")
	}
	if len(a) != len(b) {
		t.Fatalf("same seed under faults produced %d vs %d keys", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i].Key, b[i].Key) || !bytes.Equal(a[i].Value, b[i].Value) {
			t.Fatalf("fault-run byte divergence at index %d:\n  A %x = %x\n  B %x = %x",
				i, a[i].Key, a[i].Value, b[i].Key, b[i].Value)
		}
	}
}
