package hunt

import (
	"context"
	"encoding/binary"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/chaos"
	"fdb.dev/pkg/simfdb"

	"google.golang.org/protobuf/proto"
)

// TestHuntCleanSweep replays a block of seeds under the default fault-injecting profile and
// asserts every one is clean. This is the brute-force result in miniature: the record layer's
// save/index-maintenance paths survive commit_unknown/conflict/too_old retry across the whole
// keyspace. If a future change breaks idempotency, one of these seeds goes red here first.
func TestHuntCleanSweep(t *testing.T) {
	t.Parallel()
	for seed := uint64(0); seed < 40; seed++ {
		rep := Run(seed, smokeCfg)
		if rep.Failed() {
			t.Fatalf("seed %d found a bug:\n%s", seed, rep)
		}
	}
}

// smokeCfg is the fast in-suite profile: fewer ops than the full brute/fuzz default so the
// pre-commit `just test` stays quick, while still exercising every index maintainer under
// faults. Deep sweeping uses the Config{} default (300 ops) via HUNT_SECONDS / FuzzHunt.
var smokeCfg = Config{NumOps: 120}

// TestHuntFaultsActuallyFire guards the guard: a clean sweep is only meaningful if faults are
// really being injected. Across a block of seeds the aggregate fault count must be well above
// zero (each seed activates the three commit-fault sites at 0.25 and fires at 0.25).
func TestHuntFaultsActuallyFire(t *testing.T) {
	t.Parallel()
	const seeds = 40
	total := 0
	for seed := uint64(0); seed < seeds; seed++ {
		total += Run(seed, smokeCfg).FaultsFired
	}
	if total == 0 {
		t.Fatalf("no faults fired across %d seeds — the hunt is only exercising the happy path", seeds)
	}
	t.Logf("faults fired across %d seeds: %d", seeds, total)
}

// TestHuntDeterminism is the RFC-199 keystone: the same seed produces byte-identical persisted
// state and the identical fault schedule on every run. A mismatch means nondeterminism has
// leaked into the layer (map-iteration order, a wall clock, crypto/rand bypassing the Env) —
// which would make every reproducer worthless.
func TestHuntDeterminism(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 7, 42, 99, 12345} {
		a := Run(seed, smokeCfg)
		b := Run(seed, smokeCfg)
		if a.Failed() || b.Failed() {
			t.Fatalf("seed %d unexpectedly failed:\n%s\n%s", seed, a, b)
		}
		if a.Fingerprint != b.Fingerprint {
			t.Fatalf("seed %d nondeterministic: fingerprint %s != %s", seed, a.Fingerprint, b.Fingerprint)
		}
		if a.FaultsFired != b.FaultsFired {
			t.Fatalf("seed %d nondeterministic fault schedule: %d != %d", seed, a.FaultsFired, b.FaultsFired)
		}
	}
}

// TestOracleHasTeethOverSimFDB proves the oracle Run depends on actually catches a store/model
// divergence over SimFDB — otherwise a green sweep would be a no-op. We write a record through
// the store but deliberately do NOT tell the model, then run the exact verify path Run uses:
// it must report a violation.
func TestOracleHasTeethOverSimFDB(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := dst.NewSim(1)
	env.Buggify = dst.DisabledBuggifier()
	backend := simfdb.New(env)
	db := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	d := &driver{
		db:      db,
		cleanDB: db,
		md:      KitchenSinkMetadata(),
		sub:     subspace.FromBytes(tuple.Tuple{"teeth"}.Pack()),
		model:   chaos.NewStoreModel(KitchenSinkMetadata()),
	}

	// A couple of tracked saves — store and model agree.
	if err := d.save(ctx, 1, 100, 2); err != nil {
		t.Fatalf("save: %v", err)
	}
	if v := d.verify(ctx); len(v) > 0 {
		t.Fatalf("clean state should verify, got: %v", v)
	}

	// An UNtracked save: the store now has a record the model never saw.
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := d.open(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(500), Quantity: proto.Int32(9)})
		return nil, err
	})
	if err != nil {
		t.Fatalf("untracked save: %v", err)
	}
	if v := d.verify(ctx); len(v) == 0 {
		t.Fatal("oracle has no teeth: an untracked store record produced no violation")
	}
}

// TestFingerprintDistinguishesState sanity-checks the fingerprint: different seeds (hence
// different workloads) must produce different persisted states, else the determinism oracle
// would be comparing constants.
func TestFingerprintDistinguishesState(t *testing.T) {
	t.Parallel()
	const seeds = 30
	seen := map[string]uint64{}
	for seed := uint64(0); seed < seeds; seed++ {
		fp := Run(seed, smokeCfg).Fingerprint
		if fp == "" {
			t.Fatalf("seed %d produced no fingerprint", seed)
		}
		seen[fp] = seed
	}
	if len(seen) < seeds/2 {
		t.Fatalf("fingerprints not distinguishing state: only %d distinct across %d seeds", len(seen), seeds)
	}
}

// TestMinFailingPrefix unit-tests the shrink search with a synthetic predicate (the record
// layer has no natural failing seed to shrink), covering the first-failing-prefix and
// none-shorter-fails branches.
func TestMinFailingPrefix(t *testing.T) {
	t.Parallel()
	// Fails at n>=5: minimum is 5.
	if got := minFailingPrefix(20, func(n int) bool { return n >= 5 }); got != 5 {
		t.Fatalf("first-failing-prefix: got %d, want 5", got)
	}
	// Only the full length fails: returns max without probing it.
	probed := 0
	if got := minFailingPrefix(8, func(n int) bool { probed++; return false }); got != 8 {
		t.Fatalf("none-shorter: got %d, want 8", got)
	}
	if probed != 7 { // scans n=1..7, never probes n=8 (already known to fail)
		t.Fatalf("none-shorter probed %d times, want 7", probed)
	}
	// Non-monotone (fails at 3, repaired at 4+): honest minimum is the first failing prefix.
	if got := minFailingPrefix(10, func(n int) bool { return n == 3 }); got != 3 {
		t.Fatalf("non-monotone: got %d, want 3", got)
	}
}

// TestShrinkNoRepro exercises Shrink's honest-guard path: a clean seed shrinks to itself with
// a clean report (Shrink never fabricates a failure).
func TestShrinkNoRepro(t *testing.T) {
	t.Parallel()
	_, rep := Shrink(42, Config{})
	if rep.Failed() {
		t.Fatalf("clean seed 42 should not shrink to a failure:\n%s", rep)
	}
}

// TestProfilesRunClean builds every overnight-sweep profile and runs a few seeds through each,
// so a broken schema or a per-maintainer regression is caught before a long unattended run.
func TestProfilesRunClean(t *testing.T) {
	t.Parallel()
	for _, p := range Profiles() {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			cfg := p.Cfg
			cfg.NumOps = 100
			for seed := uint64(0); seed < 6; seed++ {
				if rep := Run(seed, cfg); rep.Failed() {
					t.Fatalf("profile %s seed %d:\n%s", p.Name, seed, rep)
				}
			}
		})
	}
}

// TestConfigDefaults pins the zero-value brute-force profile.
func TestConfigDefaults(t *testing.T) {
	t.Parallel()
	c := Config{}.withDefaults()
	if c.NumOps != 300 || c.MaxPKs != 30 || c.VerifyEvery != 25 || c.FaultProb != 0.25 || c.Workload == nil {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Workload.Name() != "record" {
		t.Fatalf("default workload = %q, want record", c.Workload.Name())
	}
}

// TestHuntSeed replays a single seed, the reproduce entrypoint named in Report.String(). Set
// HUNT_SEED to a suspect seed to replay it in isolation with per-op verification; unset it
// defaults to seed 0 (always exercises Run once).
func TestHuntSeed(t *testing.T) {
	t.Parallel()
	seed := uint64(0)
	if s := os.Getenv("HUNT_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("HUNT_SEED=%q: %v", s, err)
		}
		seed = v
	}
	cfg := Config{VerifyEvery: 1} // pin the exact failing op if any
	rep := Run(seed, cfg)
	t.Logf("%s", rep)
	if rep.Failed() {
		t.Fatalf("seed %d reproduced a bug:\n%s", seed, rep)
	}
}

// TestBruteHunt is the loop-until-bug runner. By default it sweeps a bounded block of seeds as
// a fast smoke. Set HUNT_SECONDS=N to run a wall-clock-budgeted parallel sweep across all cores
// (an overnight bug hunt: HUNT_SECONDS=3600), and HUNT_SEED_START to shift the block so
// separate machines cover disjoint seed space. The first failing seed fails the test with a
// full reproducer.
func TestBruteHunt(t *testing.T) {
	t.Parallel()

	start := uint64(0)
	if s := os.Getenv("HUNT_SEED_START"); s != "" {
		start, _ = strconv.ParseUint(s, 10, 64)
	}

	var deadline time.Time
	if s := os.Getenv("HUNT_SECONDS"); s != "" {
		secs, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("HUNT_SECONDS=%q: %v", s, err)
		}
		deadline = time.Now().Add(time.Duration(secs * float64(time.Second)))
	}

	// Bounded smoke when no time budget is set: keep `just test` fast but still meaningful.
	// A time-budgeted run (HUNT_SECONDS) hunts with the full Config{} profile for depth.
	const smokeSeeds = 50
	cfg := Config{}
	if deadline.IsZero() {
		cfg = smokeCfg
	}

	var (
		next     atomic.Uint64
		checked  atomic.Uint64
		mu       sync.Mutex
		firstBad *Report
	)
	next.Store(start)

	worker := func() {
		for {
			if firstBad != nil {
				return
			}
			seed := next.Add(1) - 1
			if deadline.IsZero() {
				if seed >= start+smokeSeeds {
					return
				}
			} else if time.Now().After(deadline) {
				return
			}
			rep := Run(seed, cfg)
			checked.Add(1)
			if rep.Failed() {
				mu.Lock()
				if firstBad == nil || rep.Seed < firstBad.Seed {
					firstBad = rep
				}
				mu.Unlock()
				return
			}
		}
	}

	workers := 1
	if !deadline.IsZero() {
		workers = maxParallel()
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	wg.Wait()

	t.Logf("brute hunt: checked %d seeds from %d", checked.Load(), start)
	if firstBad != nil {
		t.Fatalf("brute hunt found a bug:\n%s", firstBad)
	}
	// A WORK-DONE FLOOR. Without it this test passes when it checks ZERO seeds — a harness
	// that silently stopped producing runs is indistinguishable from one that found no bug,
	// and the second reading is the one everyone takes.
	if got := checked.Load(); got == 0 {
		t.Fatal("brute hunt checked ZERO seeds: the sweep produced no runs at all, so \"no bug " +
			"found\" says nothing about the code")
	} else if deadline.IsZero() && got < smokeSeeds {
		t.Fatalf("brute hunt checked %d of %d smoke seeds: the sweep stopped early without "+
			"reporting a bug, which reads as a clean run and is not one", got, smokeSeeds)
	}
}

// FuzzHunt is the coverage-guided driver: libFuzzer mutates the seed bytes, Go saves any
// crashing input to testdata/fuzz as a permanent regression. Run with
//
//	bazelisk test //pkg/simfdb/hunt:hunt_test --test_arg=-test.fuzz=FuzzHunt \
//	  --test_arg=-test.fuzztime=60s --test_arg=-test.fuzzcachedir=/tmp/fuzz-cache \
//	  --sandbox_writable_path=/tmp/fuzz-cache
func FuzzHunt(f *testing.F) {
	f.Add(uint64ToBytes(0))
	f.Add(uint64ToBytes(1))
	f.Add(uint64ToBytes(42))
	f.Add(uint64ToBytes(1 << 40))
	f.Fuzz(func(t *testing.T, data []byte) {
		seed := seedFromBytes(data)
		// A lighter op count keeps fuzz iterations fast; the loop's breadth comes from the
		// mutation engine exploring the seed space, not from any single deep run.
		rep := Run(seed, Config{NumOps: 150})
		if rep.Failed() {
			t.Fatalf("fuzz found a bug:\n%s", rep)
		}
	})
}

func seedFromBytes(b []byte) uint64 {
	var buf [8]byte
	copy(buf[:], b)
	return binary.LittleEndian.Uint64(buf[:])
}

func uint64ToBytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func maxParallel() int {
	n := 4
	if s := os.Getenv("HUNT_WORKERS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
		}
	}
	return n
}
