package recordlayer

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"google.golang.org/protobuf/proto"
)

// TestStoreHeader_LastUpdateTimeSeamed proves the RFC-199 Tier-0 Clock seam on the
// primary persisted-byte site: the store header's LastUpdateTime. Under a sim env it is a
// deterministic function of the sim clock (Epoch), reproducible across runs; under
// production (nil env) it falls back to the wall clock — byte-identical to pre-seam
// behavior. This is the site every store open writes (createStoreHeader).
func TestStoreHeader_LastUpdateTimeSeamed(t *testing.T) {
	t.Parallel()
	epochMillis := uint64(dst.Epoch.UnixMilli())

	// Sim env → the persisted timestamp is the sim clock (Epoch), not the wall clock.
	h1 := createStoreHeader(1, nil, dst.NewSim(7))
	if h1.LastUpdateTime == nil {
		t.Fatal("LastUpdateTime unset")
	}
	if *h1.LastUpdateTime != epochMillis {
		t.Fatalf("sim LastUpdateTime = %d, want Epoch %d", *h1.LastUpdateTime, epochMillis)
	}

	// Reproducible: a fresh sim at the same seed yields the identical persisted timestamp.
	h2 := createStoreHeader(1, nil, dst.NewSim(7))
	if *h2.LastUpdateTime != *h1.LastUpdateTime {
		t.Fatalf("sim header not reproducible: %d vs %d", *h1.LastUpdateTime, *h2.LastUpdateTime)
	}

	// Production (nil env) uses the wall clock — must NOT be the fixed Epoch, and must be
	// a recent time (proves the fallback path is live, i.e. we didn't hard-wire Epoch).
	before := uint64(time.Now().UnixMilli())
	h3 := createStoreHeader(1, nil, nil)
	after := uint64(time.Now().UnixMilli())
	if *h3.LastUpdateTime == epochMillis {
		t.Fatal("production LastUpdateTime is the sim Epoch — the wall-clock fallback is broken")
	}
	if *h3.LastUpdateTime < before || *h3.LastUpdateTime > after {
		t.Fatalf("production LastUpdateTime %d not within [%d,%d]", *h3.LastUpdateTime, before, after)
	}
}

// TestContextEnv_NilSafeAndInherited proves the Env threads DB→context and that a nil
// database env (production) yields a context whose Env() accessors are safe and wall-clock.
func TestContextEnv_NilSafeAndInherited(t *testing.T) {
	t.Parallel()

	// Production database: contexts inherit a nil env, which Env() returns and the *dst.Env
	// accessors treat as wall clock.
	prod := &FDBDatabase{}
	if prod.Env() != nil {
		t.Fatal("unset database env should be nil (production)")
	}
	prodCtx := &FDBRecordContext{env: prod.env}
	if prodCtx.Env().Now().IsZero() {
		t.Fatal("nil-env context Now() returned zero (should be wall clock)")
	}

	// Simulation database: SetEnv installs a seeded env that contexts inherit.
	sim := (&FDBDatabase{}).SetEnv(dst.NewSim(3))
	simCtx := &FDBRecordContext{env: sim.env}
	if !simCtx.Env().Now().Equal(dst.Epoch) {
		t.Fatalf("sim context Now() = %v, want Epoch %v", simCtx.Env().Now(), dst.Epoch)
	}

	// A nil context is safe too.
	var nilCtx *FDBRecordContext
	if nilCtx.Env() != nil {
		t.Fatal("nil context Env() should be nil")
	}
	if nilCtx.Env().Now().IsZero() {
		t.Fatal("nil context Env().Now() returned zero (should be wall clock)")
	}
}

// TestIndexBlockLeaseReadsTheEnvClock pins the lease-expiry seam on the indexing stamp's block,
// end to end: BlockIndex mints the expiry off the env clock, and setIndexingTypeOrThrowForIndex
// compares it against the env clock.
//
// Both halves have to be seamed or the branch a simulation exercises is the OPPOSITE of the one
// it wrote. The sim clock starts at a fixed epoch in the past, so a wall-clock READ of an expiry
// minted at sim time sees it as long expired; a wall-clock WRITE against a sim-time read sees
// every lease as eternal. Either way the run reports having tested the blocked path while
// testing the unblocked one.
//
// Nothing pinned this. The existing block tests all run under a nil env, where both spellings
// read the same wall clock, so reverting either seam left the suite green.
func TestIndexBlockLeaseReadsTheEnvClock(t *testing.T) {
	t.Parallel()
	const leaseTTL = time.Hour
	for _, tc := range []struct {
		name        string
		advance     time.Duration
		wantBlocked bool
	}{
		// The lease is live in SIM time. A wall-clock READ of the sim-minted expiry would see
		// an instant in 2020 and call it expired.
		{"lease live in sim time", 0, true},
		// The sim clock is moved past the lease. A wall-clock WRITE would have stamped an
		// expiry years ahead of the sim clock, so this would still read as blocked.
		{"lease expired in sim time", 2 * leaseTTL, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			clock := dst.NewSimClock(dst.Epoch)
			env := &dst.Env{Clock: clock, Random: dst.NewSeededRandomness(5)}
			db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
			sub := subspace.FromBytes(tuple.Tuple{"leaseseam", tc.name}.Pack())

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			if err != nil {
				t.Fatalf("build metadata: %v", err)
			}

			if _, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 5; i++ {
					if _, err := store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)),
					}); err != nil {
						return nil, err
					}
				}
				if _, err := store.ClearAndMarkIndexWriteOnly("Order$price"); err != nil {
					return nil, err
				}
				// A BY_RECORDS stamp is what BlockIndex marks and what the build compares
				// against; without it BlockIndex has nothing to block.
				return nil, store.SaveIndexingTypeStamp(priceIndex, &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				})
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(db).SetMetaData(md).SetIndex(priceIndex).SetSubspace(sub).Build()
			if err != nil {
				t.Fatalf("build indexer: %v", err)
			}
			if err := indexer.BlockIndex(ctx, "maintenance", leaseTTL); err != nil {
				t.Fatalf("BlockIndex: %v", err)
			}
			clock.Advance(tc.advance)

			_, buildErr := indexer.BuildIndex(ctx)
			var partly *PartlyBuiltError
			gotBlocked := errors.As(buildErr, &partly)
			if gotBlocked != tc.wantBlocked {
				t.Fatalf("after BlockIndex(ttl=%v) and advancing the sim clock by %v, "+
					"BuildIndex blocked=%v (err=%v), want blocked=%v — the lease expiry must be "+
					"both MINTED and COMPARED against the env clock",
					leaseTTL, tc.advance, gotBlocked, buildErr, tc.wantBlocked)
			}
			if !tc.wantBlocked && buildErr != nil {
				t.Fatalf("BuildIndex after the lease expired: %v", buildErr)
			}
		})
	}
}
