package values

import (
	"fmt"
	"io"
)

// The census's REPORT and ASSERT halves live here rather than in one corpus
// harness, because there is more than one corpus. The real-FDB SQL suite drives
// the executor sites; the translator's own package drives the planning sites, and
// its corpus is where the witnesses that indicted a converted reader came from.
// Two harnesses asserting the same invariants means the invariants are stated
// once, here, and each harness supplies only the population floors that describe
// its own corpus.

// ReportLegIdentityCensus dumps every site's counts and witnesses. It reports
// unconditionally — a narrowed corpus still has numbers worth seeing, it just has
// no floors to check.
func ReportLegIdentityCensus(w io.Writer, label string) {
	fmt.Fprintf(w, "=== LEG IDENTITY CENSUS (%s) ===\n", label)
	for _, site := range LegIdentitySites() {
		c := LegIdentityCensusOf(site)
		fmt.Fprintf(w, "%-52s [%-10s] total=%-8d exact=%-8d foldOnly=%-4d neither=%-6d unstated=%-4d retiredDiverged=%d\n",
			site.String(), c.Channel, c.Total, c.ExactEqual, c.FoldOnlyEqual, c.Neither, c.Unstated,
			c.RetiredVerdictDivergent)
		if len(c.FoldOnlySamples) > 0 {
			fmt.Fprintf(w, "    fold-only witnesses: %v\n", c.FoldOnlySamples)
		}
		if len(c.NeitherSamples) > 0 {
			fmt.Fprintf(w, "    neither witnesses: %v\n", c.NeitherSamples)
		}
		if len(c.UnstatedSamples) > 0 {
			fmt.Fprintf(w, "    unstated witnesses: %v\n", c.UnstatedSamples)
		}
		if len(c.RetiredVerdictSamples) > 0 {
			fmt.Fprintf(w, "    retired-verdict divergences: %v\n", c.RetiredVerdictSamples)
		}
	}
}

// AssertLegIdentityCensus checks the census and reports whether it failed.
//
// The ZERO assertions hold over ANY population — a fold-only pair, a diverged
// text/identity pair, an unstated identity, a RETIRED-VERDICT divergence, or a
// mixed instrument is a defect whether the corpus was one query or the whole suite
// — so they run always. That is FIVE, and callers must enumerate all five when
// they announce what a narrowed run still checks: an enumeration that omits the
// retired-verdict zero omits the one assertion that measures the conversion
// itself, which is the reason this census exists. Only the population FLOORS
// describe a particular corpus, and those are checked only for the sites the
// caller supplies: pass nil to assert the zeros over a narrowed run. Skipping the
// zeros along with the floors is how a filtered invocation used to report "floors
// not checked" while quietly also not checking the five things that are always
// checkable.
func AssertLegIdentityCensus(w io.Writer, floors map[LegIdentitySite]int64) bool {
	return AssertLegIdentityCensusWith(w, LegIdentityExpectations{Floors: floors})
}

// LegIdentityExpectations is what one CORPUS expects of the census. It exists
// because a site can sit at zero for three different reasons and they need three
// different guards — a floor watches for COLLAPSE, and once zero is the steady
// state the alarm INVERTS to growth.
type LegIdentityExpectations struct {
	// Floors is the population each site must report over the UNFILTERED corpus.
	// Dropped under a -test.run filter, which is why it cannot carry a zero: a
	// guard that vanishes under narrowing cannot watch a revival.
	Floors map[LegIdentitySite]int64

	// DeclaredEmpty names sites this corpus MEASURED at zero although their
	// recorders still stand, mapped to why. A DISPLACED reader is the case: the
	// code is reachable in principle and this suite no longer routes through it.
	//
	// Checked in the STALE direction — a declared site reporting traffic FAILS,
	// so the declaration cannot outlive the condition that made it honest. The
	// check runs under a filter too, and safely: a narrowed run is a SUBSET, so
	// it cannot exceed a population the whole suite measured at zero.
	DeclaredEmpty map[LegIdentitySite]string

	// Retired names sites whose recorder has NO production caller at all, mapped
	// to why. Unlike DeclaredEmpty this is a fact about the TREE, so its failure
	// text tells the reader that something came back rather than that a corpus
	// moved.
	Retired map[LegIdentitySite]string
}

// AssertLegIdentityCensusWith is AssertLegIdentityCensus with the zero-side
// guards a corpus may declare. See LegIdentityExpectations.
func AssertLegIdentityCensusWith(w io.Writer, exp LegIdentityExpectations) bool {
	failed := false
	floors := exp.Floors
	for _, site := range LegIdentitySites() {
		c := LegIdentityCensusOf(site)
		if why, ok := exp.Retired[site]; ok && c.Total != 0 {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported Total = %d, want 0 — it is RETIRED.\n"+
				"  %s\n"+
				"  This site's alarm is INVERTED: zero is the steady state and traffic is the\n"+
				"  finding. Do not add a floor to match — find what reintroduced the reader.\n",
				site, c.Total, why)
		}
		if why, ok := exp.DeclaredEmpty[site]; ok && c.Total != 0 {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s declares an EMPTY population and reported\n"+
				"  Total = %d. %s\n"+
				"  This is not a defect, it is the DECLARATION GOING STALE: the reader was\n"+
				"  DISPLACED, not deleted, and the corpus has started routing through it again.\n"+
				"  Re-read the site, then replace this declaration with a real floor so the\n"+
				"  population is watched for collapse the way its siblings are.\n",
				site, c.Total, why)
		}
		if floor, ok := floors[site]; ok && c.Total < floor {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported Total = %d, want >= %d.\n"+
				"  A site the suite no longer reaches makes every zero asserted about it\n"+
				"  VACUOUS. Either the shapes that drive it stopped being planned, or a\n"+
				"  reader was routed around. Find out which before adjusting this floor.\n",
				site, c.Total, floor)
		}
		if c.Channel == LegChannelMixed {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s recorded in BOTH namespaces.\n"+
				"  Its counts then describe neither comparison. A site records the pair its\n"+
				"  own comparison evaluates — RecordLegIdentityConversion for a values.SameLeg\n"+
				"  decision, RecordLegIdentityComparison for a genuine text decision — and\n"+
				"  exactly one of them.\n", site)
		}
		if c.FoldOnlyEqual != 0 {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported FoldOnlyEqual = %d, want 0.\n"+
				"  A leg is STORED under one spelling and LOOKED UP under another, so this\n"+
				"  site being exact (values.SameLeg) binds a different row than folding would.\n"+
				"  Witnesses: %v.\n"+
				"  The fix belongs at the PRODUCER that normalizes one side, not in the\n"+
				"  comparison: SameLeg is exact because the alias namespaces here are\n"+
				"  case-DISJOINT (a quoted \"q$5\" must not forge a planner-minted q$5).\n",
				site, c.FoldOnlyEqual, c.FoldOnlySamples)
		}
		if c.Unstated != 0 {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported Unstated = %d of Total = %d, want 0.\n"+
				"  One side of the comparison carries NO identity, so values.SameLeg declines\n"+
				"  it unconditionally and the read falls to whatever the caller does on a miss.\n"+
				"  A leg's identity is SOURCED at construction from its quantifier; an unstated\n"+
				"  one means a producer had no identifier to thread and left it blank rather\n"+
				"  than being fixed. Witnesses: %v.\n",
				site, c.Unstated, c.Total, c.UnstatedSamples)
		}
		if c.RetiredVerdictDivergent != 0 {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported RetiredVerdictDivergent = %d of Total = %d, want 0.\n"+
				"  The identity comparison this site now makes DISAGREES with the text\n"+
				"  comparison it replaced, on this corpus. That is a row-binding change, not a\n"+
				"  representation change: a retired=match divergence turned a resolving read\n"+
				"  into a decline, a retired=decline one bound a leg the old code walked past.\n"+
				"  Either the producers on the two sides must be reconciled so the identities\n"+
				"  agree, or the flip is a FIX and needs the shape pinned by a test that shows\n"+
				"  the new answer is the right one. Witnesses: %v.\n",
				site, c.RetiredVerdictDivergent, c.Total, c.RetiredVerdictSamples)
		}
		if LegSiteNeitherMustBeZero(site) && c.Neither != 0 {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported Neither = %d of Total = %d, want 0.\n"+
				"  A leg's TEXT and its IDENTITY name different things. The readers compare\n"+
				"  through the identity while the dotted channel still reads the text, so the\n"+
				"  two channels have DIVERGED and Name can no longer be retired by asserting\n"+
				"  they agree. Find the producer that sets one without the other — an empty\n"+
				"  Alias against a stated Name lands here too. Witnesses: %v.\n",
				site, c.Neither, c.Total, c.NeitherSamples)
		}
	}
	return failed
}
