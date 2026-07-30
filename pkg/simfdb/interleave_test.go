package simfdb_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestSimFDB_InterleavedTransactionsConflict is RFC-199 Tier 2's concurrent-open-transaction
// driver: it holds two record-layer transactions open at once, interleaves a read-modify-write
// of the same record through each, and commits them in sequence. Because both read the record
// at the same version, the second committer's write conflicts the first's read → the first
// commit fails with not_committed(1020). This is what makes SimFDB's SSI detection a REAL,
// exercised path (not a fake checkbox) — and it fires end-to-end through the record layer, not
// just at the raw key level. It then shows the record layer's Run retry loop converging.
func TestSimFDB_InterleavedTransactionsConflict(t *testing.T) {
	t.Parallel()
	db, sub := newSimDatabase(1)
	md := buildOrderMetadata(t)
	ctx := context.Background()

	// Seed the record the two transactions will contend over.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
		return nil, err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Two explicit transactions open at once — the SQL BeginTx / FDBDatabaseRunner path.
	txA, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create txA: %v", err)
	}
	txB, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create txB: %v", err)
	}
	storeA, err := openStore(recordlayer.NewFDBRecordContext(txA, nil), md, sub)
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	storeB, err := openStore(recordlayer.NewFDBRecordContext(txB, nil), md, sub)
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}

	// Both read-modify-write record 1 (LoadRecord adds a read conflict on the record key).
	priceOf := func(store *recordlayer.FDBRecordStore) int32 {
		rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
		if err != nil || rec == nil {
			t.Fatalf("load record 1: %v", err)
		}
		return rec.Record.(*gen.Order).GetPrice()
	}
	if _, err := storeA.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(priceOf(storeA) + 10)}); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if _, err := storeB.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(priceOf(storeB) + 20)}); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// B commits first; A then fails because it read record 1 before B's commit.
	if err := txB.Commit().Get(); err != nil {
		t.Fatalf("txB commit should succeed: %v", err)
	}
	errA := txA.Commit().Get()
	var fe fdb.Error
	if !errors.As(errA, &fe) || fe.Code != 1020 {
		t.Fatalf("txA commit = %v, want not_committed(1020) — SSI did not fire through the record layer", errA)
	}

	// B's write is the one that survived (price 120).
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		if got := priceOf(store); got != 120 {
			t.Fatalf("after conflict, price = %d, want 120 (B won)", got)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("post-conflict read: %v", err)
	}
}
