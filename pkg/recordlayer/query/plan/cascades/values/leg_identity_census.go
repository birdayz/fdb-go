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
// Converting a folding site to an exact one is a BEHAVIOR CHANGE for any pair
// that folds equal but is not equal. Whether such a pair occurs in production
// traffic is a measurement, not an argument, and this census is what makes it
// one. The population it counts is FoldOnlyEqual: pairs where EqualFold says
// "same leg" and exact says "different leg". Its zero is what makes the
// conversion free; a nonzero value is a leg whose two spellings must be
// reconciled at the PRODUCER before any site is converted.
//
// The counts live here, in values, because the consuming sites span three
// packages (executor, cascades, values) and values is the one they all import.
// The corpus pass that reads them lives in the conformance harness, which can
// import this package while a test in here could not import the harness back —
// the same containment the ordering-comparison census settled on.

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
	// type's Legs is a LOUD failure. It compares through SameLeg (EXACT); the
	// recorded pair is the leg's text against the reference correlation's own
	// spelling, so the census still sees what a folding comparison would have
	// decided differently.
	//
	// Its absolute totals are a planning-time artifact, not a corpus fact: the
	// hoist runs inside a Cascades rule, so the memo may fire it once or many
	// times for the same query depending on exploration order. Only the RATIOS
	// (foldOnly == 0) carry meaning across runs.
	LegSiteLeftOuterExistential

	// LegSiteFinalizeSeedWindows is values.finalizeSeedWindows' buried-sub-window
	// derivation, keyed by a map whose keys are upper-FOLDED aliases.
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
	// we know: over the FDB corpus the two disagree on 12 of 79040 firings, and the
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

	// NeitherSamples is the same service for the Neither population, and it is
	// collected ONLY at the sites where Neither is the asserted-zero
	// (LegSiteNeitherMustBeZero). At a genuine COMPARISON site Neither is the
	// ordinary scan traffic every lookup pays walking to its own leg — a large,
	// per-row population whose witnesses say nothing and whose sampling would put
	// a mutex in the row loop. At the identity-PAIR sites it is the failure itself:
	// a leg whose two spellings name different things.
	//
	// This exists because it was needed: retyping the nested-loop join plan's leg
	// identities reported Neither = 12 over 79040, and a bare 12 cannot be
	// diagnosed. The rule generalizes — whichever population a site's assertion
	// requires to be zero is the population that must name its witnesses.
	NeitherSamples []string
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
	c := &legCensus[site]
	atomic.AddInt64(&c.Total, 1)
	switch {
	case legName == corrName:
		atomic.AddInt64(&c.ExactEqual, 1)
	case strings.EqualFold(legName, corrName):
		atomic.AddInt64(&c.FoldOnlyEqual, 1)
		recordSample(site, legName, corrName, false)
	default:
		atomic.AddInt64(&c.Neither, 1)
		if LegSiteNeitherMustBeZero(site) {
			recordSample(site, legName, corrName, true)
		}
	}
}

// recordSample retains a distinct witness for one of a site's two indictable
// populations.
func recordSample(site LegIdentitySite, legName, corrName string, neither bool) {
	w := legName + " vs " + corrName
	legSampleMu.Lock()
	defer legSampleMu.Unlock()
	c := &legCensus[site]
	dst := &c.FoldOnlySamples
	if neither {
		dst = &c.NeitherSamples
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
	legSampleMu.Unlock()
	return LegIdentityCensus{
		Total:           atomic.LoadInt64(&c.Total),
		ExactEqual:      atomic.LoadInt64(&c.ExactEqual),
		FoldOnlyEqual:   atomic.LoadInt64(&c.FoldOnlyEqual),
		Neither:         atomic.LoadInt64(&c.Neither),
		FoldOnlySamples: samples,
		NeitherSamples:  neitherSamples,
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
		c.FoldOnlySamples = nil
		c.NeitherSamples = nil
	}
}
