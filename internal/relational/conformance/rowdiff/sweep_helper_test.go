package rowdiff

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// sweepSeeds runs seeds out of order on every core but merges in seed order,
// so a sweep's counters and first-N samples equal a sequential sweep's.
func TestSweepSeeds_MergesInSeedOrder(t *testing.T) {
	t.Parallel()
	const n = 64
	var mu sync.Mutex
	var ran []uint64
	var merged []uint64
	sweepSeeds(n, func(seed uint64) uint64 {
		// Early seeds finish last.
		time.Sleep(time.Duration(n-seed) * 100 * time.Microsecond)
		mu.Lock()
		ran = append(ran, seed)
		mu.Unlock()
		return seed * seed
	}, func(seed uint64, r uint64) {
		if r != seed*seed {
			t.Errorf("seed %d merged result %d, want %d", seed, r, seed*seed)
		}
		merged = append(merged, seed)
	})
	if len(merged) != n {
		t.Fatalf("merged %d seeds, want %d", len(merged), n)
	}
	for i, seed := range merged {
		if seed != uint64(i)+1 {
			t.Fatalf("merge order %v, want 1..%d", merged, n)
		}
	}
	slices.Sort(ran)
	if !slices.Equal(ran, merged) {
		t.Fatalf("ran %v, want each seed once", ran)
	}
	// No seeds, no calls.
	sweepSeeds(0, func(uint64) int { t.Fatal("run called for n=0"); return 0 }, func(uint64, int) { t.Fatal("merge called for n=0") })
}

// Callers holding a sweep at the same time share one planning; the last
// release forgets it, and a later caller plans afresh with the same result.
func TestDefaultSweep_SharedWhileHeldAndReleased(t *testing.T) {
	t.Parallel()
	// A seed count no other test uses, so this test owns the entry.
	const n = 3
	sqls := func(cases []sweptCase) []string {
		var out []string
		for _, c := range cases {
			for _, p := range c.plans {
				out = append(out, p.sql)
			}
		}
		return out
	}
	a, releaseA := defaultSweep(n)
	b, releaseB := defaultSweep(n)
	if len(a) != n || &a[0] != &b[0] {
		t.Fatal("two concurrent holders did not share one planning")
	}
	if len(sqls(a)) == 0 {
		t.Fatal("the sweep planned no queries")
	}
	releaseA()
	releaseA() // idempotent: a second release must not drop b's hold
	defaultSweepMu.Lock()
	_, held := defaultSweeps[n]
	defaultSweepMu.Unlock()
	if !held {
		t.Fatal("the sweep was forgotten while still held")
	}
	releaseB()
	defaultSweepMu.Lock()
	_, held = defaultSweeps[n]
	defaultSweepMu.Unlock()
	if held {
		t.Fatal("the sweep is retained after its last release")
	}
	c, releaseC := defaultSweep(n)
	defer releaseC()
	if &c[0] == &a[0] {
		t.Fatal("a caller after the last release got the released planning")
	}
	if !slices.Equal(sqls(c), sqls(a)) {
		t.Fatal("re-planning the same seeds produced different queries")
	}
}
