package dst

import "testing"

// pinActivation forces site's cached activation decision, bypassing the seeded draw, so a test
// can isolate the firing gate from the activation gate.
func pinActivation(b *Buggifier, site string, active bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.siteLocked("fault:" + site).activated = active
}

// schedule records the fire/no-fire sequence a site produces over n hits.
func schedule(b *Buggifier, site string, n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = b.Buggify(site)
	}
	return out
}

func allEqual(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuggifier_NewSiteDoesNotShiftAnotherSitesSchedule is the reproducibility pin.
//
// With one shared generator, every draw comes off the same sequence, so a site's schedule is a
// function of how many draws happened before it — i.e. of which OTHER sites exist and how often
// they were hit. Adding a fault point anywhere in the codebase then silently re-phases every
// other site, and "seed N reproduces the bug" stops being true across commits. Per-site derived
// generators make a site's schedule a function of (seed, its own label, its own hit count)
// only.
func TestBuggifier_NewSiteDoesNotShiftAnotherSitesSchedule(t *testing.T) {
	t.Parallel()
	const n = 64

	// Baseline: only "victim" is ever hit.
	base := schedule(NewBuggifier(7, true), "victim", n)

	// A newly added, unrelated fault point is hit before and between every victim hit.
	b := NewBuggifier(7, true)
	var withNeighbour []bool
	for i := 0; i < n; i++ {
		b.Buggify("brand.new.site")
		b.BuggifyWithProb("another.new.site", 0.9)
		withNeighbour = append(withNeighbour, b.Buggify("victim"))
	}

	if !allEqual(base, withNeighbour) {
		t.Fatalf("adding unrelated fault sites shifted victim's schedule:\n base=%v\n with=%v", base, withNeighbour)
	}
}

// TestBuggifier_CoinDoesNotShiftFaultSchedule pins the other direction: a coin flip is a
// modelling choice, not a fault, and must not re-phase the fault schedule (nor the reverse).
func TestBuggifier_CoinDoesNotShiftFaultSchedule(t *testing.T) {
	t.Parallel()
	const n = 64
	base := schedule(NewBuggifier(11, true), "commit", n)

	b := NewBuggifier(11, true)
	var withCoins []bool
	for i := 0; i < n; i++ {
		b.Coin("commit") // same LABEL as the fault site: still a separate stream
		b.Coin("some.other.decision")
		withCoins = append(withCoins, b.Buggify("commit"))
	}
	if !allEqual(base, withCoins) {
		t.Fatalf("coin flips shifted the fault schedule:\n base=%v\n with=%v", base, withCoins)
	}

	// And the coin sequence itself is unaffected by interleaved faults.
	coinsAlone := make([]bool, n)
	ca := NewBuggifier(11, true)
	for i := range coinsAlone {
		coinsAlone[i] = ca.Coin("commit")
	}
	cb := NewBuggifier(11, true)
	coinsWithFaults := make([]bool, n)
	for i := range coinsWithFaults {
		cb.Buggify("noise")
		cb.Buggify("commit")
		coinsWithFaults[i] = cb.Coin("commit")
	}
	if !allEqual(coinsAlone, coinsWithFaults) {
		t.Fatalf("fault draws shifted the coin sequence:\n alone=%v\n with=%v", coinsAlone, coinsWithFaults)
	}
}

// TestBuggifier_CoinIsDeterministicSeedDependentAndFair pins the coin's three properties: same
// seed → same sequence, different seed → different sequence, and both branches are reachable
// (a coin stuck on one branch would silently make the two-branch commit_unknown model
// one-branch again).
func TestBuggifier_CoinIsDeterministicSeedDependentAndFair(t *testing.T) {
	t.Parallel()
	draw := func(seed uint64, n int) []bool {
		b := NewBuggifier(seed, true)
		out := make([]bool, n)
		for i := range out {
			out[i] = b.Coin("simfdb.commit.unknown.applied")
		}
		return out
	}
	a, b := draw(21, 200), draw(21, 200)
	if !allEqual(a, b) {
		t.Fatal("same seed produced different coin sequences")
	}
	if allEqual(a, draw(22, 200)) {
		t.Fatal("different seeds produced an identical coin sequence")
	}
	var heads int
	for _, v := range a {
		if v {
			heads++
		}
	}
	if heads == 0 || heads == len(a) {
		t.Fatalf("degenerate coin: %d heads out of %d", heads, len(a))
	}
	// A coin is fair, unlike a 0.25 fault gate. 200 fair flips outside [60,140] is a
	// ~1e-8 event, so this discriminates a coin from a fire-probability roll.
	if heads < 60 || heads > 140 {
		t.Fatalf("coin is not fair: %d heads out of %d", heads, len(a))
	}
}

// TestBuggifier_CoinIgnoresEnabledFlag pins that a coin resolves a modelling choice even in a
// run with fault injection switched off — the continuation/fault harnesses run with
// DisabledBuggifier and still need commit_unknown to pick a branch.
func TestBuggifier_CoinIgnoresEnabledFlag(t *testing.T) {
	t.Parallel()
	on := NewBuggifier(31, true)
	off := NewBuggifier(31, false)
	for i := 0; i < 100; i++ {
		if on.Coin("x") != off.Coin("x") {
			t.Fatalf("coin at hit %d differed between enabled and disabled Buggifiers", i)
		}
	}
	if off.Fired() != 0 {
		t.Fatalf("coins counted as fault firings: %d", off.Fired())
	}
	// A nil Buggifier has no seed and cannot flip.
	var nilB *Buggifier
	if nilB.Coin("x") {
		t.Fatal("nil Buggifier flipped heads")
	}
}

// TestSiteHash_StableAcrossCalls pins that site labels map to stream selectors via a stable
// hash. Go's built-in map/string hashing is per-process randomized; using it would make a
// seed reproduce only within one process, defeating the point.
func TestSiteHash_StableAcrossCalls(t *testing.T) {
	t.Parallel()
	// Known FNV-1a 64 values — a change to the hash function changes every seed's schedule,
	// which is a decision to make deliberately, not by accident.
	cases := map[string]uint64{
		"":                       0xcbf29ce484222325,
		"a":                      0xaf63dc4c8601ec8c,
		"simfdb.commit.conflict": 0x1dfa9041d6e62e75,
	}
	for in, want := range cases {
		if got := siteHash(in); got != want {
			t.Errorf("siteHash(%q) = %#x, want %#x", in, got, want)
		}
	}
	if siteHash("fault:x") == siteHash("coin:x") {
		t.Fatal("fault and coin streams for the same label collide")
	}
}
