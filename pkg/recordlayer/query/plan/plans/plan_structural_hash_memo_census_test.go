package plans

import (
	"fmt"
	"sync"
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
// # What the hit COUNT can and cannot tell you
//
// It pins the WORKLOAD's shape. It does not, on its own, prove the memo was
// consulted, and an earlier version of this file claimed that it did.
//
// The claim was disproved by mutation: remove the memo read from
// `HashCodeWithoutChildren` AND make `storeStructuralHash` decline whenever the cell
// is already populated, and every test here still passes against a fully dark memo.
// The second half of that mutation is behaviour-preserving on its own — with the read
// intact, a hit returns before the store is reached and a foreign miss declines
// anyway — so it is one real refactor plus one dropped read, not a rigged pair.
//
// The reason is that `observe` infers "this was a hit" from `before.owner == plan`,
// which is the same cell state, compared the same way, that the memo itself consults.
// Observer and subject share a derivation route, so the classification can never
// disagree with the code it audits — CLAUDE.md's paired-assertion vacuity, exactly.
// Worse, the hit arm's `got == before.hash` check is vacuous by construction: a
// recompute is deterministic and returns the byte-identical hash, so the assertion
// holds whether or not the cached value was ever read.
//
// TestAMemoizedReadIsServedFromTheCell below is the half that actually witnesses the
// read. It plants a value in the cell that a recompute CANNOT produce and requires
// that value to come back.
//
// The division of labour across this file, since no single test covers the cell:
//
//   - the census above pins the write-side CONTRACT (classification total, store
//     decision determined by it);
//   - TestAMemoizedReadIsServedFromTheCell pins that the cache is READ at all;
//   - TestMemoIsCorrectUnderConcurrentSharers pins that concurrent sharers each get
//     their OWN plan's hash — answer correctness, which stays green against a dark
//     memo and is not a liveness test;
//   - TestOnlyOneSharerWinsAnEmptyCell pins the CompareAndSwap, which every
//     answer-correctness test here is structurally unable to see.

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
		// Check the VALUE, not just that the cell was left alone. Asserting only
		// `after == before` holds no matter what was returned, so deleting the
		// read-side owner check — which makes a copy answer with the ORIGINAL's
		// hash, a wrong identity — left this arm green. Callers arrange for the
		// sharers to differ structurally, so borrowing the cell's hash is
		// detectable here.
		if got == before.hash {
			t.Errorf("a plan that does not own the cell returned the cell's hash (%d); the "+
				"read-side owner check did not fire and these two plans now intern as one",
				got)
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

// TestAMemoizedReadIsServedFromTheCell is the only test here that witnesses the READ.
//
// Every other assertion in this file observes the cell after the call and therefore
// reports on the write side. That leaves the memo's entire reason for existing
// unpinned: a `HashCodeWithoutChildren` that ignored the cache and recomputed every
// time would satisfy all of them, because a recompute returns the same bytes the cache
// held.
//
// The witness is a value the recompute cannot produce. Plant a hash under the plan's
// OWN ownership — so the read must classify it as a hit — and require it back. The
// planted value is deliberately not a real structural hash; if it is returned, the
// answer came from the cell, and if the real hash is returned instead, the read never
// happened.
//
// This costs nothing in production and is the difference between "the memo is
// consistent" and "the memo is used".
func TestAMemoizedReadIsServedFromTheCell(t *testing.T) {
	t.Parallel()

	plan := memoTestIndexPlan(t)

	// Establish what an honest recompute yields, so the poison can be chosen to
	// differ from it and the final comparison cannot pass by coincidence.
	real := plan.HashCodeWithoutChildren()
	const poison = uint64(0xD15EA5E0D15EA5E0)
	if real == poison {
		t.Fatalf("the poison value collides with the genuine hash %d; pick another", real)
	}

	plan.hashMemo.state.Store(&hashMemoState{owner: any(plan), hash: poison})

	got := plan.HashCodeWithoutChildren()
	if got == real {
		t.Error("HashCodeWithoutChildren recomputed instead of reading the cell it owns: " +
			"the memo is DARK. Every other test in this file passes in that state, because " +
			"they observe the cell after the call and a recompute agrees with the cache on " +
			"every value it could be checked against.")
	}
	if got != poison {
		t.Errorf("read returned %d, planted %d", got, poison)
	}

	// And the read must not have disturbed what it read.
	if state := plan.hashMemo.state.Load(); state == nil || state.hash != poison {
		t.Error("serving a hit rewrote the cell")
	}
}

// TestMemoIsCorrectUnderConcurrentSharers exercises the dimension the atomic exists
// for, and which nothing else here touched: several goroutines hashing plans that
// SHARE one cell, at once.
//
// The cell is `atomic.Pointer` rather than two plain fields precisely because a
// reader must never see one plan's hash paired with another plan's identity — that
// pairing is a wrong ANSWER, not a wasted recompute. Every other test in this file
// runs single-threaded, so the guarantee was argued and never observed. Under
// `-race` this is also the only test that can surface a torn read of the cell.
//
// What must hold no matter the interleaving: every caller gets the hash of the plan
// it asked about, and the original never has its entry replaced by a sharer's.
func TestMemoIsCorrectUnderConcurrentSharers(t *testing.T) {
	t.Parallel()

	original := memoTestIndexPlan(t)
	want := original.structuralKey().Hash("indexplan|")

	variant := original.WithScanComparisons([]*predicates.ComparisonRange{
		scanCostRange(t, predicates.ComparisonEquals, int64(77)),
	})
	if variant.hashMemo != original.hashMemo {
		t.Fatal("the copy got its own cell, so this test is not exercising sharing")
	}
	variantWant := variant.structuralKey().Hash("indexplan|")
	if variantWant == want {
		t.Fatal("the two plans hash equal, so a swapped answer would be undetectable")
	}

	const goroutines, iterations = 8, 200
	var wg sync.WaitGroup
	errs := make(chan string, goroutines*2)
	for g := 0; g < goroutines; g++ {
		for _, tc := range []struct {
			plan *RecordQueryIndexPlan
			want uint64
			name string
		}{{original, want, "original"}, {variant, variantWant, "copy"}} {
			wg.Add(1)
			go func(plan *RecordQueryIndexPlan, want uint64, name string) {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					if got := plan.HashCodeWithoutChildren(); got != want {
						select {
						case errs <- fmt.Sprintf("%s got %d, want %d — a sharer was served "+
							"another plan's hash", name, got, want):
						default:
						}
						return
					}
				}
			}(tc.plan, tc.want, tc.name)
		}
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}

	// Whoever won the initial race, the cell must belong to exactly one of them and
	// hold that one's hash — never a mismatched pair.
	state := original.hashMemo.state.Load()
	if state == nil {
		t.Fatal("the cell is empty after 3200 reads")
	}
	switch state.owner {
	case any(original):
		if state.hash != want {
			t.Errorf("cell is owned by the original but holds %d, not %d", state.hash, want)
		}
	case any(variant):
		if state.hash != variantWant {
			t.Errorf("cell is owned by the copy but holds %d, not %d", state.hash, variantWant)
		}
	default:
		t.Errorf("cell is owned by neither sharer: %v", state.owner)
	}
}

// TestOnlyOneSharerWinsAnEmptyCell pins the CompareAndSwap, which every
// answer-correctness test in this file is structurally unable to see.
//
// Under a plain Store the memo still returns the right hash to every caller — reads
// validate the owner either way — so the two forms differ ONLY in how many stores
// survive a race on an empty cell. Reverting the CAS to a Store therefore leaves the
// whole package green, including the concurrency test, which accepts either sharer
// as the eventual owner. That made the CAS a real fix with no sentinel.
//
// The observable that separates them is the number of stores that report success.
// N sharers of one empty cell all pass the owner check (the cell is nil, so nobody
// is foreign), so all N attempt a write. A CAS lets exactly one land: every loser
// compares against a `nil` that is no longer current. A plain Store lets each of
// them overwrite the last, so more than one reports success whenever the race lands
// — and the loser's owner then holds a cell the winner will keep evicting.
func TestOnlyOneSharerWinsAnEmptyCell(t *testing.T) {
	t.Parallel()

	original := memoTestIndexPlan(t)

	// Sharers with DISTINCT owners, all reaching the same still-empty cell.
	sharers := []*RecordQueryIndexPlan{original}
	for i := 0; i < 3; i++ {
		variant := original.WithScanComparisons([]*predicates.ComparisonRange{
			scanCostRange(t, predicates.ComparisonEquals, int64(500+i)),
		})
		if variant.hashMemo != original.hashMemo {
			t.Fatal("the copy got its own cell; this test needs sharers")
		}
		sharers = append(sharers, variant)
	}

	cell := original.hashMemo
	observed := cell.state.Load()
	if observed != nil {
		t.Fatal("the cell is already populated, so there is no empty-cell transition to test")
	}

	// Every sharer commits against the state it observed — all of them nil, which is
	// exactly what happens when their loads interleave ahead of the first store. This
	// is driven directly rather than raced for: through storeStructuralHash the
	// overlap is rare, because whichever goroutine arrives first claims the cell and
	// the rest decline as foreign, so a concurrent version of this test passes under a
	// plain Store nearly every time. Committing against a fixed `prev` makes the
	// contended transition reproducible.
	won := 0
	for _, plan := range sharers {
		if cell.commit(observed, plan, plan.structuralKey().Hash("indexplan|")) {
			won++
		}
	}

	if won != 1 {
		t.Fatalf("%d of %d sharers committed successfully against the same observed empty "+
			"cell, want exactly 1. More than one means a store overwrote a live entry, so "+
			"the evicted owner misses its own memo and stores back — the ping-pong the "+
			"conditional store is documented to prevent", won, len(sharers))
	}

	// The single winner's entry must be intact and self-consistent.
	state := cell.state.Load()
	if state == nil {
		t.Fatal("no entry survived")
	}
	winner, ok := state.owner.(*RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("cell owner is %T, not a plan", state.owner)
	}
	if want := winner.structuralKey().Hash("indexplan|"); state.hash != want {
		t.Errorf("cell holds hash %d under an owner whose hash is %d", state.hash, want)
	}
}

// TestCommitRefusesAForeignOwnedPrev pins the guard inside commit, which is what
// makes the primitive safe rather than merely used safely.
//
// It closes a refactor that keeps a genuine CompareAndSwap and defeats every other
// test here: having storeStructuralHash re-load rather than pass the state its owner
// check was based on. A sharer whose check saw an EMPTY cell would then swap over a
// cell another sharer had since claimed — the exact eviction the CAS prevents,
// reintroduced by an edit that reads like a tidy-up.
//
// The guard is deliberately not a no-op for the deterministic winner test either:
// there all four sharers commit against a nil prev, so it does not fire, and
// TestOnlyOneSharerWinsAnEmptyCell keeps its teeth. The two properties are
// orthogonal, which is why one does not mask the other.
func TestCommitRefusesAForeignOwnedPrev(t *testing.T) {
	t.Parallel()

	owner := memoTestIndexPlan(t)
	foreign := memoTestIndexPlan(t)
	cell := owner.hashMemo

	// The cell belongs to `foreign`.
	foreignHash := foreign.structuralKey().Hash("indexplan|")
	if !cell.commit(nil, foreign, foreignHash) {
		t.Fatal("could not seed the cell")
	}
	seeded := cell.state.Load()

	// `owner` commits against the state it just read — observed, but belonging to
	// someone else. Under a bare CAS this succeeds and evicts `foreign`.
	if cell.commit(seeded, owner, owner.structuralKey().Hash("indexplan|")) {
		t.Error("commit accepted a prev owned by another plan; a caller that observed a " +
			"foreign-owned cell can now swap over it, which is the eviction the " +
			"conditional store exists to prevent")
	}

	state := cell.state.Load()
	if state == nil || state.owner != any(foreign) || state.hash != foreignHash {
		t.Errorf("the foreign owner lost its entry: %+v", state)
	}

	// And the owner may still claim a cell that is genuinely its own.
	if !cell.commit(state, foreign, foreignHash) {
		t.Error("commit refused the cell's own owner")
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
