package cascades

import (
	"fmt"
	"io"
	"sync/atomic"
)

// This file is the MEASUREMENT half of ProjectionMergeRule's slot composition,
// and it exists because the entry it retired was WRONG on a probe.
//
// The RFC-197 debt entry for this rule's name-matching arm read "Probed: the arm
// is HEAVILY LIVE -- a panic at its match point reds dozens of FDB tests", and on
// that basis the deletion was held for upstream work that had already landed. The
// panic was real and the reading of it was not.
//
// A PANIC PROBE ANSWERS EXACTLY ONE QUESTION — "is this program point reachable
// at all?" — AND ANSWERS IT ONCE. It cannot answer "how often", and it cannot
// attribute traffic to an arm, because the panic destroys the run that would have
// produced the distribution. (There is no recover() anywhere in this planner:
// OnMatch is invoked bare at expression_matcher.go, implementation_rule.go and
// expression_rule_adapter.go, so the first hit kills the run.)
//
// The mechanism that actually bit here was PLACEMENT AMBIGUITY. A panic at the
// rule's match point reds dozens of tests, which proves the RULE is hot — and the
// rule IS hot, 897 firings over the relational suite. Read as evidence about the
// ARM, the same red says the arm is hot. It is not: the arm took ZERO of those
// 897. Volume blindness is a second, weaker mechanism. Both collapse to one
// failure: a panic converts a distribution question into a boolean, and the
// reader spends the boolean as if it were the distribution.
//
// So the counter is COMMITTED rather than run once and reported. It does double
// duty. It is the evidence for the deletion, and it is the standing guard that
// the arm stays dead: a lazy outer read now DECLINES the merge, and a decline is
// silent. If the resolver's projection-output baking ever regresses, LazyOuterReads
// goes nonzero here — which is a test failure — instead of showing up as an extra
// Projection operator nobody attributes.
//
// The corpus reading lives in pkg/relational/conformance/explaindiff, for the
// same reason the ordering census does: that harness imports this package, so a
// test inside this package cannot import the harness back.

// ProjectionMergeCensus is the population of outer slots ProjectionMergeRule
// examined.
type ProjectionMergeCensus struct {
	// RuleFirings is every time OnMatch found a projection over a projection —
	// counted BEFORE any slot is looked at.
	//
	// It is the DENOMINATOR, and it is counted independently rather than summed
	// from the arms below. A denominator built by adding up its own parts is true
	// by construction and detects nothing; this one can disagree with them, which
	// is the only way it can catch a slot loop that stops running.
	//
	// Its other job is to keep the LazyOuterReads zero from being vacuous. A zero
	// over a population nothing reached reads exactly like a zero over a
	// population that was reached and stayed clean, and only this count separates
	// them.
	RuleFirings int64

	// SlotCompositions is every outer projection slot the rule examined, across
	// all firings — including the ones that made it decline.
	SlotCompositions int64

	// BakedSingleAccessor is the slots that TOOK the ordinal arm: the outer read
	// carried a resolved single-accessor path, so it named the inner's output
	// slot structurally. This is the arm that carries all real traffic.
	//
	// "Took the arm", not "was composed" — the increment precedes the
	// ordinal-range check, so a baked read whose ordinal falls outside the
	// inner's slot list is counted here and then declines. That placement is
	// deliberate: counting after the range check would leave such a slot in no
	// arm at all, and the arms-sum cross-check below is worth more than the
	// finer distinction. The population it names is "reads that arrived with an
	// ordinal", which is exactly the population the LazyOuterReads zero is the
	// complement of.
	BakedSingleAccessor int64

	// LazyOuterReads is the slots where the outer read was a childless FieldValue
	// with NO resolved path — a display name and nothing else.
	//
	// This is the population the retired name-matching arm served, and it MUST be
	// ZERO. The resolver bakes a projection-output reference to its output ordinal
	// before it reaches this rule; a nonzero value means that stopped happening,
	// and the fix belongs there, not here. The rule's own behaviour on such a read
	// is a fail-closed decline either way — it will never select a slot by name
	// again — so a regression costs a lost merge, never a wrong column.
	LazyOuterReads int64

	// DeclinedNotComposable is the slots that were neither: a non-FieldValue, a
	// read with a live child, or a resolved path with more than one accessor.
	//
	// It is counted so the three arms can be checked against SlotCompositions.
	// Without it a slot vanishing from the loop would look like a clean zero.
	DeclinedNotComposable int64
}

var projectionMergeCensus ProjectionMergeCensus

// ResetProjectionMergeCensus zeroes the counts.
//
// The counters are PACKAGE-scoped, so a sibling test planning in parallel adds
// to them. The assertion built on them is a ZERO, and a zero over a sum of
// non-negative terms is exact however many extra passes contribute — a
// concurrent pass can only make it FAIL, never falsely pass. The absolute totals
// have no such protection and are a lower bound unless the pass ran alone.
func ResetProjectionMergeCensus() {
	c := &projectionMergeCensus
	atomic.StoreInt64(&c.RuleFirings, 0)
	atomic.StoreInt64(&c.SlotCompositions, 0)
	atomic.StoreInt64(&c.BakedSingleAccessor, 0)
	atomic.StoreInt64(&c.LazyOuterReads, 0)
	atomic.StoreInt64(&c.DeclinedNotComposable, 0)
}

// ProjectionMergeCensusSnapshot returns the current counts.
func ProjectionMergeCensusSnapshot() ProjectionMergeCensus {
	c := &projectionMergeCensus
	return ProjectionMergeCensus{
		RuleFirings:           atomic.LoadInt64(&c.RuleFirings),
		SlotCompositions:      atomic.LoadInt64(&c.SlotCompositions),
		BakedSingleAccessor:   atomic.LoadInt64(&c.BakedSingleAccessor),
		LazyOuterReads:        atomic.LoadInt64(&c.LazyOuterReads),
		DeclinedNotComposable: atomic.LoadInt64(&c.DeclinedNotComposable),
	}
}

// ProjectionMergeFloors is the population floor for one corpus.
//
// ORDER-OF-MAGNITUDE below the measurement, like every other per-site floor on
// this path: it exists to catch the rule going dark, not to re-bless a count
// that moves whenever a test file is added. The load-bearing assertion is the
// LazyOuterReads ZERO, which is not configurable and is checked unconditionally.
type ProjectionMergeFloors struct {
	RuleFirings         int64
	BakedSingleAccessor int64
}

// FormatProjectionMergeCensus renders the counts for a corpus report.
func FormatProjectionMergeCensus() string {
	c := ProjectionMergeCensusSnapshot()
	return fmt.Sprintf("projection-merge census: firings=%d slots=%d baked=%d lazy=%d notComposable=%d",
		c.RuleFirings, c.SlotCompositions, c.BakedSingleAccessor,
		c.LazyOuterReads, c.DeclinedNotComposable)
}

// AssertProjectionMergeCensus checks the claim the name-arm deletion rests on.
// It reports whether anything failed.
//
// The ZERO is the claim; the floors and the partition check are what keep the
// zero from being vacuous. Both are needed, and in this order: a zero over a
// population nothing reached, and a zero produced by a counter that stopped
// being wired, are indistinguishable in a printed report.
func AssertProjectionMergeCensus(w io.Writer, floors *ProjectionMergeFloors) bool {
	c := ProjectionMergeCensusSnapshot()
	failed := false

	if floors != nil {
		if c.RuleFirings < floors.RuleFirings {
			fmt.Fprintf(w, "projection-merge census: RuleFirings=%d below floor %d.\n"+
				"The rule has gone dark over this corpus, so the LazyOuterReads zero "+
				"below it is vacuous.\n", c.RuleFirings, floors.RuleFirings)
			failed = true
		}
		if c.BakedSingleAccessor < floors.BakedSingleAccessor {
			fmt.Fprintf(w, "projection-merge census: BakedSingleAccessor=%d below floor %d.\n"+
				"The ordinal arm is the only composing arm left; a corpus that stops "+
				"taking it cannot testify about whether the removed NAME arm was needed, "+
				"because it never merged a projection at all.\n",
				c.BakedSingleAccessor, floors.BakedSingleAccessor)
			failed = true
		}
	}

	if c.LazyOuterReads != 0 {
		fmt.Fprintf(w, "projection-merge census: %d LAZY outer read(s), want 0.\n"+
			"A lazy outer read is a childless FieldValue with no resolved path — a "+
			"display name and nothing else. The resolver bakes a projection-output "+
			"reference to its output ordinal before it reaches ProjectionMergeRule, "+
			"and this count going nonzero means it stopped. Nothing is silently "+
			"wrong: the rule DECLINES such a read, so the cost is a lost merge (an "+
			"extra Projection operator), never a wrong column. The fix belongs at "+
			"the resolver — restoring a name-matching arm at the rule is the RFC-197 "+
			"defect, not its repair.\n", c.LazyOuterReads)
		failed = true
	}

	if sum := c.BakedSingleAccessor + c.LazyOuterReads + c.DeclinedNotComposable; sum != c.SlotCompositions {
		fmt.Fprintf(w, "projection-merge census: arms sum to %d but %d slots were examined.\n"+
			"An unclassified slot is a hole in the instrument: the lazy-read zero is "+
			"only meaningful if every slot lands in exactly one arm.\n",
			sum, c.SlotCompositions)
		failed = true
	}
	return failed
}

// The recorders are unconditional — no enable gate. Each is one relaxed atomic
// add on a path that already allocates a slice per firing, and the gate itself
// would cost a load. The ordering census is gated because it builds a comparison
// CONTEXT it would otherwise allocate per comparison; there is nothing to build
// here.
func recordProjectionMergeFiring() { atomic.AddInt64(&projectionMergeCensus.RuleFirings, 1) }
func recordProjectionMergeSlot()   { atomic.AddInt64(&projectionMergeCensus.SlotCompositions, 1) }
func recordProjectionMergeBaked()  { atomic.AddInt64(&projectionMergeCensus.BakedSingleAccessor, 1) }
func recordProjectionMergeNotComposable() {
	atomic.AddInt64(&projectionMergeCensus.DeclinedNotComposable, 1)
}
