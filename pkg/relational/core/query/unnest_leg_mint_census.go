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
// WHAT IT MEASURED — whole real-FDB sqldriver corpus, uncached, one run:
//
//	nonChainedMerge(no seed test)      calls 23 | minted names 0
//	anchoredNonExists(no seed test)    calls  5 | minted names 0
//	buried(!seedWindowed)              calls  0 | minted names 0
//	joinPredicate(!seedWindowed)       calls  0 | minted names 0
//	chained(name-model seed)           calls  0 | minted names 0
//	  buried(!seedWindowed)        reached  10 | name 0 | ordinal-twin  9 | planTimeBake  1 | leg-relative  0
//	  joinPredicate(!seedWindowed) reached 165 | name 0 | ordinal-twin  0 | planTimeBake 92 | leg-relative 73
//	  chained(name-model seed)     reached  33 | name 0 | ordinal-twin 33 | planTimeBake  0 | leg-relative  0
//	rebaseUnnestOuterLegPredicateOrdinal calls 133 (42 attributed to a branch arm)
//	distinct minted names: NONE
//
// The ARM rows are the finding, and the per-site call counts alone do NOT
// support it. All three seed-tested branch points are REACHED and take the name
// arm ZERO times: the seed over them is already windowed, so the conversion a
// lifted seed gate would perform has already happened on the other side of each
// branch. Read without the arms, those three zeros say "never reached" — the
// opposite claim, with the opposite follow-up — and a prior revision of this
// header said exactly that.
//
// The other two sites apply no seed test, are reached 28 times, and mint nothing.
// So the qualified-name channel this mint is supposed to feed carries no name at
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

// UnnestLegMintArm is WHICH arm a seed-tested branch point took.
//
// It exists because a per-site call count of ZERO at a `!seedWindowed`
// else-branch is AMBIGUOUS on its own, and the ambiguity has opposite
// consequences. "Never reached" says the shape is not planned; "reached, took
// the ordinal arm" says the shape IS planned and its conversion has already
// happened on the other side of the branch. Both readings support demoting a
// conversion booked against the name arm, and they imply different follow-ups —
// so the census must not leave the reader to pick one.
//
// The arm is recorded AT the arm, and the branch's REACH is recorded
// INDEPENDENTLY at the branch point, before any arm is chosen. Both halves are
// necessary and the second one was missing when this census first shipped: the
// renderer computed the branch total AS the sum of the arms, so an arm added
// without a `RecordUnnestLegMintArm` call would silently SHRINK the printed
// `reached N` instead of showing up as a gap. That is the summed denominator this
// RFC's own text calls "true by construction", one field over from the twin
// counter that was given a real one.
//
// It bites hardest at the joinPredicate branch, where the census guard doubles as
// the third arm (`else if jpCensus`): inserting an `else if` ahead of it steals
// silently from `leg-relative`. With an independent reach counter that theft is
// an ARM PARTITION FAIL, not a quieter number.
type UnnestLegMintArm int

const (
	// UnnestLegMintArmName: the branch took the name-keyed rebase — the arm the
	// mint counters above measure.
	UnnestLegMintArmName UnnestLegMintArm = iota

	// UnnestLegMintArmOrdinalTwin: the branch took
	// rebaseUnnestOuterLegPredicateOrdinal. This is the reading that turns a zero
	// on the name arm from "dead shape" into "already converted here".
	UnnestLegMintArmOrdinalTwin

	// UnnestLegMintArmPlanTimeBake: the branch took the E-1a plan-time bake
	// (bakeInnerExistsPredicateOrdinal), which reaches the ordinal twin one level
	// down. Counted apart from the twin arm because the two are different
	// DECISIONS that happen to share a callee, and folding them would report an
	// inner-cluster bake as a seed-test outcome.
	UnnestLegMintArmPlanTimeBake

	// UnnestLegMintArmLegRelative: the JoinPredicate channel's windowed,
	// non-plan-time-bake fall-through, which rebases NOTHING and leaves the refs
	// leg-relative for the executor's below-FOD hoist. It is an arm of the branch
	// and therefore counted; without it the branch's arms do not partition and a
	// zero elsewhere cannot be read.
	UnnestLegMintArmLegRelative

	unnestLegMintArmCount
)

// hasSeedBranch reports whether this site sits on a seed test with arms to take.
//
// Two of the five do not: they are unconditional calls, so their arm row would
// be zeros forever. Printing that as a measurement is the same defect the arm
// sub-report exists to fix one level up — a structural zero reading exactly like
// an observed one.
func (s UnnestLegMintSite) hasSeedBranch() bool {
	switch s {
	case UnnestLegMintSiteBuriedNotWindowed,
		UnnestLegMintSiteJoinPredNotWindowed,
		UnnestLegMintSiteChainedNameModel:
		return true
	}
	return false
}

func (a UnnestLegMintArm) String() string {
	switch a {
	case UnnestLegMintArmName:
		return "name"
	case UnnestLegMintArmOrdinalTwin:
		return "ordinal-twin"
	case UnnestLegMintArmPlanTimeBake:
		return "planTimeBake"
	case UnnestLegMintArmLegRelative:
		return "leg-relative"
	}
	return "unknown"
}

const unnestLegMintWitnessCap = 128

var (
	unnestLegMintMu      sync.Mutex
	unnestLegMintCalls   [unnestLegMintSiteCount]int
	unnestLegMintRewrote [unnestLegMintSiteCount]int
	unnestLegMintArms    [unnestLegMintSiteCount][unnestLegMintArmCount]int
	unnestLegMintReached [unnestLegMintSiteCount]int
	unnestLegOrdinalTwin int
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

// RecordUnnestLegMintBranchReached counts ONE arrival at a seed-tested branch
// point, BEFORE any arm is chosen. Callers must guard on
// values.LegIdentityCensusEnabled().
//
// It is the INDEPENDENT denominator for the arm matrix. Summing the arms instead
// is true by construction and cannot see an arm that was added without a
// counter — see the UnnestLegMintArm doc.
func RecordUnnestLegMintBranchReached(site UnnestLegMintSite) {
	if site < 0 || site >= unnestLegMintSiteCount {
		return
	}
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegMintReached[site]++
}

// RecordUnnestLegMintArm counts ONE arm taken at a seed-tested branch point.
// Callers must guard on values.LegIdentityCensusEnabled().
func RecordUnnestLegMintArm(site UnnestLegMintSite, arm UnnestLegMintArm) {
	if site < 0 || site >= unnestLegMintSiteCount || arm < 0 || arm >= unnestLegMintArmCount {
		return
	}
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegMintArms[site][arm]++
}

// RecordUnnestLegOrdinalTwinCall counts ONE invocation of
// rebaseUnnestOuterLegPredicateOrdinal, from any caller.
//
// It is the DENOMINATOR the per-arm counts are read against, and it is counted
// inside the twin rather than summed from the arms for the reason the fold-step1
// census states about its own independent denominator: a sum over the recorded
// arms is true by construction and cannot see a caller that reaches the twin
// without passing a branch point this census instruments.
//
// The twin has THREE call sites: the buried `seedWindowed` arm, the chained
// `ordinalSeed` arm, and `bakeInnerExistsPredicateOrdinal`. This census
// instruments the first two directly as ARMS; the third is reached only through
// the two planTimeBake arms, so its calls are attributed to no arm and show up
// as the gap between this total and the summed ordinal-twin arms. (An earlier
// revision of this comment said "four callers", counting the two planTimeBake
// arms as separate callers of the twin — they are two callers of
// `bakeInnerExistsPredicateOrdinal`, which is one caller of the twin.)
func RecordUnnestLegOrdinalTwinCall() {
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegOrdinalTwin++
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

// UnnestLegMintArms reports the per-site arm matrix and the twin's independent
// total.
func UnnestLegMintArms() ([unnestLegMintSiteCount][unnestLegMintArmCount]int, [unnestLegMintSiteCount]int, int) {
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	return unnestLegMintArms, unnestLegMintReached, unnestLegOrdinalTwin
}

// ResetUnnestLegMintCensus clears the counters.
func ResetUnnestLegMintCensus() {
	unnestLegMintMu.Lock()
	defer unnestLegMintMu.Unlock()
	unnestLegMintCalls = [unnestLegMintSiteCount]int{}
	unnestLegMintRewrote = [unnestLegMintSiteCount]int{}
	unnestLegMintArms = [unnestLegMintSiteCount][unnestLegMintArmCount]int{}
	unnestLegMintReached = [unnestLegMintSiteCount]int{}
	unnestLegOrdinalTwin = 0
	unnestLegMintNames = nil
}

// FormatUnnestLegMintCensus renders the census for a harness to log.
func FormatUnnestLegMintCensus() string {
	calls, mints, names := UnnestLegMintCensus()
	arms, reached, twinCalls := UnnestLegMintArms()
	return formatUnnestLegMintCounters(calls, mints, arms, reached, twinCalls, names)
}

// formatUnnestLegMintCounters is the renderer, split from the process-global
// counters so the report's two load-bearing readings — an unreached branch and a
// branch that took another arm — can be driven from a test without racing the
// package's other parallel tests, which run under an enabled census.
func formatUnnestLegMintCounters(
	calls, mints [unnestLegMintSiteCount]int,
	arms [unnestLegMintSiteCount][unnestLegMintArmCount]int,
	reached [unnestLegMintSiteCount]int,
	twinCalls int,
	names []string,
) string {
	var b strings.Builder
	b.WriteString("unnest leg-mint (rebaseUnnestOuterLegPredicate, per call):")
	for s := UnnestLegMintSite(0); s < unnestLegMintSiteCount; s++ {
		fmt.Fprintf(&b, "\n  %-34s calls %d | minted names %d", s, calls[s], mints[s])
	}
	attributed := 0
	b.WriteString("\n  ARM taken at the three seed-tested branch points — a name-arm ZERO beside" +
		"\n  a non-zero ordinal/planTimeBake arm means CONVERTED HERE, not dead:")
	for s := UnnestLegMintSite(0); s < unnestLegMintSiteCount; s++ {
		// The branch total is the INDEPENDENT counter, not the arm sum. The two
		// are compared by assertUnnestLegMintArmPartition; printing the sum here
		// would hide exactly the gap that assertion exists to surface.
		total := reached[s]
		armSum := 0
		for a := 0; a < int(unnestLegMintArmCount); a++ {
			armSum += arms[s][a]
		}
		attributed += arms[s][UnnestLegMintArmOrdinalTwin]
		if !s.hasSeedBranch() {
			// STRUCTURAL, not measured. These two sites are unconditional calls
			// with no arm to take, so a row of zeros here would be a tautology
			// wearing the shape of a reading — the exact ambiguity this whole
			// sub-report exists to remove.
			fmt.Fprintf(&b, "\n    %-34s NO SEED BRANCH EXISTS (unconditional call site)", s)
			continue
		}
		if total == 0 {
			fmt.Fprintf(&b, "\n    %-34s BRANCH POINT NEVER REACHED", s)
			continue
		}
		fmt.Fprintf(&b, "\n    %-34s reached %d", s, total)
		for a := UnnestLegMintArm(0); a < unnestLegMintArmCount; a++ {
			fmt.Fprintf(&b, " | %s %d", a, arms[s][a])
		}
		if armSum != total {
			fmt.Fprintf(&b, "  <<< ARM GAP: arms sum to %d", armSum)
		}
	}
	fmt.Fprintf(&b, "\n  rebaseUnnestOuterLegPredicateOrdinal calls %d (counted INSIDE the twin, "+
		"independently of\n  the arms; %d attributed to an instrumented branch arm, the rest reach "+
		"it from\n  bakeInnerExistsPredicateOrdinal or the chained ordinal path)", twinCalls, attributed)
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
	failed := assertUnnestLegMintNames(w, minted, executorDottedNames)
	arms, reached, _ := UnnestLegMintArms()
	return assertUnnestLegMintArmPartition(w, arms, reached) || failed
}

// assertUnnestLegMintArmPartition checks each seed-tested branch point's arms
// against its INDEPENDENT reach counter, and checks that a branchless site
// records neither.
//
// This is the check the arm matrix's own doc claimed and did not have. Without
// it, an arm added to one of these branches without a recorder does not surface
// as a gap — it silently shrinks the branch total, because the renderer used to
// compute that total AS the arm sum. The joinPredicate branch is the sharp case:
// its census guard doubles as the third arm, so an `else if` inserted ahead of
// that guard steals from `leg-relative` with nothing to notice.
func assertUnnestLegMintArmPartition(
	w io.Writer,
	arms [unnestLegMintSiteCount][unnestLegMintArmCount]int,
	reached [unnestLegMintSiteCount]int,
) bool {
	failed := false
	for s := UnnestLegMintSite(0); s < unnestLegMintSiteCount; s++ {
		armSum := 0
		for a := 0; a < int(unnestLegMintArmCount); a++ {
			armSum += arms[s][a]
		}
		if !s.hasSeedBranch() {
			if armSum != 0 || reached[s] != 0 {
				failed = true
				fmt.Fprintf(w, "UNNEST LEG-MINT ARM PARTITION FAIL: %s is an UNCONDITIONAL call\n"+
					"  site with no seed branch, but recorded %d arm(s) and %d branch arrival(s).\n"+
					"  Either the site grew a branch — in which case hasSeedBranch must say so, or\n"+
					"  its report will keep printing a structural line where a reading now exists —\n"+
					"  or a recorder was wired to the wrong site.\n", s, armSum, reached[s])
			}
			continue
		}
		if armSum == reached[s] {
			continue
		}
		failed = true
		fmt.Fprintf(w, "UNNEST LEG-MINT ARM PARTITION FAIL: %s was reached %d time(s) but its\n"+
			"  arms sum to %d.\n"+
			"  The reach counter is recorded at the branch point BEFORE any arm is chosen and\n"+
			"  the arms are recorded inside each arm, deliberately, so exactly this gap is\n"+
			"  visible. A gap means an arm exists with no RecordUnnestLegMintArm call.\n"+
			"  WHAT A GAP RE-ARMS: every reading of this matrix. Its whole purpose is to tell\n"+
			"  a name-arm ZERO that means 'never reached' apart from one that means 'reached,\n"+
			"  converted on the other arm', and an unrecorded arm silently moves traffic out\n"+
			"  of both. At joinPredicate the census guard IS the third arm (`else if\n"+
			"  jpCensus`), so an `else if` inserted ahead of it steals from leg-relative.\n"+
			"  Add the recorder; do NOT widen this check.\n", s, reached[s], armSum)
	}
	return failed
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
