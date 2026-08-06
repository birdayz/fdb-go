package query

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The UNNEST LEG-MINT census.
//
// It measures the surviving qualified-name mint at
// `rebaseUnnestOuterLegPredicate` — `leg + "." + UPPER(field)`, the name-model
// arm's rebase — on two axes that have only ever been reasoned about:
//
//  1. WHICH of the five call sites are reached by the corpus. The five are not
//     one arm: three sit in an explicit `!seedWindowed` / `!ordinalSeed`
//     else-branch with the ordinal twin selected on the other side, and two
//     apply no seed test at all. A conversion driven by flipping a seed test
//     moves only the first three, and a census that reported one total could not
//     say so.
//
//  2. WHAT NAMES it mints. This is the axis that matters, because the standing
//     acceptance condition for retiring the EXECUTOR's dotted reader
//     (`rowSlotForLegColumn`) is "the leg-column provenance dotted-hit count
//     goes to 0", booked against converting THIS mint. That condition is only
//     reachable from here if the names this mint produces are the names that
//     reader answers on. Those are two separately measured sets, and until this
//     census existed nothing compared them.
//
// The comparison is the assertion, not the counts:
// AssertUnnestLegMintCensus checks that the minted-name set and the executor's
// dotted-hit name set are DISJOINT. A disjoint reading is a NEGATIVE result and
// it is pinned deliberately — it says the executor reader's live hits come from
// a different producer, so converting this mint cannot drive them to zero. If
// the sets ever intersect, that claim is re-armed and has to be re-derived; the
// failure message says so.
//
// WHAT IT MEASURED — whole real-FDB sqldriver corpus, master at 6692ac268,
// uncached, three consecutive runs agreeing on the shape:
//
//	nonChainedMerge(no seed test)      calls 23 | minted names 0
//	anchoredNonExists(no seed test)    calls  5 | minted names 0
//	buried(!seedWindowed)              calls  0 | minted names 0
//	joinPredicate(!seedWindowed)       calls  0 | minted names 0
//	chained(name-model seed)           calls  0 | minted names 0
//	distinct minted names: NONE
//
// The split is the finding. The two sites that apply NO seed test are LIVE and
// mint nothing; the three sites a seed-gate flip would convert are DARK. So a
// conversion driven by making the seed windowed moves arms nothing reaches, and
// the qualified-name channel this mint is supposed to feed carries no name at
// all over this corpus.
//
// NO FLOOR IS ASSERTED, deliberately, and the direction is why. Every other
// floor on this path guards a population whose collapse would make a zero read
// as good news. Here a collapse to zero at the two live sites would be a
// DELETION SIGNAL for the whole arm, not a regression — the dangerous direction
// is the opposite one, a site starting to mint names, and that is what the
// disjointness assertion below watches. The per-site report is the instrument;
// a floor would turn a finding into a build break.
//
// GATED by values.LegIdentityCensusEnabled, like every census on this path.
// Counts are per CALL of the mint's rewrite, so read the totals as traffic.

// UnnestLegMintSite is one call site of the name-keyed
// `rebaseUnnestOuterLegPredicate`. The five partition its callers, and they are
// named for WHY they reach it rather than for their line numbers, which move.
type UnnestLegMintSite int

const (
	// UnnestLegMintSiteNonChainedMerge is the ELSE of the chained-unnest check
	// in the filter-over-unnest merge. It applies no seed test at all — the
	// `ordinalSeed` test lives INSIDE the chained arm, on the other side of this
	// branch — so no seed-gate flip converts it.
	UnnestLegMintSiteNonChainedMerge UnnestLegMintSite = iota

	// UnnestLegMintSiteAnchoredNonExists is the plain non-chained unnest merge's
	// anchored (not admitted to the gather / no record) arm, which the code
	// names "the correct and now ONLY domain of the name-keyed rebase". Also no
	// seed test.
	UnnestLegMintSiteAnchoredNonExists

	// UnnestLegMintSiteBuriedNotWindowed is the BURIED subquery-internal
	// outer-only filter's `!seedWindowed` else-branch. Its ordinal twin
	// (rebaseUnnestOuterLegPredicateOrdinal) is selected on the other side.
	UnnestLegMintSiteBuriedNotWindowed

	// UnnestLegMintSiteJoinPredNotWindowed is the EXISTS JoinPredicate channel's
	// `!seedWindowed` arm. Same twin on the other side.
	UnnestLegMintSiteJoinPredNotWindowed

	// UnnestLegMintSiteChainedNameModel is the chained rebase's name-model
	// fallback, taken when the chained seed is not ordinal.
	UnnestLegMintSiteChainedNameModel

	unnestLegMintSiteCount
)

func (s UnnestLegMintSite) String() string {
	switch s {
	case UnnestLegMintSiteNonChainedMerge:
		return "nonChainedMerge(no seed test)"
	case UnnestLegMintSiteAnchoredNonExists:
		return "anchoredNonExists(no seed test)"
	case UnnestLegMintSiteBuriedNotWindowed:
		return "buried(!seedWindowed)"
	case UnnestLegMintSiteJoinPredNotWindowed:
		return "joinPredicate(!seedWindowed)"
	case UnnestLegMintSiteChainedNameModel:
		return "chained(name-model seed)"
	}
	return "unknown"
}

const unnestLegMintWitnessCap = 128

var (
	unnestLegMintMu      sync.Mutex
	unnestLegMintCalls   [unnestLegMintSiteCount]int
	unnestLegMintRewrote [unnestLegMintSiteCount]int
	unnestLegMintNames   []string
)

// RecordUnnestLegMintCall counts ONE invocation of the name-keyed rebase at a
// site, whether or not it rewrites anything. Callers must guard on
// values.LegIdentityCensusEnabled().
//
// Counted separately from the NAMES because a site can be reached constantly
// and mint nothing (no reference in the predicate names an outer leg), and the
// two facts have opposite meanings for a conversion: the first says the arm is
// live, the second says it produces the text a downstream reader decides on.
func RecordUnnestLegMintCall(site UnnestLegMintSite) {
	if site < 0 || site >= unnestLegMintSiteCount {
		return
	}
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegMintCalls[site]++
}

// RecordUnnestLegMintName counts ONE minted qualified name at a site. Callers
// must guard on values.LegIdentityCensusEnabled().
func RecordUnnestLegMintName(site UnnestLegMintSite, name string) {
	if site < 0 || site >= unnestLegMintSiteCount {
		return
	}
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegMintRewrote[site]++
	if len(unnestLegMintNames) >= unnestLegMintWitnessCap {
		return
	}
	for _, seen := range unnestLegMintNames {
		if seen == name {
			return
		}
	}
	unnestLegMintNames = append(unnestLegMintNames, name)
}

// UnnestLegMintCensus reports per-site calls, per-site mints, and the distinct
// minted names.
func UnnestLegMintCensus() (calls, mints [unnestLegMintSiteCount]int, names []string) {
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	out := make([]string, len(unnestLegMintNames))
	copy(out, unnestLegMintNames)
	return unnestLegMintCalls, unnestLegMintRewrote, out
}

// ResetUnnestLegMintCensus clears the counters.
func ResetUnnestLegMintCensus() {
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegMintCalls = [unnestLegMintSiteCount]int{}
	unnestLegMintRewrote = [unnestLegMintSiteCount]int{}
	unnestLegMintNames = nil
}

// FormatUnnestLegMintCensus renders the census for a harness to log.
func FormatUnnestLegMintCensus() string {
	calls, mints, names := UnnestLegMintCensus()
	var b strings.Builder
	b.WriteString("unnest leg-mint (rebaseUnnestOuterLegPredicate, per call):")
	for s := UnnestLegMintSite(0); s < unnestLegMintSiteCount; s++ {
		fmt.Fprintf(&b, "\n  %-34s calls %d | minted names %d", s, calls[s], mints[s])
	}
	if len(names) > 0 {
		sorted := append([]string{}, names...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  distinct minted names (%d, cap %d): %s",
			len(sorted), unnestLegMintWitnessCap, strings.Join(sorted, ", "))
	} else {
		b.WriteString("\n  distinct minted names: NONE")
	}
	return b.String()
}

// AssertUnnestLegMintCensus checks the one claim this census exists to keep
// honest: the names this mint produces and the names the EXECUTOR's dotted
// leg-column reader answers on are DISJOINT sets.
//
// executorDottedNames is the dotted-hit name list from
// executor.LegColumnProvenanceDottedNames — threaded in rather than imported,
// because the executor package must not depend on the translator and this
// assertion needs both populations in one place.
//
// WHY A DISJOINTNESS ASSERTION AND NOT A FLOOR. The booked acceptance condition
// for retiring the executor's dotted arm is that its hit count reaches zero, and
// that condition was booked against converting THIS mint. Disjointness is the
// measurement that says the condition is not reachable from here: the reader's
// live hits are produced somewhere else, so this mint could be deleted outright
// and the reader would answer exactly as often as before. A negative result of
// that shape is load-bearing — it is what reclassifies the conversion — so it is
// pinned rather than written down.
func AssertUnnestLegMintCensus(w io.Writer, executorDottedNames []string) bool {
	_, _, minted := UnnestLegMintCensus()
	return assertUnnestLegMintNames(w, minted, executorDottedNames)
}

// assertUnnestLegMintNames is the decision, split from the process-global
// counters so both directions can be driven without a corpus run — the same
// split every census on this path makes, and for the same reason: a gate is a
// claim about which states FAIL, and an unexercised gate makes no claim.
func assertUnnestLegMintNames(w io.Writer, minted, executorDottedNames []string) bool {
	if len(minted) == 0 {
		// VACUOUS, and it is announced rather than passed over. A disjointness
		// check with an empty side reports the same clean result as two
		// populations measured apart, and this census's whole finding is a
		// comparison — so a silent pass here would read as "the sets do not meet"
		// when what happened is that one of them does not exist.
		//
		// It is a REPORT and not a failure because zero is the measured reading on
		// this tree: the mint is unreached over the real-FDB sqldriver corpus. What
		// must not happen is that reading being mistaken for the comparison having
		// been made.
		fmt.Fprintf(w, "UNNEST LEG-MINT CENSUS VACUOUS: the name-keyed unnest rebase minted\n"+
			"  NO qualified names over this run, so the disjointness check against the\n"+
			"  executor's %d dotted-hit name(s) did not compare anything.\n"+
			"  The per-site call counts above are what distinguishes a DARK arm (never\n"+
			"  reached) from a LIVE one carrying no rewrite; read them before quoting this\n"+
			"  census as evidence about either.\n", len(executorDottedNames))
		return false
	}
	if len(executorDottedNames) == 0 {
		return false
	}
	dotted := make(map[string]struct{}, len(executorDottedNames))
	for _, n := range executorDottedNames {
		dotted[strings.ToUpper(n)] = struct{}{}
	}
	var overlap []string
	for _, n := range minted {
		if _, hit := dotted[strings.ToUpper(n)]; hit {
			overlap = append(overlap, n)
		}
	}
	if len(overlap) == 0 {
		return false
	}
	sort.Strings(overlap)
	fmt.Fprintf(w, "UNNEST LEG-MINT CENSUS FAIL: %d name(s) minted by the translator's\n"+
		"  name-keyed unnest rebase are ALSO answered by the executor's dotted\n"+
		"  leg-column reader: %s\n"+
		"  These two populations have been measured DISJOINT, and that disjointness is\n"+
		"  why converting this mint cannot drive the executor reader's dotted-hit count\n"+
		"  to zero — the reader's live hits are minted by the correlated-scalar seed's\n"+
		"  leg-column labels, not here.\n"+
		"  WHAT AN OVERLAP RE-ARMS: the acceptance condition 'dotted hits -> 0 by\n"+
		"  converting this mint' becomes reachable from the translator side again, and\n"+
		"  every plan that reclassified it as unreachable has to be re-derived against\n"+
		"  this reading. Do NOT relax this assertion; identify which site minted the\n"+
		"  overlapping name and which reader answered on it.\n"+
		"  census: %s\n", len(overlap), strings.Join(overlap, ", "), FormatUnnestLegMintCensus())
	return true
}

// unnestLegMintEnabled is the census gate, kept as one call so the mint's hot
// path reads a single predicate.
func unnestLegMintEnabled() bool { return values.LegIdentityCensusEnabled() }
