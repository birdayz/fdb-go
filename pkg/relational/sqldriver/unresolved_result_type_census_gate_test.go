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
	// MinReads is floored an order of magnitude below the measurement (15,909
	// classified reads originally; 15,074 on a re-run: 14,939 resolved + 135
	// unresolved).
	//
	// MinSites is an EXACT count, not a floor with headroom, because the whole
	// corpus reports exactly five deciding sites and each one is a distinct
	// consumer RFC-213 is denominated against — losing any of them is the event
	// this arm exists for. The same run that set it also shows why a read floor
	// cannot do this job: predicatesFilterIsFullPKPointProbe alone contributes
	// 13,651 of the 15,074 reads (91%), so the other four consumers could go
	// silent together and MinReads would still be clear by an order of magnitude.
	//
	//	bakedIntersectionKeys                 resolved 412    UNRESOLVED 0
	//	distinctKeyColumns                    resolved 4      UNRESOLVED 31
	//	planBuriedLegConcat                   resolved 252    UNRESOLVED 0
	//	planRowRecordType                     resolved 620    UNRESOLVED 104
	//	predicatesFilterIsFullPKPointProbe    resolved 13651  UNRESOLVED 0
	floors := &cascades.UnresolvedResultTypeFloors{MinReads: 1000, MinSites: 5}
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "unresolved-result-type census: reached-consumers floors NOT checked "+
			"(-test.run=%q narrowed the corpus; they describe the whole suite). "+
			"The census still reports its counts above; there is no hard zero here to run "+
			"over the reduced population.\n", f.Value.String())
		floors = nil
	}
	return cascades.AssertUnresolvedResultTypeCensus(w, floors)
}
