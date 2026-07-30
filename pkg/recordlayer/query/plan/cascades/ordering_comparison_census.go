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

	// RootWildcardBridges is the FieldPairs decided EQUAL whose two operands have
	// DIFFERENT roots — so the match rested on values.SameOrderingColumn treating
	// the ZERO correlation as a wildcard (a childless source-relative key against
	// a key read off a named quantifier).
	//
	// This is not a defect count. The bridge is load-bearing: a match candidate
	// mints its ordering keys childless while a request scoped to its owning
	// quantifier does not, so declining it would lose every such match. It is the
	// DENOMINATOR for the two counts below.
	RootWildcardBridges int64

	// RootWildcardNoContext is the RootWildcardBridges whose call site supplied
	// no comparison context, so the ambiguity below could not be evaluated for
	// them.
	//
	// It must be ZERO, and for the same reason Total must be nonzero: a zero
	// ambiguity count over a population the instrument never looked at is a
	// vacuous zero. A nonzero value here means a comparator call site was added
	// without threading the list it is scanning.
	RootWildcardNoContext int64

	// RootWildcardContextRooted is the RootWildcardBridges whose comparison
	// context holds ANY key read off a named quantifier — whether or not that
	// quantifier differs from the one the bridge matched.
	//
	// It exists to make the two counts below INTERPRETABLE rather than merely
	// zero. A zero MultiRoot means one of two very different things: the contexts
	// hold exactly one quantifier root (the structural claim — a match candidate's
	// ordering parts all come off one candidate), or the contexts hold NO
	// quantifier root at all (the weaker claim — the lists scanned are the
	// candidate's own uniformly childless keys, so the wildcard's second root
	// could not appear there whatever the query). Reporting only a zero would
	// leave the reader unable to tell which, and a zero whose reason is unknown is
	// not evidence.
	RootWildcardContextRooted int64

	// RootWildcardMultiRoot is the RootWildcardBridges whose comparison context
	// (the ordering set / partition key list being scanned) holds a SECOND
	// distinct named quantifier root, besides the one the bridge matched.
	//
	// This is the population where the wildcard COULD be intransitive, counted
	// deliberately broad — it does not check that the second root's key is even
	// the same column. Over-counting is the fail-loud direction: a zero here is a
	// proof of absence, and a nonzero value is a prompt to look at the exact
	// count below rather than a verdict on its own.
	RootWildcardMultiRoot int64

	// RootWildcardIntransitive is the RootWildcardMultiRoot where that second
	// root's key shares the childless operand's column PATH — so the triple
	// (childless ≡ o.A, childless ≡ i.A, o.A ≢ i.A) is actually present in one
	// list, and membership in that list depends on the order it was built in.
	//
	// This is the exact hazard. It must be ZERO. A nonzero value is not a lost
	// merge, it is a nondeterministic plan, and the fix is CQ-55-A2's
	// correlation-space translation: give the childless root the quantifier it
	// actually reads, so the wildcard has nothing left to bridge.
	// ordering_comparator_dispatch_test.go's root-axis witness pins the
	// comparator behaviour this counts.
	RootWildcardIntransitive int64
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
		atomic.StoreInt64(&c.RootWildcardBridges, 0)
		atomic.StoreInt64(&c.RootWildcardNoContext, 0)
		atomic.StoreInt64(&c.RootWildcardContextRooted, 0)
		atomic.StoreInt64(&c.RootWildcardMultiRoot, 0)
		atomic.StoreInt64(&c.RootWildcardIntransitive, 0)
	}
}

// OrderingComparisonCensusOf returns a snapshot of one site's counts.
func OrderingComparisonCensusOf(site OrderingComparisonSite) OrderingComparisonCensus {
	c := &orderingCensus[site]
	return OrderingComparisonCensus{
		Total:                     atomic.LoadInt64(&c.Total),
		FieldPairs:                atomic.LoadInt64(&c.FieldPairs),
		FieldPairsDecided:         atomic.LoadInt64(&c.FieldPairsDecided),
		DeclineResidual:           atomic.LoadInt64(&c.DeclineResidual),
		ResidualWeakerArmsAgreed:  atomic.LoadInt64(&c.ResidualWeakerArmsAgreed),
		ResidualMatchesLost:       atomic.LoadInt64(&c.ResidualMatchesLost),
		NonFieldPairs:             atomic.LoadInt64(&c.NonFieldPairs),
		NonFieldBridgeOnly:        atomic.LoadInt64(&c.NonFieldBridgeOnly),
		RootWildcardBridges:       atomic.LoadInt64(&c.RootWildcardBridges),
		RootWildcardNoContext:     atomic.LoadInt64(&c.RootWildcardNoContext),
		RootWildcardContextRooted: atomic.LoadInt64(&c.RootWildcardContextRooted),
		RootWildcardMultiRoot:     atomic.LoadInt64(&c.RootWildcardMultiRoot),
		RootWildcardIntransitive:  atomic.LoadInt64(&c.RootWildcardIntransitive),
	}
}

// orderingComparisonCensusOn reports whether the census is counting. Call sites
// use it to skip building a comparison context they would otherwise allocate on
// every production planning pass.
func orderingComparisonCensusOn() bool { return orderingCensusEnabled.Load() }

// recordOrderingComparison classifies one comparison.
//
// It is deliberately independent of what the calling site DOES with the pair: it
// derives both the type-dispatch and the availability-dispatch answer itself, so
// the census measures the population and the two dispatches' agreement rather
// than restating whichever arm the site happens to run. bridge is the site's own
// weaker arm (the two sites use different bridges), used only to classify the
// non-FieldValue residual.
//
// context is the LIST the call site is scanning — the requested/provided ordering
// set, or the intersection's partition key list. It is the unit the root-wildcard
// ambiguity is defined over: an intransitive triple only costs anything when all
// three of its members sit in ONE list, because that is what makes membership
// depend on the order the list was built in. A site that supplies none is counted
// separately rather than silently treated as unambiguous.
func recordOrderingComparison(
	site OrderingComparisonSite,
	left, right values.Value,
	bridge func(a, b values.Value) bool,
	context []values.Value,
) {
	if !orderingCensusEnabled.Load() {
		return
	}
	classifyOrderingComparison(&orderingCensus[site], left, right, bridge, context)
}

// classifyOrderingComparison is recordOrderingComparison's body over an explicit
// census, so the classification can be exercised against a caller-owned struct
// instead of the package counters.
//
// That separation is what makes the instrument testable at all. The counters are
// package-scoped and the corpus assertion built on them is a ZERO, which is exact
// under concurrent passes; a test asserting a NONZERO count has no such
// protection, and every test in this package must be safe to run in parallel. So
// the reachability pins — the ones that keep a zero from being a broken detector —
// pass their own census here and touch no global.
func classifyOrderingComparison(
	c *OrderingComparisonCensus,
	left, right values.Value,
	bridge func(a, b values.Value) bool,
	context []values.Value,
) {
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
		recordRootWildcard(c, left, right, context)
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

// recordRootWildcard classifies the ROOT decision of a pair the identity arm
// decided EQUAL.
//
// The domain axis of values.SameOrderingColumn is an equivalence relation; the
// ROOT axis is not, because the zero correlation is a wildcard. This is what
// counts the population where that costs something. It runs only on pairs that
// landed in FieldPairsDecided — which is exactly why it exists: those pairs are
// invisible in that bucket, and the domain-axis instrument therefore cannot see
// this hazard at all.
func recordRootWildcard(
	c *OrderingComparisonCensus,
	left, right values.Value,
	context []values.Value,
) {
	if !values.SameOrderingColumn(left, right) {
		return
	}
	leftRoot, leftOK := values.OrderingRootCorrelationOf(left)
	rightRoot, rightOK := values.OrderingRootCorrelationOf(right)
	if !leftOK || !rightOK || leftRoot == rightRoot {
		return
	}

	// The roots differ and the pair still matched, so SameOrderingColumn's
	// wildcard fired: exactly one side is zero-rooted.
	childless, matchedRoot := left, rightRoot
	if rightRoot.IsZero() {
		childless, matchedRoot = right, leftRoot
	}
	atomic.AddInt64(&c.RootWildcardBridges, 1)

	if len(context) == 0 {
		atomic.AddInt64(&c.RootWildcardNoContext, 1)
		return
	}

	childlessField, isField := childless.(*values.FieldValue)
	if !isField {
		return
	}

	contextRooted, multiRoot, intransitive := false, false, false
	for _, other := range context {
		otherRoot, ok := values.OrderingRootCorrelationOf(other)
		if !ok || otherRoot.IsZero() {
			continue
		}
		contextRooted = true
		if otherRoot == matchedRoot {
			continue
		}
		multiRoot = true
		otherField, ok := other.(*values.FieldValue)
		if ok && values.SameColumnPath(childlessField.Resolved, otherField.Resolved) {
			intransitive = true
			break
		}
	}
	if contextRooted {
		atomic.AddInt64(&c.RootWildcardContextRooted, 1)
	}
	if multiRoot {
		atomic.AddInt64(&c.RootWildcardMultiRoot, 1)
	}
	if intransitive {
		atomic.AddInt64(&c.RootWildcardIntransitive, 1)
	}
}
