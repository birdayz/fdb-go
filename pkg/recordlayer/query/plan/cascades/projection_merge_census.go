package cascades

import "sync/atomic"

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

	// BakedSingleAccessor is the slots composed by ORDINAL: the outer read
	// carried a resolved single-accessor path, so it named the inner's output
	// slot structurally. This is the arm that carries all real traffic.
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

// The recorders are unconditional — no enable gate. Each is one relaxed atomic
// add on a path that already allocates a slice per firing, and the gate itself
// would cost a load. The ordering census is gated because it builds a comparison
// CONTEXT it would otherwise allocate per comparison; there is nothing to build
// here.
func recordProjectionMergeFiring()   { atomic.AddInt64(&projectionMergeCensus.RuleFirings, 1) }
func recordProjectionMergeSlot()     { atomic.AddInt64(&projectionMergeCensus.SlotCompositions, 1) }
func recordProjectionMergeBaked()    { atomic.AddInt64(&projectionMergeCensus.BakedSingleAccessor, 1) }
func recordProjectionMergeLazyRead() { atomic.AddInt64(&projectionMergeCensus.LazyOuterReads, 1) }
func recordProjectionMergeNotComposable() {
	atomic.AddInt64(&projectionMergeCensus.DeclinedNotComposable, 1)
}
