package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// steppingClock advances by a fixed step on every read. It is how these tests make time pass
// during a build WITHOUT a wall clock anywhere: elapsed time becomes a pure function of how many
// times the code under test looked at the clock, so a build that trips a time limit trips at the
// same point on every run and on every machine.
type steppingClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

// TestBuildIndexTimeLimitUsesTheEnvClock pins that the online indexer's time limit is measured
// on the env clock — the anchor AND the elapsed comparison, which must be the same clock.
//
// The seam allowlist called this site a "build-duration metric". It is not: the anchor
// BuildIndex mints is what throttleBetweenRanges compares oi.timeLimit against, so it decides
// when the build gives up with TimeLimitExceededError and therefore HOW MANY RANGES end up
// durably recorded in the built-range set. A wall-clock build stops at a point that depends on
// how fast the machine was, which is the one thing a simulation may not do.
//
// Both arms are needed because the two halves fail in OPPOSITE directions, and either one alone
// is satisfied by a mutation of the other. Reading the anchor off the wall clock and measuring
// against the sim clock yields a NEGATIVE elapsed, so the limit never trips — caught only by the
// arm that requires it to trip. Minting the anchor on the sim clock and measuring with
// time.Since yields years of elapsed on the first range — caught only by the arm that requires
// it not to.
func TestBuildIndexTimeLimitUsesTheEnvClock(t *testing.T) {
	t.Parallel()
	const records = 40

	// run indexes records under a clock that steps 1s per read, and reports what the build did.
	run := func(t *testing.T, name string, timeLimit time.Duration) (int64, error) {
		t.Helper()
		ctx := context.Background()
		env := &dst.Env{
			Clock:  &steppingClock{now: dst.Epoch, step: time.Second},
			Random: dst.NewSeededRandomness(11),
		}
		db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
		sub := subspace.FromBytes(tuple.Tuple{"timelimitseam", name}.Pack())

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
			for i := int64(1); i <= records; i++ {
				if _, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i), Price: proto.Int32(int32(i)),
				}); err != nil {
					return nil, err
				}
			}
			_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
			return nil, err
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// A small per-transaction limit forces many ranges, so throttleBetweenRanges — where
		// the limit is actually evaluated — runs repeatedly.
		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(db).SetMetaData(md).SetIndex(priceIndex).SetSubspace(sub).
			SetLimit(2).SetTimeLimit(timeLimit).Build()
		if err != nil {
			t.Fatalf("build indexer: %v", err)
		}
		return indexer.BuildIndex(ctx)
	}

	t.Run("limit far beyond the simulated elapsed time is not reached", func(t *testing.T) {
		t.Parallel()
		total, err := run(t, "generous", 24*time.Hour)
		var exceeded *TimeLimitExceededError
		if errors.As(err, &exceeded) {
			t.Fatalf("BuildIndex reported %v elapsed against a 24h limit under a clock that "+
				"steps one second per read — the elapsed comparison is reading the wall clock "+
				"while the anchor came off the env clock", exceeded.Elapsed)
		}
		if err != nil {
			t.Fatalf("BuildIndex: %v", err)
		}
		if total < records {
			t.Fatalf("indexed %d of %d records", total, records)
		}
	})

	t.Run("limit inside the simulated elapsed time is reached, reproducibly", func(t *testing.T) {
		t.Parallel()
		const limit = 5 * time.Second
		var first *TimeLimitExceededError
		for attempt := 0; attempt < 2; attempt++ {
			_, err := run(t, fmt.Sprintf("tight%d", attempt), limit)
			var exceeded *TimeLimitExceededError
			if !errors.As(err, &exceeded) {
				t.Fatalf("BuildIndex did not reach its %v limit under a clock that steps one "+
					"second per read (err=%v) — an anchor minted on the wall clock and "+
					"measured against the env clock spans two unrelated epochs, so elapsed "+
					"comes out NEGATIVE and the limit can never trip", limit, err)
			}
			if attempt == 0 {
				first = exceeded
				continue
			}
			if exceeded.Elapsed != first.Elapsed {
				t.Fatalf("the same build reported %v elapsed on one run and %v on the next — "+
					"a simulated build must stop at the same point every time", first.Elapsed,
					exceeded.Elapsed)
			}
		}
	})
}

// TestMutualFragmentOrderIsReproducible pins that a mutual build's fragment visit order is a
// function of the run's seed.
//
// The seam allowlist justified leaving this on the global math/rand by saying the built index is
// identical whatever order the fragments are visited in. That is true only of a build that RUNS
// TO COMPLETION. A build interrupted partway — which is the entire point of injecting faults —
// has recorded a different SUBSET of fragments in the persisted built-range set depending on
// where the walk had got to, and a later builder resumes from that set. So the order decides
// persisted bytes exactly under the conditions a simulation creates, and a seeded run has to
// replay it.
//
// Two builders from two FRESH envs at the same seed must agree; two seeds must not. A global
// math/rand draw can satisfy neither.
func TestMutualFragmentOrderIsReproducible(t *testing.T) {
	t.Parallel()
	// Enough boundaries for a fragment count > 1, or the order is trivially constant and the
	// test cannot see the seed at all.
	boundaries := [][]byte{{0x10}, {0x20}, {0x30}, {0x40}, {0x50}, {0x60}, {0x70}}

	order := func(seed uint64) (first, step, count int) {
		t.Helper()
		env := dst.NewSim(seed)
		db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
		oi := &OnlineIndexer{
			db:               db,
			mutual:           true,
			mutualBoundaries: boundaries,
			subspace:         subspace.FromBytes(tuple.Tuple{"fragseam"}.Pack()),
		}
		m, err := newMutualIndexBuilder(oi)
		if err != nil {
			t.Fatalf("newMutualIndexBuilder: %v", err)
		}
		return m.fragmentFirst, m.fragmentStep, m.fragmentCount
	}

	f1, s1, n1 := order(7)
	f2, s2, n2 := order(7)
	if n1 <= 1 {
		t.Fatalf("fragmentCount = %d; the preset boundaries must yield more than one fragment "+
			"or the visit order carries no information", n1)
	}
	if f1 != f2 || s1 != s2 || n1 != n2 {
		t.Fatalf("same seed produced different fragment orders: (first=%d step=%d n=%d) vs "+
			"(first=%d step=%d n=%d) — the order must be drawn from the run's env, not the "+
			"process-global math/rand", f1, s1, n1, f2, s2, n2)
	}

	// And the seed must actually reach it: across enough distinct seeds, the order cannot be
	// constant. (A single differing pair suffices; sweeping guards against an unlucky
	// collision on any one seed.)
	varied := false
	for seed := uint64(1); seed <= 32 && !varied; seed++ {
		if f, s, _ := order(seed); f != f1 || s != s1 {
			varied = true
		}
	}
	if !varied {
		t.Fatalf("32 distinct seeds all produced fragment order (first=%d step=%d) — the seed "+
			"is not reaching the draw", f1, s1)
	}
}

// TestScanTimeBudgetUsesTheEnvClock pins that a leaf cursor's TimeLimit is measured on the
// clock its anchor was minted on, and that under a simulation that clock is the env's.
//
// The budget is not instrumentation. When it trips, the cursor stops with TimeLimitReached and
// the caller gets a continuation — so the wall clock deciding it means a simulated run PAGES
// DIFFERENTLY depending on how fast the host was. This path is armed on every SQL statement
// (paginatingRows.executeProps always clamps to a per-transaction page budget so the FDB 5s wall
// is never crossed), so it is not a hypothetical corner.
//
// Both directions again. Frozen sim clock plus a 1ns limit must return everything: no simulated
// time has passed, so nothing may stop. A stepping clock plus a limit inside its range must stop
// early, and at the SAME row count every run.
func TestScanTimeBudgetUsesTheEnvClock(t *testing.T) {
	t.Parallel()
	const records = 30

	// scan indexes `records` rows and returns how many the cursor handed back under limit,
	// measured on clock.
	scan := func(t *testing.T, name string, clock dst.Clock, limit time.Duration) int {
		t.Helper()
		ctx := context.Background()
		env := &dst.Env{Clock: clock, Random: dst.NewSeededRandomness(13)}
		db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
		sub := subspace.FromBytes(tuple.Tuple{"scanbudget", name}.Pack())

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		if err != nil {
			t.Fatalf("build metadata: %v", err)
		}
		got, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= records; i++ {
				if _, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i), Price: proto.Int32(int32(i)),
				}); err != nil {
					return nil, err
				}
			}
			props := ForwardScan()
			props.ExecuteProperties = DefaultExecutePropertiesIn(env).WithTimeLimit(limit)
			cursor := store.ScanRecords(nil, props)
			defer cursor.Close()
			n := 0
			for {
				res, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !res.HasNext() {
					return n, nil
				}
				n++
			}
		})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return got.(int)
	}

	t.Run("frozen sim clock never trips the budget", func(t *testing.T) {
		t.Parallel()
		// A one-nanosecond budget: on the wall clock this stops after the free initial pass.
		if got := scan(t, "frozen", dst.NewSimClock(dst.Epoch), time.Nanosecond); got != records {
			t.Fatalf("scan returned %d of %d rows under a FROZEN sim clock with a 1ns budget — "+
				"no simulated time passed, so the budget cannot have been reached; the elapsed "+
				"comparison is reading the wall clock", got, records)
		}
	})

	t.Run("stepping clock trips it at a reproducible row", func(t *testing.T) {
		t.Parallel()
		// One second per clock read, budget 5s: the cursor must stop, and at the same row every
		// time. A wall-clock anchor measured against the env clock gives a NEGATIVE elapsed and
		// never stops at all.
		first := scan(t, "stepping0", &steppingClock{now: dst.Epoch, step: time.Second}, 5*time.Second)
		if first >= records {
			t.Fatalf("scan returned all %d rows under a clock stepping one second per read with "+
				"a 5s budget — the budget must be reached; an anchor minted off the env clock "+
				"and measured against another spans two epochs and can never trip", records)
		}
		second := scan(t, "stepping1", &steppingClock{now: dst.Epoch, step: time.Second}, 5*time.Second)
		if first != second {
			t.Fatalf("the same scan stopped after %d rows on one run and %d on the next — a "+
				"simulated page must end at the same row every time", first, second)
		}
	})
}

// TestScanLimiterStateElapsedIsAnchoredToItsOwnClock pins the mechanism the cursors rest on: a
// state anchored on an env reports that env's elapsed time, and a state with no env reports the
// wall clock's. Tying the anchor and the measurement to ONE stored clock is what makes the
// epoch-mismatch bug (a wall anchor minus a simulated now) unrepresentable rather than merely
// absent.
func TestScanLimiterStateElapsedIsAnchoredToItsOwnClock(t *testing.T) {
	t.Parallel()

	clock := dst.NewSimClock(dst.Epoch)
	simState := NewScanLimiterStateIn(&dst.Env{Clock: clock})
	if got := simState.Elapsed(); got != 0 {
		t.Fatalf("elapsed on a freshly minted sim state = %v, want 0", got)
	}
	clock.Advance(3 * time.Second)
	if got := simState.Elapsed(); got != 3*time.Second {
		t.Fatalf("elapsed after advancing the sim clock 3s = %v, want 3s", got)
	}
	if !simState.StartTime().Equal(dst.Epoch) {
		t.Fatalf("anchor = %v, want the sim Epoch %v — the anchor must come off the same clock "+
			"the elapsed measurement does", simState.StartTime(), dst.Epoch)
	}

	// Production: a nil env is the wall clock, so the anchor is a recent real instant and
	// elapsed is non-negative. This is the arm that would break if the nil default were ever
	// changed to something "more deterministic".
	before := time.Now()
	prodState := NewScanLimiterState()
	after := time.Now()
	if prodState.StartTime().Before(before) || prodState.StartTime().After(after) {
		t.Fatalf("production anchor %v not within [%v,%v] — a nil env must be the wall clock",
			prodState.StartTime(), before, after)
	}
	// No Elapsed() >= 0 assert here: the anchor is a past instant on the same monotonic clock
	// Elapsed() reads, so that comparison cannot fail whatever the seam does. The bracket above is
	// the assertion that can.

	// A nil state never trips a limit.
	var nilState *ScanLimiterState
	if got := nilState.Elapsed(); got != 0 {
		t.Fatalf("elapsed on a nil state = %v, want 0", got)
	}
}
