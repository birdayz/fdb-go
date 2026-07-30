package cascades

import (
	"sync/atomic"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// This file is the MEASUREMENT half of the ordering-value comparators. The two
// comparators — orderingValuesEqual (a requested ordering key against a match
// candidate's) and intersectionValuesEqual (a primary-key intersection's own
// key lists) — decide whether two ordering Values are the same column. Both
// dispatch by TYPE: a pair of plain FieldValues is decided by column identity
// and nothing else, with no fallthrough. The retired alternative dispatched on
// whether the two sides STATE an identity, which let an UNKNOWN-domain
// FieldValue pair fall through to the domain-blind structural arm and was
// intransitive there (values.StatesOrderingColumn documents the witness).
//
// Type dispatch's precondition is a MEASUREMENT, not an argument: every
// FieldValue reaching either site must have STATED an identity, because a
// decline at these sites costs a set-operation merge. The census below is what
// keeps "the decline residual is zero" true, and it is the standing acceptance
// instrument for that claim — not a one-off check that landed with the flip.
//
// So it is counted, on the real corpus, by
// pkg/relational/conformance/explaindiff's ordering-census test — which is where
// the count has to live, because the corpus harness imports this package and a
// test inside it could not import the harness back.
//
// The census also computes what the retired AVAILABILITY dispatch would have
// answered for the same pair. That is the load-bearing comparison: it turns "no
// match was lost" from a claim into a per-pair assertion over the whole corpus.
// A pair where the two dispatches disagree is either a lost merge (the residual
// is not zero after all) or a repaired conflation, and neither may pass
// unexamined.

// OrderingComparisonSite identifies one of the two ordering-value comparators.
type OrderingComparisonSite int

const (
	// OrderingSiteRequestedVsCandidate is orderingValuesEqual: a requested
	// ordering key against a match candidate's ordering key.
	OrderingSiteRequestedVsCandidate OrderingComparisonSite = iota

	// OrderingSiteIntersectionKeys is intersectionValuesEqual: the primary-key
	// intersection's own comparison-key, equality-bound and implicit
	// discriminator lists.
	OrderingSiteIntersectionKeys

	orderingComparisonSiteCount
)

// String names the site for test failure messages.
func (s OrderingComparisonSite) String() string {
	switch s {
	case OrderingSiteRequestedVsCandidate:
		return "orderingValuesEqual (requested vs candidate)"
	case OrderingSiteIntersectionKeys:
		return "intersectionValuesEqual (intersection key lists)"
	default:
		return "unknown ordering comparison site"
	}
}

// OrderingComparisonCensus is one site's decision population.
type OrderingComparisonCensus struct {
	// Total is every comparison the site made.
	Total int64

	// FieldPairs is the comparisons where BOTH operands are plain FieldValues —
	// the type class identity decides.
	FieldPairs int64

	// FieldPairsDecided is the FieldPairs where both operands stated an
	// identity, so SameOrderingColumn had a domain and an ordinal on each side.
	FieldPairsDecided int64

	// DeclineResidual is the FieldPairs where at least one operand stated NO
	// identity. Type dispatch declines these, so this is the count that must be
	// ZERO: a nonzero value is a producer minting an ordering key without the
	// layout its ordinal indexes, and the fix belongs there, not here.
	DeclineResidual int64

	// ResidualWeakerArmsAgreed is the DeclineResidual pairs the retired
	// availability dispatch would ALSO have called unequal — a decline that
	// costs nothing.
	ResidualWeakerArmsAgreed int64

	// ResidualMatchesLost is the DeclineResidual pairs the retired availability
	// dispatch would have called EQUAL. Each one is a merge type dispatch gives
	// up, so this is the count whose zero makes the conversion free.
	ResidualMatchesLost int64

	// NonFieldPairs is the comparisons outside the FieldValue class — the
	// *RecordTypeValue discriminators the intersection carries, arithmetic and
	// function ordering keys, the CardinalityValue wrapper.
	NonFieldPairs int64

	// NonFieldBridgeOnly is the NonFieldPairs the ordinal-free NAME bridge
	// decides EQUAL and structural equality does not. It exists to answer
	// whether the bridge arm is still load-bearing once the FieldValue class no
	// longer reaches it; zero means it is measured dead code.
	NonFieldBridgeOnly int64
}

var (
	orderingCensusEnabled atomic.Bool
	orderingCensus        [orderingComparisonSiteCount]OrderingComparisonCensus
)

// SetOrderingComparisonCensusEnabled turns the census on or off. It is OFF in
// every production path and in every test but the corpus census, which needs
// the counts and is the only reader; when off, each comparison pays one relaxed
// atomic load.
func SetOrderingComparisonCensusEnabled(on bool) { orderingCensusEnabled.Store(on) }

// ResetOrderingComparisonCensus zeroes every site's counts.
//
// It exists because these counters are PACKAGE-scoped, and this package has
// learned what that costs: ReachabilityCollector's header records a corpus tally
// that read 3x its true value because sibling tests planned the same corpus in
// parallel. The same hazard applies here, with one difference that makes the
// package scope tolerable — the assertion built on these counts is a ZERO, and a
// zero over a sum of non-negative terms is exact no matter how many extra
// corpus passes contribute to it. A concurrent pass can only make the assertion
// FAIL, never falsely pass.
//
// The absolute TOTALS have no such protection: they are a lower bound unless the
// census ran alone. Reset before a pass and run that pass with -run isolation if
// the totals themselves are the artifact you need.
func ResetOrderingComparisonCensus() {
	for i := range orderingCensus {
		c := &orderingCensus[i]
		atomic.StoreInt64(&c.Total, 0)
		atomic.StoreInt64(&c.FieldPairs, 0)
		atomic.StoreInt64(&c.FieldPairsDecided, 0)
		atomic.StoreInt64(&c.DeclineResidual, 0)
		atomic.StoreInt64(&c.ResidualWeakerArmsAgreed, 0)
		atomic.StoreInt64(&c.ResidualMatchesLost, 0)
		atomic.StoreInt64(&c.NonFieldPairs, 0)
		atomic.StoreInt64(&c.NonFieldBridgeOnly, 0)
	}
}

// OrderingComparisonCensusOf returns a snapshot of one site's counts.
func OrderingComparisonCensusOf(site OrderingComparisonSite) OrderingComparisonCensus {
	c := &orderingCensus[site]
	return OrderingComparisonCensus{
		Total:                    atomic.LoadInt64(&c.Total),
		FieldPairs:               atomic.LoadInt64(&c.FieldPairs),
		FieldPairsDecided:        atomic.LoadInt64(&c.FieldPairsDecided),
		DeclineResidual:          atomic.LoadInt64(&c.DeclineResidual),
		ResidualWeakerArmsAgreed: atomic.LoadInt64(&c.ResidualWeakerArmsAgreed),
		ResidualMatchesLost:      atomic.LoadInt64(&c.ResidualMatchesLost),
		NonFieldPairs:            atomic.LoadInt64(&c.NonFieldPairs),
		NonFieldBridgeOnly:       atomic.LoadInt64(&c.NonFieldBridgeOnly),
	}
}

// recordOrderingComparison classifies one comparison.
//
// It is deliberately independent of what the calling site DOES with the pair: it
// derives both the type-dispatch and the availability-dispatch answer itself, so
// the census measures the population and the two dispatches' agreement rather
// than restating whichever arm the site happens to run. bridge is the site's own
// weaker arm (the two sites use different bridges), used only to classify the
// non-FieldValue residual.
func recordOrderingComparison(
	site OrderingComparisonSite,
	left, right values.Value,
	bridge func(a, b values.Value) bool,
) {
	if !orderingCensusEnabled.Load() {
		return
	}
	c := &orderingCensus[site]
	atomic.AddInt64(&c.Total, 1)

	if !values.OrderingFieldPair(left, right) {
		atomic.AddInt64(&c.NonFieldPairs, 1)
		if !values.ValuesStructurallyEqual(left, right) && bridge(left, right) {
			atomic.AddInt64(&c.NonFieldBridgeOnly, 1)
		}
		return
	}

	atomic.AddInt64(&c.FieldPairs, 1)
	if values.StatesOrderingColumn(left) && values.StatesOrderingColumn(right) {
		atomic.AddInt64(&c.FieldPairsDecided, 1)
		return
	}

	// The residual: type dispatch declines, so what the retired dispatch would
	// have said is exactly what the conversion costs.
	atomic.AddInt64(&c.DeclineResidual, 1)
	if values.ValuesStructurallyEqual(left, right) || bridge(left, right) {
		atomic.AddInt64(&c.ResidualMatchesLost, 1)
	} else {
		atomic.AddInt64(&c.ResidualWeakerArmsAgreed, 1)
	}
}
