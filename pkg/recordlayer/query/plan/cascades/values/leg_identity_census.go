package values

import (
	"strings"
	"sync"
	"sync/atomic"
)

// This file is the MEASUREMENT half of the leg-identity comparison sites — the
// places that decide "does this correlation name that leg window?" and therefore
// which row's slots a value reads. A wrong answer there is a wrong-rows plan, not
// a lost optimization.
//
// RecordTypeLeg used to carry leg identity as TEXT (RecordTypeLeg.Name) alone,
// and the sites that consumed it did NOT agree on how to compare it: two compared
// exactly against the counterparty correlation's own spelling, two upper-FOLDED
// one side first. RecordTypeLeg now also carries Alias, and every reader whose
// counterparty is a correlation goes through values.SameLeg, which is EXACT (see
// its comment: alias namespaces here are deliberately case-DISJOINT, so a fold
// lets a quoted user alias forge a planner-minted q$N leg).
//
// The census outlives that conversion. Its standing job is to keep proving the
// premise the conversion rested on — that a leg's text and its identity name the
// same thing everywhere — so a producer that starts storing them apart is loud
// instead of silently rebinding rows.
//
// Converting a site's comparison is a BEHAVIOR CHANGE for any pair the two
// predicates decide differently. Whether such a pair occurs in production traffic
// is a measurement, not an argument, and this census is what makes it one. The
// population that settles it is RetiredVerdictDivergent — the pairs on which the
// retired predicate and the shipped one disagree. Its zero is what makes a
// conversion free; a nonzero value is either a producer to reconcile or a fix that
// owes a test.
//
// FoldOnlyEqual remains as the diagnostic beneath it: a case-ONLY difference is
// the forgery shape, since the machine namespace is lowercase and folding a minted
// q$N yields the Q$N that SameLeg exists to keep out. But fold-only is NOT
// sufficient on its own — see RetiredVerdictDivergent for the flip direction it
// structurally cannot see.
//
// The counts live here, in values, because the consuming sites span three
// packages (executor, cascades, values) and values is the one they all import.
// The corpus pass that reads them lives in the conformance harness, which can
// import this package while a test in here could not import the harness back —
// the same containment the ordering-comparison census settled on.
//
// AN INSTRUMENT MUST RECORD THE PAIR ITS SITE ACTUALLY EVALUATES. The first form
// of this census took two STRINGS, so a site that had been converted to compare
// identities could only report its leg's TEXT against the counterparty's text —
// and would score ExactEqual for a pair the shipped comparison DECLINES. A leg
// whose Name is the UPPER fold of its own minted lowercase identity reads as
// ("Q$5","Q$5") in the text channel while the comparison evaluates ("q$5","Q$5")
// and returns false. That is not a weak measurement; it is a measurement of a
// different program, and it is why one site's conversion could be argued about
// from numbers that described neither side of it.
//
// (What the corrected instrument then found, stated because the correction is
// worth less than the finding: on the real-FDB corpus NO converted site has such
// a pair. The only ones anywhere are the deliberate forgeries in the translator
// package's own fixtures, where the decline is the assertion.)
//
// Hence three channels, and a structural guard that a site uses exactly one
// namespace:
//
//   - RecordLegIdentityPair takes two CorrelationIdentifiers and classifies them
//     the way SameLeg decides;
//   - RecordLegIdentityConversion adds the retired predicate's verdict — what a
//     converted site must record, because the pair alone cannot show a flip;
//   - RecordLegIdentityComparison takes two strings, for the divergence censuses
//     that pair a leg's own two spellings and for any comparison that genuinely
//     IS text.
//
// legCensusChannel records which namespace a site used and reports MIXED if a site
// ever uses both, so the claim "this site's numbers describe its comparison" is
// checked rather than asserted in a comment.

// LegIdentitySite identifies one leg-identity comparison site.
type LegIdentitySite int

const (
	// LegSiteRowLegsBinder is the RUNTIME leg binder
	// (executor.rowLegsBinder.GetCorrelationBinding): it resolves a correlation
	// to its window within a merged row's own Type.Legs. Compares EXACTLY today.
	LegSiteRowLegsBinder LegIdentitySite = iota

	// LegSiteBuriedLegWindow is executor.buriedLegWindow: the sub-window of a
	// buried leg inside a clustered box span. Compares EXACTLY today.
	LegSiteBuriedLegWindow

	// LegSiteTextVsIdentity is not a comparison site but the DIVERGENCE census
	// between a leg's two spellings: its recorded pair is (RecordTypeLeg.Name, the
	// same leg's Alias.Name()), sampled at every reader that walks a leg.
	//
	// This is the count that decides whether retyping the readers from Name to
	// Alias is a behaviour change at all. Name and Alias are recorded
	// independently by the producers — several of which normalize one and not the
	// other (a window map keyed by an UPPER fold whose window carries the raw
	// correlation; a select whose sourceAliases entry need not match its
	// quantifier's alias) — so their agreement is an empirical fact, not a
	// structural one. Its zero is what makes the conversion representation-only
	// and the plan-shape golden untouched.
	//
	// It also subsumes the re-mint at executor.spansFromMergedLegs, which used to
	// manufacture a fresh identifier from the leg's text: that re-mint is exactly
	// the operation whose result this compares against the identity the producer
	// actually chose.
	LegSiteTextVsIdentity

	// LegSiteLeftOuterExistential is cascades.hoistLegRefsOntoMergedRow's drift
	// check: a leg absent from the derived windows but present in the merged
	// type's Legs is a LOUD failure. It compares through SameLeg (EXACT), and the
	// recorded pair is that comparison's own: the leg's identity against the
	// reference correlation's. Its retired predicate folded the reference before
	// comparing it to the leg's text, which is why the verdict rather than the pair
	// is what says whether the conversion moved this tripwire.
	//
	// Its absolute totals are a planning-time artifact, not a corpus fact: the
	// hoist runs inside a Cascades rule, so the memo may fire it once or many
	// times for the same query depending on exploration order. Only the RATIOS
	// (foldOnly == 0) carry meaning across runs.
	LegSiteLeftOuterExistential

	// LegSiteFinalizeSeedWindows is values.finalizeSeedWindows' buried-sub-window
	// derivation: "is this buried leg the box run's rightmost leaf?", decided
	// through SameLeg on the leaf's identity against the box window's.
	//
	// It was the last site held back from the conversion, on a justification that
	// was measured to be false: the two identifiers were said to be "legitimately
	// different — box quantifier vs leaf", when the sourceBinding convention is
	// exactly that for the rightmost leaf they are the SAME identifier. The red
	// that was cited as proof came from a test fixture hand-minting the box
	// correlation lowercase where production mints sourceAlias(box), upper.
	// Measured over the real-FDB corpus, the identity and text comparisons decide
	// identically on all 1311 pairs. The premise now has its own deterministic pin
	// (TestBoxCorrelationIsItsRightmostLeafIdentity) rather than resting on a count.
	//
	// The map KEYS remain upper-FOLDED text — readers still arrive holding a string
	// — so this one site holds both namespaces at once, deliberately.
	LegSiteFinalizeSeedWindows

	// LegSiteSelectOutputLegs is the translator's expressionOutputLegs, recorded at
	// the PRODUCER rather than at a reader.
	//
	// It is the one site whose two spellings come from genuinely independent
	// sources — the leg's identity from its quantifier, its text from the select's
	// parallel sourceAliases slice — so nothing downstream guarantees they agree.
	// Recording it here rather than trusting a reader to walk these legs is what
	// keeps the text-vs-identity zero from resting on an unmeasured producer.
	LegSiteSelectOutputLegs

	// LegSiteNLJPlanAlias is the materialized nested-loop join's own leg
	// identifiers, recorded at the PLANNER where the plan is constructed.
	//
	// The plan used to hold these as strings, and the executor minted an
	// identifier from each at the plan boundary — so the leg identities on the
	// whole merge path were manufactured downstream of the quantifier that owned
	// them. They are threaded from the select's quantifiers now, and this site
	// records the substitution: the pair is the SOURCE-ALIAS text the plan used to
	// carry against the quantifier identifier it carries instead.
	//
	// The substitution is NOT purely representational, and the measurement is why
	// we know: over the FDB corpus the two disagree on 12 of ~80k firings (the 12 is
	// stable across runs, the denominator is not -- 79040 / 79960 / 80856 measured), and the
	// disagreement is total rather than a case variant — witnesses "q$N vs E". In
	// those the select's source-alias slice carries a RE-MINTED identifier while its
	// quantifier still carries the user alias, so the slice is the stale one and the
	// quantifier is the authority. The old pairing was internally inconsistent
	// there: the seed's QOVs were minted as ToUpper(alias) while the plan's leg
	// identity kept the raw alias, so a lowercase q$N became Q$N in the seed and
	// q$N in the plan, and values.SameLeg — exact — then bound neither.
	// TestReconstructFoldStep1Seed_CarriesTheThreadedIdentityVerbatim pins that.
	//
	// So the zero this site asserts is FoldOnlyEqual, not Neither. Fold-only is the
	// forgery population: a case-ONLY difference means one spelling is an upper fold
	// of the other, which is precisely how ToUpper manufactured a Q$N that could
	// forge a minted q$N leg. A wholly different name is a stale source alias, which
	// threading the quantifier is the fix for.
	LegSiteNLJPlanAlias

	// LegSiteOrdinalSlotInLegWindow is the translator's ordinalSlotInLegWindow: it
	// resolves a qualified column to its slot WITHIN the named leg's window, and its
	// decline is what keeps a qualified `B.ID` from flat-first-matching A's
	// same-named slot. Its counterparty is a correlation, so it compares through
	// SameLeg like every other Group-A reader; it used to compare the leg's text
	// against the UPPER fold of that correlation.
	//
	// This is the site where the instrument's blindness had consequences. Its census
	// recorded (leg text, correlation text) while its comparison evaluated the two
	// identities, so a minted leg read by its own UPPER Name scored an exact MATCH
	// in the census and a DECLINE in the code. Measured with the pair the comparison
	// actually evaluates, the real-FDB corpus reports 0 divergences over 105
	// comparisons; the only such pairs anywhere are the deliberate negative controls
	// in the translator package's own fixture, where the decline is the assertion.
	LegSiteOrdinalSlotInLegWindow

	legIdentitySiteCount
)

// String names the site for test failure messages.
func (s LegIdentitySite) String() string {
	switch s {
	case LegSiteRowLegsBinder:
		return "rowLegsBinder.GetCorrelationBinding (runtime leg binder)"
	case LegSiteBuriedLegWindow:
		return "buriedLegWindow (buried sub-window in a box span)"
	case LegSiteTextVsIdentity:
		return "leg text vs leg identity (Name vs Alias)"
	case LegSiteLeftOuterExistential:
		return "hoistLegRefsOntoMergedRow (drift check)"
	case LegSiteFinalizeSeedWindows:
		return "finalizeSeedWindows (buried sub-window derivation)"
	case LegSiteSelectOutputLegs:
		return "expressionOutputLegs (producer: quantifier vs sourceAliases)"
	case LegSiteNLJPlanAlias:
		return "NLJ plan leg identity (planner: sourceAlias text vs quantifier)"
	case LegSiteOrdinalSlotInLegWindow:
		return "ordinalSlotInLegWindow (qualified slot within a leg window)"
	default:
		return "unknown leg identity site"
	}
}

// LegIdentityCensus is one site's comparison population.
type LegIdentityCensus struct {
	// Total is every leg/correlation pair the site examined.
	Total int64

	// ExactEqual is the pairs whose two spellings are byte-identical — the
	// population on which an exact and a folding comparison cannot disagree.
	ExactEqual int64

	// FoldOnlyEqual is the pairs that fold equal but are NOT equal. This is the
	// conversion delta, and it is the count that must be ZERO for a
	// folding-to-exact conversion to be free. A nonzero value means a leg is
	// stored under one spelling and looked up under another, which is a producer
	// defect: the fix belongs at whichever site normalizes, not in the
	// comparison.
	FoldOnlyEqual int64

	// Neither is the pairs that are different legs under either comparison — the
	// scan traffic every lookup pays walking to its own leg.
	Neither int64

	// FoldOnlySamples holds up to legIdentitySampleCap distinct "legName vs
	// corrName" witnesses from the FoldOnlyEqual population, so a nonzero count
	// names the offending pair instead of merely reporting a number.
	FoldOnlySamples []string

	// Unstated is the pairs where at least one side is the ZERO
	// CorrelationIdentifier. It exists only on the identity channel: SameLeg
	// declines an unstated identifier unconditionally (SameLeg(zero, zero) used to
	// be true, and two omissions agreeing is how a leg bound the wrong row), so
	// such a pair is neither a match nor an ordinary miss — it is a leg whose
	// identity was never sourced. Counting it apart keeps ExactEqual/FoldOnlyEqual/
	// Neither meaning what they say on the identity channel.
	//
	// The TEXT channel has no equivalent bucket: there, "" is just a string, and a
	// leg with no text is skipped by its producers before the comparison.
	Unstated int64

	// UnstatedSamples names the offending sides, same service as the other two
	// witness sets — an unstated identity is diagnosable only by which side is
	// blank and what the other one was.
	UnstatedSamples []string

	// NeitherSamples is the same service for the Neither population, and it is
	// collected ONLY at the sites where Neither is the asserted-zero
	// (LegSiteNeitherMustBeZero). At a genuine COMPARISON site Neither is the
	// ordinary scan traffic every lookup pays walking to its own leg — a large,
	// per-row population whose witnesses say nothing and whose sampling would put
	// a mutex in the row loop. At the identity-PAIR sites it is the failure itself:
	// a leg whose two spellings name different things.
	//
	// This exists because it was needed: retyping the nested-loop join plan's leg
	// identities reported Neither = 12 over ~80k firings, and a bare 12 cannot be
	// diagnosed. The rule generalizes — whichever population a site's assertion
	// requires to be zero is the population that must name its witnesses.
	NeitherSamples []string

	// RetiredVerdictDivergent is the pairs on which the predicate the site USED TO
	// evaluate disagrees with the identity predicate it evaluates NOW. It is the
	// conversion delta measured DIRECTLY, and it is the only population that
	// answers the question the conversion actually raises.
	//
	// The three zeros above do not answer it, and reasoning from them is what let a
	// converted reader be reported clean. FoldOnlyEqual catches a match that became
	// a decline only when the retired predicate was exact-on-text; two of these
	// sites upper-FOLDED one side, so their retired predicate could match a pair
	// this one declines for a reason no fold-only count sees. And a DECLINE that
	// became a MATCH is invisible to every one of them — a leg whose identity is a
	// lowercase machine mint matches itself exactly while the retired
	// upper-folding predicate rejected it, and that pair lands in ExactEqual,
	// indistinguishable from a pair both predicates accepted.
	//
	// Zero here means the conversion is representation-only ON THIS CORPUS, which
	// is the claim the migration makes. A nonzero value names a shape whose row
	// binding changed and must be justified as a FIX with a test, not measured
	// away.
	RetiredVerdictDivergent int64

	// RetiredVerdictSamples names the divergent pairs, in the form
	// "leg vs corr (retired=match|decline)", so a nonzero count says which
	// direction flipped as well as where.
	RetiredVerdictSamples []string

	// Channel is which namespace the site recorded in — see LegCensusChannel. It
	// is observed, not declared, so a site whose instrument drifts away from its
	// comparison is reported instead of believed.
	Channel LegCensusChannel
}

// LegCensusChannel is the namespace a site's recorded pairs live in.
//
// It is set by whichever Record function the site calls and is never declared in
// a table, because a table is exactly what went wrong: the sites were DOCUMENTED
// as comparing identities while their instrument recorded text, and no mechanism
// noticed. A site that records in both namespaces reports LegChannelMixed, which
// the corpus gate treats as a failure — its numbers then describe neither
// comparison.
type LegCensusChannel int32

const (
	// LegChannelNone is a site nothing recorded at.
	LegChannelNone LegCensusChannel = 0
	// LegChannelText is a site whose pairs are two strings, compared as text.
	LegChannelText LegCensusChannel = 1
	// LegChannelIdentity is a site whose pairs are two CorrelationIdentifiers,
	// classified the way SameLeg decides.
	LegChannelIdentity LegCensusChannel = 2
	// LegChannelMixed is a site that recorded in both — an instrument that cannot
	// be read.
	LegChannelMixed LegCensusChannel = 3
)

// String names the channel for report and failure text.
func (c LegCensusChannel) String() string {
	switch c {
	case LegChannelText:
		return "text"
	case LegChannelIdentity:
		return "identity"
	case LegChannelMixed:
		return "MIXED"
	default:
		return "unrecorded"
	}
}

// LegSiteNeitherMustBeZero reports whether a site's Neither population is a
// FAILURE rather than ordinary traffic.
//
// It is true exactly at the sites whose recorded pair is two spellings of ONE
// leg that must therefore denote the same thing: a leg's own (Name,
// Alias.Name()). Everywhere else the pair is a lookup against a leg it is allowed
// not to be, so Neither counts misses and only FoldOnlyEqual can indict.
//
// LegSiteNLJPlanAlias is deliberately NOT here even though its pair is also two
// spellings of one leg: measured, the select's source-alias slice can carry a
// re-minted identifier while its quantifier keeps the user alias, and that
// divergence is the reason to prefer the quantifier rather than a reason to fail.
// See that site's comment.
//
// The distinction is the reason FoldOnlyEqual == 0 was not enough on its own: a
// producer that stores Name "X" against Alias "Y" lands in Neither and leaves the
// fold-only count at zero while the text channel and the identity channel have
// diverged.
func LegSiteNeitherMustBeZero(site LegIdentitySite) bool {
	switch site {
	case LegSiteTextVsIdentity, LegSiteSelectOutputLegs:
		return true
	default:
		return false
	}
}

// legIdentitySampleCap bounds the retained witness set. A handful names the
// producer; an unbounded set would let a hot row path grow the slice without
// limit.
const legIdentitySampleCap = 16

var (
	legCensusEnabled atomic.Bool
	legCensus        [legIdentitySiteCount]LegIdentityCensus

	// legCensusChannel is the observed namespace per site, ORed from the channel
	// each Record call belongs to: text|identity == LegChannelMixed, so a site that
	// records both is detected without any site declaring anything.
	legCensusChannel [legIdentitySiteCount]atomic.Int32

	// legSampleMu guards the sample slices only. The counters are atomic and
	// lock-free; sampling happens only on the FoldOnlyEqual path, which is the
	// population whose zero is being proven, so the lock is uncontended whenever
	// the proof holds.
	legSampleMu sync.Mutex
)

// SetLegIdentityCensusEnabled turns the census on or off. It is OFF in every
// production path and in every test but the corpus census pass; when off, each
// comparison pays one relaxed atomic load.
//
// The gate matters more here than for the planner-side censuses: two of these
// sites are on the PER-ROW executor path, so an always-on counter would put a
// contended atomic increment in the row loop.
func SetLegIdentityCensusEnabled(on bool) { legCensusEnabled.Store(on) }

// LegIdentityCensusEnabled reports the gate state.
func LegIdentityCensusEnabled() bool { return legCensusEnabled.Load() }

// RecordLegIdentityLeg records a leg's two spellings against each other at the
// text-vs-identity divergence site. Every reader that walks a leg calls it, so the
// population is every leg any consumer looked at.
//
// Callers must guard on LegIdentityCensusEnabled().
func RecordLegIdentityLeg(leg RecordTypeLeg) {
	RecordLegIdentityComparison(LegSiteTextVsIdentity, leg.Name, leg.Alias.Name())
}

// RecordLegIdentityComparison records one leg-identity pair at a site: legName is
// the leg's stored text, corrName the counterparty correlation's own spelling.
//
// Callers must guard on LegIdentityCensusEnabled() so the disabled path costs one
// atomic load and no argument evaluation.
func RecordLegIdentityComparison(site LegIdentitySite, legName, corrName string) {
	if site < 0 || site >= legIdentitySiteCount {
		return
	}
	legCensusChannel[site].Or(int32(LegChannelText))
	c := &legCensus[site]
	atomic.AddInt64(&c.Total, 1)
	switch {
	case legName == corrName:
		atomic.AddInt64(&c.ExactEqual, 1)
	case strings.EqualFold(legName, corrName):
		atomic.AddInt64(&c.FoldOnlyEqual, 1)
		recordSample(site, legName, corrName, sampleFoldOnly)
	default:
		atomic.AddInt64(&c.Neither, 1)
		if LegSiteNeitherMustBeZero(site) {
			recordSample(site, legName, corrName, sampleNeither)
		}
	}
}

// RecordLegIdentityConversion is RecordLegIdentityPair plus the acceptance test
// the conversion needs: retiredVerdict is what the predicate this site USED TO
// evaluate says about this same pair, and a disagreement with SameLeg is counted
// and witnessed.
//
// Every converted site must use this rather than RecordLegIdentityPair while the
// migration is open. Computing the retired predicate at the site costs a string
// compare inside the census gate — production never evaluates it — and it is what
// turns "the conversion is representation-only" from three inferences chained
// across separate zero counts into one measurement of the decision itself. The
// retired predicates are not uniform (two sites compared exact text, two
// upper-folded one side), so no single central rule could stand in for them; each
// site states its own, verbatim.
//
// Callers must guard on LegIdentityCensusEnabled().
func RecordLegIdentityConversion(site LegIdentitySite, leg, corr CorrelationIdentifier, retiredVerdict bool) {
	RecordLegIdentityPair(site, leg, corr)
	if site < 0 || site >= legIdentitySiteCount {
		return
	}
	if retiredVerdict == SameLeg(leg, corr) {
		return
	}
	atomic.AddInt64(&legCensus[site].RetiredVerdictDivergent, 1)
	verdict := "decline"
	if retiredVerdict {
		verdict = "match"
	}
	recordSample(site, leg.Name(), corr.Name()+" (retired="+verdict+")", sampleRetired)
}

// RecordLegIdentityPair records the pair a site's IDENTITY comparison actually
// evaluates: the leg's own CorrelationIdentifier against the counterparty
// correlation's. Every site that decides with values.SameLeg must use this and not
// the string form — a site whose comparison is exact-on-identifiers and whose
// census is exact-on-text can report a MATCH for a pair the comparison DECLINES,
// which is how a converted reader was reported clean while its instrument
// described a different program.
//
// Classification mirrors SameLeg exactly, so the census cannot disagree with the
// decision it is measuring: an unstated side is Unstated (SameLeg declines it),
// byte-equal is ExactEqual, case-equal is FoldOnlyEqual — the pairs a folding
// comparison would have matched and this one does not — and anything else is
// Neither.
//
// A converted site wants RecordLegIdentityConversion instead: this function's
// populations describe the pair, not the change in verdict.
//
// Callers must guard on LegIdentityCensusEnabled().
func RecordLegIdentityPair(site LegIdentitySite, leg, corr CorrelationIdentifier) {
	if site < 0 || site >= legIdentitySiteCount {
		return
	}
	legCensusChannel[site].Or(int32(LegChannelIdentity))
	c := &legCensus[site]
	atomic.AddInt64(&c.Total, 1)
	switch {
	case leg.IsZero() || corr.IsZero():
		atomic.AddInt64(&c.Unstated, 1)
		recordSample(site, leg.Name(), corr.Name(), sampleUnstated)
	case leg.Name() == corr.Name():
		atomic.AddInt64(&c.ExactEqual, 1)
	case strings.EqualFold(leg.Name(), corr.Name()):
		atomic.AddInt64(&c.FoldOnlyEqual, 1)
		recordSample(site, leg.Name(), corr.Name(), sampleFoldOnly)
	default:
		atomic.AddInt64(&c.Neither, 1)
		if LegSiteNeitherMustBeZero(site) {
			recordSample(site, leg.Name(), corr.Name(), sampleNeither)
		}
	}
}

// legSampleKind selects which of a site's indictable populations a witness names.
type legSampleKind int

const (
	sampleFoldOnly legSampleKind = iota
	sampleNeither
	sampleUnstated
	sampleRetired
)

// recordSample retains a distinct witness for one of a site's indictable
// populations.
func recordSample(site LegIdentitySite, legName, corrName string, kind legSampleKind) {
	w := legName + " vs " + corrName
	legSampleMu.Lock()
	defer legSampleMu.Unlock()
	c := &legCensus[site]
	var dst *[]string
	switch kind {
	case sampleNeither:
		dst = &c.NeitherSamples
	case sampleUnstated:
		dst = &c.UnstatedSamples
	case sampleRetired:
		dst = &c.RetiredVerdictSamples
	default:
		dst = &c.FoldOnlySamples
	}
	for _, s := range *dst {
		if s == w {
			return
		}
	}
	if len(*dst) < legIdentitySampleCap {
		*dst = append(*dst, w)
	}
}

// LegIdentityCensusOf returns a site's counts.
//
// The absolute totals are a LOWER BOUND unless the census ran alone: these
// counters are package-scoped, so a sibling test planning or executing the same
// corpus in parallel contributes to them. The assertion the census exists to
// support is a ZERO (FoldOnlyEqual), and a zero over a sum of non-negative terms
// is exact however many extra passes contribute — a concurrent pass can only make
// it FAIL, never falsely pass. Reset before a pass and isolate that pass if the
// totals themselves are the artifact needed.
func LegIdentityCensusOf(site LegIdentitySite) LegIdentityCensus {
	if site < 0 || site >= legIdentitySiteCount {
		return LegIdentityCensus{}
	}
	c := &legCensus[site]
	legSampleMu.Lock()
	samples := append([]string(nil), c.FoldOnlySamples...)
	neitherSamples := append([]string(nil), c.NeitherSamples...)
	unstatedSamples := append([]string(nil), c.UnstatedSamples...)
	retiredSamples := append([]string(nil), c.RetiredVerdictSamples...)
	legSampleMu.Unlock()
	return LegIdentityCensus{
		Total:                   atomic.LoadInt64(&c.Total),
		ExactEqual:              atomic.LoadInt64(&c.ExactEqual),
		FoldOnlyEqual:           atomic.LoadInt64(&c.FoldOnlyEqual),
		Neither:                 atomic.LoadInt64(&c.Neither),
		Unstated:                atomic.LoadInt64(&c.Unstated),
		RetiredVerdictDivergent: atomic.LoadInt64(&c.RetiredVerdictDivergent),
		FoldOnlySamples:         samples,
		NeitherSamples:          neitherSamples,
		UnstatedSamples:         unstatedSamples,
		RetiredVerdictSamples:   retiredSamples,
		Channel:                 LegCensusChannel(legCensusChannel[site].Load()),
	}
}

// LegIdentitySites returns every site, so a census pass can iterate without
// hard-coding the set.
func LegIdentitySites() []LegIdentitySite {
	out := make([]LegIdentitySite, 0, legIdentitySiteCount)
	for i := LegIdentitySite(0); i < legIdentitySiteCount; i++ {
		out = append(out, i)
	}
	return out
}

// ResetLegIdentityCensus zeroes every site's counts and witnesses.
func ResetLegIdentityCensus() {
	legSampleMu.Lock()
	defer legSampleMu.Unlock()
	for i := range legCensus {
		c := &legCensus[i]
		atomic.StoreInt64(&c.Total, 0)
		atomic.StoreInt64(&c.ExactEqual, 0)
		atomic.StoreInt64(&c.FoldOnlyEqual, 0)
		atomic.StoreInt64(&c.Neither, 0)
		atomic.StoreInt64(&c.Unstated, 0)
		atomic.StoreInt64(&c.RetiredVerdictDivergent, 0)
		c.FoldOnlySamples = nil
		c.NeitherSamples = nil
		c.UnstatedSamples = nil
		c.RetiredVerdictSamples = nil
		legCensusChannel[i].Store(int32(LegChannelNone))
	}
}
