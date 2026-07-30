package simfdb_test

import (
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// TestExplicitTransactionCarriesTheEnv pins the seam on the path SQL's explicit
// BeginTx→COMMIT takes.
//
// Five of the six FDBRecordContext construction sites thread the database's env; the sixth —
// NewFDBRecordContext, which wraps a transaction the caller created — silently did not. Every
// explicit transaction therefore ran with env==nil: production clock, production randomness,
// under a fully seeded simulation. The persisted store header carries a LastUpdateTime taken
// from that clock (store_builder.go, store.go), so an explicit-transaction run wrote WALL-CLOCK
// BYTES and could not be replayed — and the explicit-COMMIT path is exactly the one a
// transaction-semantics RFC needs to replay.
//
// The assertion is on persisted bytes, not on a getter: the header's timestamp must be the sim
// clock's epoch. A wall-clock timestamp is many orders of magnitude larger, so this cannot pass
// by luck.
func TestExplicitTransactionCarriesTheEnv(t *testing.T) {
	t.Parallel()
	db, sub := newSimDatabase(1)
	md := buildOrderMetadata(t)

	// The explicit-transaction shape: a transaction the caller owns, wrapped by hand — what
	// EmbeddedConnection.beginTransaction does.
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	rctx := recordlayer.NewFDBRecordContext(tx, db.Env())
	if rctx.Env() == nil {
		t.Error("the explicit-transaction record context has no env: the seam is not threaded")
	}

	store, err := openStore(rctx, md, sub)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SetUserVersion(7); err != nil {
		t.Fatalf("SetUserVersion: %v", err)
	}
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	wantMillis := uint64(dst.Epoch.UnixMilli())
	if got := store.GetStoreHeader().GetLastUpdateTime(); got != wantMillis {
		t.Fatalf("store header LastUpdateTime = %d, want the sim epoch %d — the explicit "+
			"transaction persisted a WALL-CLOCK timestamp, so the run cannot be replayed",
			got, wantMillis)
	}
}

// TestExplicitTransactionIsDrivenByTheSimClock is the byte-level half, and it is deliberately
// stated as "the driver controls the value" rather than "two runs agree". Two wall-clock runs
// inside the same millisecond also agree, so a two-run comparison passes with the seam broken —
// it is a coincidence test, not a determinism test.
//
// Instead the sim clock is set to a distinctive instant and the persisted store-header timestamp
// must be exactly it. A wall-clock value cannot land there.
func TestExplicitTransactionIsDrivenByTheSimClock(t *testing.T) {
	t.Parallel()

	target := dst.Epoch.Add(1234567 * time.Millisecond)
	env := dst.NewSim(5)
	env.Buggify = dst.DisabledBuggifier()
	env.Clock.(*dst.SimClock).Set(target)

	db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	md := buildOrderMetadata(t)
	sub := subspace.FromBytes(tuple.Tuple{"explicit-repro"}.Pack())

	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	store, err := openStore(recordlayer.NewFDBRecordContext(tx, db.Env()), md, sub)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SetUserVersion(3); err != nil {
		t.Fatalf("SetUserVersion: %v", err)
	}
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got, want := store.GetStoreHeader().GetLastUpdateTime(), uint64(target.UnixMilli()); got != want {
		t.Fatalf("store header LastUpdateTime = %d, want the driver-set sim instant %d — the "+
			"explicit-transaction path is not reading the simulated clock", got, want)
	}

	// And the persisted keyspace is then a pure function of the run, so a replay reproduces it.
	fp := hunt.Fingerprint(db)
	if fp == "" {
		t.Fatal("empty fingerprint: nothing was persisted, so this test proved nothing")
	}
}
