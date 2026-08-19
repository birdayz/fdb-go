package sqldriver_test

import (
	"flag"
	"fmt"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// assertUnresolvedResultTypeCensus gates RFC-213's reached-consumers floor over
// this corpus.
//
// Third of three censuses found asserting a whole-corpus POPULATION floor under
// `-test.run` narrowing, where it reds on the filter rather than on a defect.
// They were found one at a time, by re-running a narrowed suite after each fix
// and watching the next one surface — which is the whack-a-mole this file's
// siblings and the DST recipe both warn about, so the last step was to enumerate
// instead: of the sixteen assertions in the census block, fourteen route through
// a local wrapper and every one of those already guarded; exactly two called a
// package assert directly, and this was the one carrying a floor.
//
// Measured under `-run TestFDB_AggregateIndexVacatedGroup`, every selected test
// passing:
//
//	UNRESOLVED-RESULT-TYPE CENSUS FAIL: 0 classified read(s), want >= 1000.
//
// This one is skipped ENTIRELY when narrowed, with no residual assertion, and
// that is a property of what it checks rather than an inconsistency with its
// siblings. Its own call site says it best — "there is no zero to defend: the
// unresolved reads ARE the defect and their count is a measurement, not a
// contract". The floor exists only so that a future "unresolved is 0" cannot be
// confused with the consumers having gone dark. That is a whole-corpus claim
// end to end, so a narrowed run has nothing here it can honestly decide.
//
// Where a census DOES carry a hard zero — a collision count, a partition — the
// zero survives narrowing and only the floor is withheld, because a zero over a
// sum of non-negative counters is exact under any filter. See the dotted-witness
// gate next door for that shape.
// BOTH floors are whole-corpus population claims, so both are withheld together
// when narrowed. MinSites is not a milder version of MinReads — it is the arm
// that sees a census kept alive by one hot consumer while the others go dark —
// but it is just as filter-dependent, so a narrowed run has nothing here it can
// honestly decide either.
//
// The floors themselves are pinned as a UNIT, away from this corpus and away from
// the process globals, in the cascades package's
// unresolved_result_type_census_test.go: every class, every floor and the nil
// vacuity guard. A corpus reading exercises only the arms the corpus reaches, so
// it is not a substitute for that.
func assertUnresolvedResultTypeCensus(w io.Writer) bool {
	// MinSites is an EXACT count, not a floor with headroom: each site is a
	// distinct consumer RFC-213 is denominated against, and losing one is the
	// event this arm exists for. It reads FOUR, and the fifth was RETIRED rather
	// than lost — this is the reconciliation that retirement requires, not the
	// floor being lowered to match a run.
	//
	// The retired consumer is predicatesFilterIsFullPKPointProbe, and it was by
	// far the hottest: 14,166 of 15,589 classified reads (91%). It no longer
	// reads a plan RESULT TYPE at all. Its cardinality proof now goes through
	// scan.ProvidedOutputLayout()/Carrier() — the scanned row's exact physical
	// layout — because a result type could not tell this scan's own primary key
	// from a correlated outer reference that happens to share a leaf name, and
	// `ID` is the primary key of nearly every table in this corpus. That is a
	// consumer converted off the unresolved-type channel, which is what RFC-213
	// wanted, so its disappearance from this census is the conversion landing and
	// not a site going dark.
	//
	// MinReads moves WITH it. The floor sat an order of magnitude below a
	// measurement the retired consumer supplied 91% of; leaving it at 1000
	// against a 1564-read corpus would leave the collapse guard a factor of 1.6
	// from the measurement, which fails on churn instead of on a defect. It is
	// re-derived the same way it was originally: an order of magnitude below what
	// the four remaining consumers actually report.
	//
	// A SECOND RETIREMENT (RFC-235), and it RESOLVES rather than complicates the
	// shape of this floor. `planRowRecordType` and `planBuriedLegConcat` also
	// called the recorder, and the three-quantifier NLJ arm was their only live
	// caller. They were briefly orphaned — reachable from tests alone, reporting
	// nothing over the corpus — which WOULD have made a downward-watching floor
	// the wrong instrument, because an orphan's steady state is zero and its
	// revival moves the count UP. That ambiguity is gone: both functions are
	// DELETED, so the census has exactly the two consumers below and no dormant
	// third to confuse with a new one.
	//
	// MinSites 2 is therefore a COMPLETE collapse guard here rather than a partial
	// one, and it needs no asserted site set to disambiguate a revival: a count of
	// 3 can only be a genuinely new consumer, which is a change to this census's
	// population and belongs in this comment before it belongs in the number.
	//
	// The two remaining sites, whole corpus, floors NOT withheld:
	//
	//	bakedIntersectionKeys                 resolved 412    UNRESOLVED 0
	//	distinctKeyColumns                    resolved 70     UNRESOLVED 0
	//
	// BOTH READ RESOLVED, and so did the two that have now retired. That zero is
	// the RFC-213 payoff arriving. That zero is the RFC's payoff arriving, and it is
	// deliberately NOT asserted here: this census has no hard zero, because the
	// unresolved reads ARE the defect and their count is a measurement rather
	// than a contract. What is asserted is that the consumers are still reached,
	// so a future zero cannot be confused with the instrument going dark.
	floors := &cascades.UnresolvedResultTypeFloors{MinReads: 48, MinSites: 2}
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "unresolved-result-type census: reached-consumers floors NOT checked "+
			"(-test.run=%q narrowed the corpus; they describe the whole suite). "+
			"The census still reports its counts above; there is no hard zero here to run "+
			"over the reduced population.\n", f.Value.String())
		floors = nil
	}
	return cascades.AssertUnresolvedResultTypeCensus(w, floors)
}
