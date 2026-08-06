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
// second is RFC-197 debt. So the population that actually has to fall to zero —
// the RE-SPLITS — had no counter anywhere, and a regrown splitter raised none.
//
// That gap was load-bearing rather than cosmetic. The "110 calls → 3 → 0"
// progression quoted for the projection-channel conversion, in TODO.md, in
// road-to-prod.md and in the comment at the flat baker's own re-split arm, was
// SCRATCH measurement: a number obtained once by hand, written into prose, and
// thereafter re-quoted rather than re-read. This file is what makes those
// figures instrument readings, and what makes a regrowth red instead of silent.
//
// WHAT IS COUNTED. One call per resolution decision at each of the two arms that
// can still slice a rendered name, bucketed by WHICH REPRESENTATION DECIDED:
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
// So the OPEN question is not "what should happen" — Java answers that — but
// whether Go's star-body normalization should be changed to match, which moves a
// live projection channel and is the owner's call. What this census does is
// remove the way that question could be answered BY ACCIDENT: the day a dotted
// label reaches either arm, SPLIT-QUALIFIED leaves zero and the assertion fires
// with the decision named in its message, instead of the arm quietly treating
// the label's dot as a qualifier and settling it.
//
// GATED by LegIdentityCensusEnabled. Counts are per CALL.

// NameSplitSite is one translator arm that can still recover a qualifier by
// slicing a rendered name.
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
}

// AssertNameSplitCensus checks the census's hard zero and its floors, writing a
// report to w. Returns TRUE if anything FAILED — the convention its siblings on
// this path use, so a call site reading `if failed := ...` means what it says.
//
// The HARD ZERO is SPLIT-QUALIFIED at every site, and it is asserted rather than
// floored because it is not a population to be kept healthy — it is the debt,
// and the whole claim of the projection-channel conversion is that production
// traffic no longer produces any. A non-zero here is not a regression in the
// ordinary sense: it means either an un-migrated producer has started carrying
// dotted text where a triple belongs, or a machinery-minted label has acquired a
// dot and the arm is about to answer the star-body addressing question by
// accident. The failure text names both readings, because the fix differs.
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
				"  This population is asserted at ZERO: every parsed channel carries the\n"+
				"  parser's segment triple, so a rendered name reaching a splitting arm with a\n"+
				"  dot in it is one of two things, and they need different fixes:\n"+
				"    (a) an UN-MIGRATED producer now feeding dotted text where a triple belongs\n"+
				"        — migrate it (logical.ColumnRefFor), do not widen this zero; or\n"+
				"    (b) a projection MACHINERY label (star-body normalization, translateScan)\n"+
				"        that has acquired a dot. That label has no parse tree behind it, so\n"+
				"        this arm treating its dot as a qualifier ANSWERS BY ACCIDENT the open\n"+
				"        question of whether a star-projected body column is leg-addressable.\n"+
				"        Java's guidance is that an unnamed output is not name-resolvable at\n"+
				"        all (SemanticAnalyzer.java:459-461); making Go match is an owner\n"+
				"        decision, and this assertion exists so it is made deliberately.\n"+
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
		}
	}
	return failed
}
