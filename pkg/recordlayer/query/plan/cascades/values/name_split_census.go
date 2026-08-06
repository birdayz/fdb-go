package values

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// The NAME SPLIT census — the instrument the dotted-leg qualifier census does
// not provide, and the absence its own header admits.
//
// Its sibling counts what a qualifier MATCHED once it reached a leg table. It
// never counts how the qualifier was OBTAINED, and those are different
// questions: a qualifier carried as a parse-tree segment and one recovered by
// slicing a rendered string are indistinguishable at the reader, and only the
// second is RFC-197 debt.
//
// THE SCOPE OF EVERY NUMBER BELOW IS TWO ARMS, and stating that first is the
// point of this paragraph. This census watches the two LEG BAKERS in
// cascades_translator.go — bakeDottedRefsToLegQOVWithRef's segmentsOf and
// bakeFlatRefsAgainstColumns' dotted arm — and NOTHING ELSE. It is not a census
// of "the re-split population" in Go; it is a census of the re-split population
// AT THE TWO BAKERS THE PROJECTION-CHANNEL CONVERSION TOUCHED. Every "zero",
// every floor and every regrowth claim in this file carries that scope, because
// the dark siblings below are neither counted here nor covered by any other
// instrument, and a reader who takes these zeros globally would conclude the
// opposite of what was measured.
//
// THE DARK SIBLINGS — sites that recover a qualifier from a rendered name and
// are NOT counted here. Named rather than merely admitted, because an
// uninstrumented site with no name is one nobody can go and instrument:
//
//   - cascades_translator.go:9547, recursiveRemapValues. STRICTLY WORSE than
//     SPLIT-QUALIFIED: it does not merely manufacture a qualifier STRING to look
//     up in a leg table, it manufactures a CorrelationIdentifier directly out of
//     the bytes before the first dot. Its own header (~:9515) admits the break
//     this census's debt bucket is about — the lazy dotted Field "spells both
//     the qualified `B.ID` and a QUOTED identifier containing a dot in the same
//     string".
//   - core/embedded/colref.go:18, parseColRef — a LAST-dot split with 27
//     production call sites. Most are display or lookup; three MANUFACTURE a
//     qualifier that then decides something: logical_predicate.go:10329 (inner-
//     vs-outer scoping of a projection field), cascades_generator.go:6376 (a
//     projected column's qualifier against the scan's name/alias) and :4476 (the
//     display label, guarded by the parenthesis heuristic at colref.go:41-47 —
//     a heuristic, not a parse).
//   - cascades_translator.go:5016, splitQualifier — the EXISTS fold's LAST-dot
//     split, whose own doc states it treats `A.B.C` as qualifier `A.B`.
//   - derived_unnest.go:107 — a LAST-dot split of the unnest body's source
//     column, compared against the base scan's alias/table name.
//
// One near-sibling IS instrumented, by a DIFFERENT census, and is listed so it
// is not counted twice or mistaken for a gap: unnest_gather.go:391's dotted arm
// is pinned by the SEED-WINDOW READER census (its QUALIFIED-NO-IDENTITY hard
// zero), not by this one.
//
// Instrumenting or converting those sites is booked as CQ-94; the parseColRef
// family in particular is a new measured front, not a widening of this one.
//
// WHY THIS CENSUS EXISTS AT ALL. The "110 calls → 3 → 0" progression quoted for
// the projection-channel conversion, in TODO.md, in road-to-prod.md and in the
// comment at the flat baker's own re-split arm, was SCRATCH measurement: a
// number obtained once by hand, written into prose, and thereafter re-quoted
// rather than re-read. This file is what makes those figures instrument
// readings, and what makes a regrowth at THESE TWO ARMS red instead of silent.
//
// WHAT IS COUNTED. One call per resolution decision at each of the two leg-baker
// arms above, bucketed by WHICH REPRESENTATION DECIDED:
//
//   - SEGMENTED — the parser's triple decided (qualified or not, by FullId
//     segment COUNT). No string was sliced. This is the converted path and it is
//     counted so the channel's liveness is visible: a conversion that silently
//     stopped being reached would otherwise look identical to one that succeeded.
//   - SPLIT-QUALIFIED — a dot was found inside a rendered name and a qualifier
//     was MANUFACTURED out of the bytes before it. This is the debt. It is the
//     one bucket that cannot tell a qualified `A.B` from a quoted `"A.B"`,
//     because at this point the two ARE the same bytes.
//   - SPLIT-BARE — the fallback ran and found no dot, so nothing was
//     manufactured and the whole name is the leaf. Not debt: no qualifier was
//     invented. Counted because it shares the arm with the bucket above, and a
//     census that reported only the debt bucket could not distinguish "the arm
//     is clean" from "the arm is dark".
//
// WHAT IT MEASURED, dated 2026-08-06 over the whole real-FDB sqldriver corpus:
//
//	legQOVSegmentsOf   calls 9 | segmented 9 | SPLIT-QUALIFIED 0 | splitBare 0
//	flatColumnBake     calls 2 | segmented 0 | SPLIT-QUALIFIED 0 | splitBare 2
//
// SPLIT-QUALIFIED is ZERO at both arms. That is the claim the scratch "110 → 3 →
// 0" figures asserted, confirmed for the first time by an instrument rather than
// by a number somebody wrote down once.
//
// THE POPULATION IS THE FINDING, and it is smaller than the scratch figures
// implied. Eleven calls between the two arms — not the ~110 the prose implied
// was flowing before conversion and might flow again. Two consequences, both
// stated rather than smoothed over:
//
//   - legQOVSegmentsOf takes the SEGMENTED path on all 9 of its calls. Its
//     fallback split is not merely producing no qualifiers; it is not being
//     ENTERED. The conversion of that baker's channel is total over this corpus.
//   - flatColumnBake's re-split arm is reached twice, both times by a BARE name
//     that then falls through unbaked. The arm has never, over this corpus,
//     been handed a dotted name at all.
//
// So the hard zero below is nearly vacuous on its own, and saying so is the
// point: the floors are what carry the weight, because they are what fails when
// these arms go from nearly-dark to dark. A zero over a population of two is
// evidence about the corpus, not about the code, and this census is written to
// report which one it has.
//
// THE ZERO-POPULATION SITE IS DECLARED, NOT SILENTLY FLOORED. The natural next
// floor — "each site's SPLIT population (splitBare + SPLIT-QUALIFIED) is at
// least 1", which is what actually guards the recorder wiring on the arms whose
// hard zero this census is about — is RED at legQOVSegmentsOf today, because
// that arm's split population is 0. Floors are therefore declared PER SITE, and
// the zero one carries its disposition rather than a number that would have to
// be relaxed to 0 and read as a guarantee:
//
//	SplitFloor[flatColumnBake]     = 1   // measured 2, and the arm's only class
//	SplitFloor[legQOVSegmentsOf]   = 0   // WATCHED, NOT PROVEN — see below
//
// legQOVSegmentsOf's split arm is NOT dead, and that was measured rather than
// assumed. A panic wired into the arm's entry is reached, with a DOTTED name —
// `TestRecursiveBodyGatesOrdinal` drives it as `C.ORDER_ID`, i.e. it reaches the
// SPLIT-QUALIFIED bucket itself, not merely the arm. That shape is not a test
// artifact either: the arm serves a projection slot whose value is not the
// carrier the translator minted, so `ref` is absent and the split decides. So
// the arm cannot be deleted the way the single-ForEach qualifier arm was (whose
// panic probe came back EMPTY at the same reach), and its zero here is a fact
// about the census-enabled sqldriver CORPUS, not about the code.
//
// The consequence is stated because it is the honest one: a recorder dropped
// from either SPLIT arm of legQOVSegmentsOf is invisible to this corpus — the
// bucket is 0 either way. The Calls floor catches a recorder dropped from its
// SEGMENTED arm (9 → 0) and nothing else. What covers the split arms is a UNIT
// pin on the wiring, per arm and per class, in
// query/name_split_recorder_wiring_test.go — not this floor, which over an empty
// population could not carry that weight and does not claim to.
//
// The DIVERGENCE from the dotted-leg qualifier census's `flatColumnBake calls
// 102` is not a contradiction and is worth stating so the two are not read as
// disagreeing: that census counts `legWindowSlot` calls, which the SEGMENTED
// caller (bakeSegmentedColumnRef) also makes. 102 there against 2 here is the
// converted channel carrying essentially all of the traffic.
//
// WHY THE ARMS STAY at a measured zero. They serve the carrier that states NO
// segments, which the segment triple's contract requires be read as "unknown"
// rather than "unqualified". One class of such carrier can never state them: a
// projection MACHINERY mint, whose names are body output columns with no FullId
// behind them (the star-body normalization in translateScan). Deleting the arms
// would convert those carriers' behaviour by accident.
//
// THE BOUNDARY THIS CENSUS PINS. Those machinery labels are BARE by
// construction, which is why SPLIT-QUALIFIED is zero — not because the arm
// refuses them, but because no label reaching it has ever contained a dot. A
// quoted identifier carrying a dot is legal SQL, so that is a property of the
// current normalization and not a guarantee. Java's guidance on what should then
// happen is POSITIVE, not absent: an output with no `Identifier` is not
// name-resolvable at all — `SemanticAnalyzer.lookup` skips such an expression
// before any comparison (SemanticAnalyzer.java:459-461), rather than matching it
// positionally or synthesizing a string for it (`Expression.name` is an
// `Optional<Identifier>`, Expression.java:100-113; `Expression.ofUnnamed`,
// :305-322, mints the empty ones). Java re-parses no dotted string anywhere:
// identity is `name` + `List<String> qualifier` (Identifier.java:34-58), built
// segment-by-segment off the ANTLR FullId (IdentifierVisitor.java:56-64) and
// joined only for display (Identifier.java:61-63).
//
// THAT SETTLES THE QUESTION THIS CENSUS GUARDS, and the assertion says so
// instead of asking. Earlier prose here called "what should happen to a dotted
// machinery label" open and owner-owned. It is not open. Java rules it: a
// projection output carrying no `Identifier` is not name-resolvable, so a label
// the machinery minted must not resolve BY ANY SPELLING — least of all by this
// arm reading a dot inside it as a qualifier boundary. Widening the
// SPLIT-QUALIFIED zero to accommodate such a label is therefore the one fix that
// is known-wrong, and the failure text says that rather than telling the tripper
// to escalate.
//
// What remains owner-owned is narrower and is not this arm's question: whether
// Go's star-body normalization should stop minting resolvable-looking labels in
// the first place, which moves a live projection channel. Until it does, the
// correct behaviour AT THIS ARM is settled — decline.
//
// GATED by LegIdentityCensusEnabled. Counts are per CALL.

// NameSplitSite is one of the TWO LEG BAKERS' arms that can still recover a
// qualifier by slicing a rendered name. It is not an enumeration of every such
// site in Go — the dark siblings named in this file's header are outside this
// census's scope and outside its zeros.
type NameSplitSite int

const (
	// NameSplitSiteLegQOVSegmentsOf is query.bakeDottedRefsToLegQOVWithRef's
	// segmentsOf: the root node consults the carrier's parse-tree triple when it
	// is Present, and every other node — and every root whose carrier states
	// nothing — falls back to the slice.
	//
	// This baker is where a misread is most consequential: unlike the flat baker
	// it has no exact-name precedence to resolve a quoted spelling first, so the
	// split is the FIRST thing it does.
	NameSplitSiteLegQOVSegmentsOf NameSplitSite = iota

	// NameSplitSiteFlatColumnBake is query.bakeFlatRefsAgainstColumns' dotted
	// arm, reached only after the exact-name first-match over the output columns
	// has already failed. Its segmented counterpart is bakeSegmentedColumnRef,
	// which is a different function rather than a branch here — so this site
	// reports segmented 0 by construction, and that zero is structural, not a
	// finding.
	NameSplitSiteFlatColumnBake

	nameSplitSiteCount
)

func (s NameSplitSite) String() string {
	switch s {
	case NameSplitSiteLegQOVSegmentsOf:
		return "legQOVSegmentsOf"
	case NameSplitSiteFlatColumnBake:
		return "flatColumnBake"
	default:
		return "unknown"
	}
}

// NameSplitClass is one call's bucket. The three partition every call.
type NameSplitClass int

const (
	// NameSplitSegmented: the parser's segment triple decided this node's
	// qualification. No rendered name was sliced.
	NameSplitSegmented NameSplitClass = iota

	// NameSplitQualified: a dot was found in a rendered name and the bytes
	// before it became a qualifier. THE DEBT POPULATION — the only bucket in
	// which a quoted `"A.B"` and a qualified `A.B` are the same input.
	NameSplitQualified

	// NameSplitBare: the fallback ran and found no dot, so no qualifier was
	// manufactured. Not debt; counted so a clean arm is distinguishable from a
	// dark one.
	NameSplitBare

	nameSplitClassCount
)

func (c NameSplitClass) String() string {
	switch c {
	case NameSplitSegmented:
		return "segmented"
	case NameSplitQualified:
		return "SPLIT-QUALIFIED"
	case NameSplitBare:
		return "splitBare"
	default:
		return "unknown"
	}
}

const nameSplitWitnessCap = 64

var (
	nameSplitMu        sync.Mutex
	nameSplitCounts    [nameSplitSiteCount][nameSplitClassCount]int
	nameSplitWitnesses []string
)

// RecordNameSplit records one resolution decision at a splitting arm. `name` is
// the rendered field name the arm was handed; it is used only for the witness
// list of the debt bucket, so a regrowth arrives with the spelling that caused
// it rather than only a count.
func RecordNameSplit(site NameSplitSite, class NameSplitClass, name string) {
	if !LegIdentityCensusEnabled() || site < 0 || site >= nameSplitSiteCount {
		return
	}
	nameSplitMu.Lock()
	defer nameSplitMu.Unlock()
	nameSplitCounts[site][class]++
	// Only the debt bucket earns a witness. The other two are high-population
	// and their spellings say nothing a count does not.
	if class == NameSplitQualified && len(nameSplitWitnesses) < nameSplitWitnessCap {
		w := fmt.Sprintf("%s %q", site, name)
		for _, e := range nameSplitWitnesses {
			if e == w {
				return
			}
		}
		nameSplitWitnesses = append(nameSplitWitnesses, w)
	}
}

// NameSplitCensus returns a snapshot of the counters and the debt witnesses.
func NameSplitCensus() ([nameSplitSiteCount][nameSplitClassCount]int, []string) {
	nameSplitMu.Lock()
	defer nameSplitMu.Unlock()
	w := append([]string(nil), nameSplitWitnesses...)
	sort.Strings(w)
	return nameSplitCounts, w
}

// ResetNameSplitCensus clears the counters. For tests that measure one shape.
func ResetNameSplitCensus() {
	nameSplitMu.Lock()
	defer nameSplitMu.Unlock()
	nameSplitCounts = [nameSplitSiteCount][nameSplitClassCount]int{}
	nameSplitWitnesses = nil
}

// NameSplitFloors is the minimum population each site must report, so that a
// site going DARK is a failure rather than a clean-looking zero.
type NameSplitFloors struct {
	// Calls is the minimum total calls (all three classes) per site.
	Calls [nameSplitSiteCount]int

	// Split is the minimum SPLIT population (splitBare + SPLIT-QUALIFIED) per
	// site — the calls that actually entered a splitting arm, as opposed to the
	// segmented calls that decided without one.
	//
	// It is a separate floor from Calls because at legQOVSegmentsOf the two
	// diverge completely: 9 calls, 0 of them splits. A healthy Calls total there
	// says the SEGMENTED arm is being reached and says nothing whatever about
	// the arms this census's hard zero is a zero over.
	//
	// A ZERO ENTRY IS A DECLARATION, NOT AN ABSENT FLOOR, and it is checked as
	// one: a site declared 0 whose split population is now non-zero FAILS, so
	// the "watched, not proven" label cannot silently outlive the condition that
	// made it honest. Declaring 0 means "this site's split arms are measured
	// empty over this corpus, they are covered by a unit wiring pin instead, and
	// the day the corpus starts driving them somebody re-reads this line."
	Split [nameSplitSiteCount]int
}

// AssertNameSplitCensus checks the census's hard zero and its floors, writing a
// report to w. Returns TRUE if anything FAILED — the convention its siblings on
// this path use, so a call site reading `if failed := ...` means what it says.
//
// The HARD ZERO is SPLIT-QUALIFIED at both of THIS CENSUS's two sites — not at
// every splitter in Go — and it is asserted rather than floored because it is not
// a population to be kept healthy: it is the debt, and the whole claim of the
// projection-channel conversion is that production traffic no longer produces any
// AT THESE ARMS. A non-zero means either an un-migrated producer has started
// carrying dotted text where a triple belongs, or a machinery-minted label has
// acquired a dot. The failure text names both readings because the fix differs —
// and for the second it names the RULING rather than an escalation, since Java
// settles what a nameless projection output resolves to (nothing).
func AssertNameSplitCensus(w io.Writer, floors *NameSplitFloors) bool {
	counts, witnesses := NameSplitCensus()
	return assertNameSplitCounts(w, counts, witnesses, floors)
}

// assertNameSplitCounts is the decision, split from the process-global counters
// so both failure directions can be exercised without a full corpus run — the
// same split assertDottedLegQualifierCounts makes, for the same reason.
func assertNameSplitCounts(w io.Writer, counts [nameSplitSiteCount][nameSplitClassCount]int, witnesses []string, floors *NameSplitFloors) bool {
	failed := false

	fmt.Fprintf(w, "[sqldriver real-FDB corpus] translator name split (per resolution decision):\n")
	for s := NameSplitSite(0); s < nameSplitSiteCount; s++ {
		total := 0
		for c := NameSplitClass(0); c < nameSplitClassCount; c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(w, "  %-18s calls %d | segmented %d | SPLIT-QUALIFIED %d | splitBare %d\n",
			s, total, counts[s][NameSplitSegmented],
			counts[s][NameSplitQualified], counts[s][NameSplitBare])
	}
	for _, wit := range witnesses {
		fmt.Fprintf(w, "    SPLIT-QUALIFIED witness: %s\n", wit)
	}

	for s := NameSplitSite(0); s < nameSplitSiteCount; s++ {
		if n := counts[s][NameSplitQualified]; n != 0 {
			failed = true
			fmt.Fprintf(w, "FAIL: %s manufactured a qualifier by slicing a rendered name %d time(s).\n"+
				"  This population is asserted at ZERO at THIS ARM (it says nothing about the\n"+
				"  uninstrumented splitters named in name_split_census.go's header): every\n"+
				"  parsed channel carries the parser's segment triple, so a rendered name\n"+
				"  reaching THIS arm with a dot in it is one of two things:\n"+
				"    (a) an UN-MIGRATED producer now feeding dotted text where a triple belongs\n"+
				"        — migrate it (logical.ColumnRefFor); or\n"+
				"    (b) a projection MACHINERY label (star-body normalization, translateScan)\n"+
				"        that has acquired a dot. That label has no parse tree behind it.\n"+
				"  DO NOT WIDEN THIS ZERO in either case, and for (b) do not escalate either —\n"+
				"  the behaviour is already RULED. Java skips a projection output that carries\n"+
				"  no Identifier before any name comparison (SemanticAnalyzer.java:459-461), so\n"+
				"  a machinery-minted label is NOT name-resolvable and must not resolve by any\n"+
				"  spelling — least of all by this arm reading a dot inside it as a qualifier\n"+
				"  boundary. The correct behaviour here is to DECLINE. What is still open is\n"+
				"  only whether the star-body normalization should stop minting such labels at\n"+
				"  all; that is upstream of this arm and does not change what this arm does.\n"+
				"  Witnesses above name the spellings.\n", s, n)
		}
	}

	if floors != nil {
		for s := NameSplitSite(0); s < nameSplitSiteCount; s++ {
			total := 0
			for c := NameSplitClass(0); c < nameSplitClassCount; c++ {
				total += counts[s][c]
			}
			if total < floors.Calls[s] {
				failed = true
				fmt.Fprintf(w, "FAIL: %s reported %d call(s), below its floor of %d — the arm has gone\n"+
					"  DARK. A splitting arm at zero reads exactly like a splitting arm measured\n"+
					"  clean, and this census's SPLIT-QUALIFIED zero is worthless over an empty\n"+
					"  population. Either the shapes that drive it stopped being planned, or the\n"+
					"  recorder was dropped from the arm.\n", s, total, floors.Calls[s])
			}
			split := counts[s][NameSplitBare] + counts[s][NameSplitQualified]
			switch f := floors.Split[s]; {
			case f > 0 && split < f:
				failed = true
				fmt.Fprintf(w, "FAIL: %s entered a SPLITTING arm %d time(s), below its split floor of\n"+
					"  %d. Its total call count can stay healthy on SEGMENTED traffic alone, so\n"+
					"  this is the floor — and the only floor — that says the arms this census's\n"+
					"  hard zero is a zero OVER are still being reached.\n", s, split, f)
			case f == 0 && split > 0:
				failed = true
				fmt.Fprintf(w, "FAIL: %s declares a split floor of 0 — WATCHED, NOT PROVEN, meaning its\n"+
					"  splitting arms were measured empty over this corpus and are covered by a\n"+
					"  unit wiring pin instead. They are no longer empty: %d split call(s) now\n"+
					"  arrive here. This is not a defect, it is the declaration going stale.\n"+
					"  Re-read the arm, then raise this floor to a real number so the corpus\n"+
					"  starts carrying the guarantee the unit pin was standing in for.\n", s, split)
			}
		}
	}
	return failed
}
