package plans

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// The sibling file pins the memo's individual invariants. This one pins the
// property those invariants add up to, which is what a census over a real planner
// sweep measured and which no single-case test states: **the classification of a
// read is TOTAL, and the store decision is fully determined by it.**
//
// Every read is exactly one of three things — a hit, a miss on an empty cell, or a
// miss on a cell owned by someone else — and the write side follows with no
// remaining freedom: every empty miss stores, every foreign miss declines, and
// nothing else ever happens. A sweep of 3,071,048 reads over 649,360 distinct
// owners bore that out exactly (storeOK == empty misses, storeDeclined == foreign
// misses, both to the unit). That is a stronger statement than any one case, and
// it is the statement that has to survive a refactor.
//
// # Why this dimension needed its own test
//
// The four invariant tests each drive ONE path and assert what that path does.
// None of them can observe a read that falls through every arm, or a store that
// happens on a path nobody classified — and those are precisely the shapes a
// future edit to the cell would introduce. A fourth outcome appearing (a read that
// is neither hit nor miss, a store on a hit) is invisible to a suite of
// single-path tests and visible here as an arithmetic mismatch.
//
// # And why the hit RATE is pinned, not just the pairing
//
// A memo can regress in two directions with opposite responses, and the counts are
// the only thing that separates them: going DARK (hits collapse, the cache stopped
// working) versus simply being asked more (more plans constructed). A pairing check
// alone stays perfectly green on a memo that never hits at all — every read would
// be an empty miss, every store would succeed, and the arithmetic would balance.
// So the workload below fixes the exact hit count it must produce.

// memoReadOutcome is what a single HashCodeWithoutChildren call did to the cell.
type memoReadOutcome int

const (
	memoHit memoReadOutcome = iota
	memoMissEmpty
	memoMissForeign
)

func (o memoReadOutcome) String() string {
	switch o {
	case memoHit:
		return "hit"
	case memoMissEmpty:
		return "miss-empty"
	case memoMissForeign:
		return "miss-foreign"
	}
	return "unknown"
}

// memoCensus counts read outcomes and the store decisions that followed.
type memoCensus struct {
	reads    map[memoReadOutcome]int
	stored   int
	declined int
}

func newMemoCensus() *memoCensus {
	return &memoCensus{reads: map[memoReadOutcome]int{}}
}

// observe asks plan for its hash and records BOTH what the read was and what the
// write side then did, classifying each from the cell's state around the call
// rather than by re-running the memo's own predicates. Re-deriving the answer from
// the same conditions the code uses would make this a tautology; reading the cell
// before and after makes it an observation.
func (c *memoCensus) observe(t *testing.T, plan *RecordQueryIndexPlan) uint64 {
	t.Helper()

	cell := plan.hashMemo
	if cell == nil {
		t.Fatal("plan carries no memo cell; the census would count nothing")
	}
	before := cell.state.Load()

	var outcome memoReadOutcome
	switch {
	case before == nil:
		outcome = memoMissEmpty
	case before.owner == any(plan):
		outcome = memoHit
	default:
		outcome = memoMissForeign
	}

	got := plan.HashCodeWithoutChildren()
	after := cell.state.Load()

	switch outcome {
	case memoHit:
		if after != before {
			t.Errorf("a hit rewrote the cell; a read that already had its answer must not store")
		}
		if got != before.hash {
			t.Errorf("hit returned %d, cell holds %d", got, before.hash)
		}
	case memoMissEmpty:
		if after == nil {
			t.Fatal("a miss on an empty cell did not store; the memo would never warm up")
		}
		if after.owner != any(plan) {
			t.Errorf("a miss on an empty cell stored under owner %v, not the asking plan", after.owner)
		}
		if after.hash != got {
			t.Errorf("stored %d but returned %d", after.hash, got)
		}
		c.stored++
	case memoMissForeign:
		if after != before {
			t.Errorf("a miss on a foreign-owned cell rewrote it; the two sharers would now " +
				"evict each other on every comparison")
		}
		c.declined++
	}
	c.reads[outcome]++
	return got
}

func (c *memoCensus) total() int {
	n := 0
	for _, v := range c.reads {
		n += v
	}
	return n
}

func (c *memoCensus) String() string {
	return fmt.Sprintf("reads=%d hit=%d miss-empty=%d miss-foreign=%d stored=%d declined=%d",
		c.total(), c.reads[memoHit], c.reads[memoMissEmpty], c.reads[memoMissForeign],
		c.stored, c.declined)
}

// TestMemoStoreDecisionIsDeterminedByReadClassification drives a workload with a
// known shape and asserts the exact counts, so both failure directions are caught:
// a fourth outcome or a stray store breaks the arithmetic, and a memo that stopped
// hitting breaks the hit count.
func TestMemoStoreDecisionIsDeterminedByReadClassification(t *testing.T) {
	t.Parallel()

	const (
		owners        = 7 // independently constructed plans
		readsPerOwner = 5 // ... each asked repeatedly, as the memo insert loops do
	)

	census := newMemoCensus()

	originals := make([]*RecordQueryIndexPlan, 0, owners)
	for i := 0; i < owners; i++ {
		originals = append(originals, memoTestIndexPlan(t))
	}

	// Phase 1: each owner asked readsPerOwner times. The first read of each is an
	// empty miss that stores; every later read is a hit.
	for _, plan := range originals {
		for r := 0; r < readsPerOwner; r++ {
			census.observe(t, plan)
		}
	}

	// Phase 2: a copy of each owner, sharing its cell. Every read is a foreign miss
	// that declines — and, critically, the original must still answer from its own
	// memo afterwards, which phase 3 checks.
	copies := make([]*RecordQueryIndexPlan, 0, owners)
	for i, plan := range originals {
		variant := plan.WithScanComparisons([]*predicates.ComparisonRange{
			scanCostRange(t, predicates.ComparisonEquals, int64(100+i)),
		})
		if variant.hashMemo != plan.hashMemo {
			t.Fatal("the copy got its own cell, so the foreign-miss arm is not being exercised")
		}
		copies = append(copies, variant)
	}
	for _, variant := range copies {
		for r := 0; r < readsPerOwner; r++ {
			census.observe(t, variant)
		}
	}

	// Phase 3: the originals again. Every read must still be a hit — the copies did
	// not take the cell.
	for _, plan := range originals {
		census.observe(t, plan)
	}

	wantEmpty := owners                   // one per owner, on its first read
	wantForeign := owners * readsPerOwner // every copy read declines
	wantHit := owners*(readsPerOwner-1) + owners
	wantTotal := wantEmpty + wantForeign + wantHit

	if got := census.reads[memoMissEmpty]; got != wantEmpty {
		t.Errorf("miss-empty = %d, want %d (%s)", got, wantEmpty, census)
	}
	if got := census.reads[memoMissForeign]; got != wantForeign {
		t.Errorf("miss-foreign = %d, want %d (%s)", got, wantForeign, census)
	}
	if got := census.reads[memoHit]; got != wantHit {
		t.Errorf("hit = %d, want %d — the memo is going dark, which is a different "+
			"failure from being asked more and has the opposite response (%s)",
			got, wantHit, census)
	}
	if got := census.total(); got != wantTotal {
		t.Errorf("total reads = %d, want %d; the classification is not total, so some read "+
			"took a path no arm accounts for (%s)", got, wantTotal, census)
	}

	// The pairing the sweep confirmed over three million operations: the store
	// decision carries no freedom beyond the read classification.
	if census.stored != census.reads[memoMissEmpty] {
		t.Errorf("stored %d times on %d empty misses; a store happened on a path that was "+
			"not an empty miss, or an empty miss failed to warm the cell (%s)",
			census.stored, census.reads[memoMissEmpty], census)
	}
	if census.declined != census.reads[memoMissForeign] {
		t.Errorf("declined %d times on %d foreign misses (%s)",
			census.declined, census.reads[memoMissForeign], census)
	}
	if census.stored+census.declined != census.reads[memoMissEmpty]+census.reads[memoMissForeign] {
		t.Errorf("write-side decisions (%d) do not account for every miss (%d) (%s)",
			census.stored+census.declined,
			census.reads[memoMissEmpty]+census.reads[memoMissForeign], census)
	}
}

// TestMemoCensusObserverCanSeeAllThreeOutcomes guards the instrument rather than
// the memo. The census above asserts exact counts, so an observer that could only
// ever report one outcome would fail loudly — except in the one case that matters:
// if it silently classified everything as a hit while the memo was dark, the
// arithmetic could still be made to balance. Requiring each arm to be non-empty
// keeps the instrument honest independently of the counts it produces.
func TestMemoCensusObserverCanSeeAllThreeOutcomes(t *testing.T) {
	t.Parallel()

	census := newMemoCensus()
	plan := memoTestIndexPlan(t)

	census.observe(t, plan) // empty cell -> miss-empty
	census.observe(t, plan) // now owned by plan -> hit

	variant := plan.WithScanComparisons([]*predicates.ComparisonRange{
		scanCostRange(t, predicates.ComparisonEquals, int64(9)),
	})
	census.observe(t, variant) // cell owned by plan -> miss-foreign

	for _, outcome := range []memoReadOutcome{memoHit, memoMissEmpty, memoMissForeign} {
		if census.reads[outcome] == 0 {
			t.Errorf("the observer never reported %s, so any census it produces is blind to "+
				"that arm (%s)", outcome, census)
		}
	}
}
