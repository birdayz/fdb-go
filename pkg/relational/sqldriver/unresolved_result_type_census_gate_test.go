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
func assertUnresolvedResultTypeCensus(w io.Writer) bool {
	// Floored an order of magnitude below the measured 15,909 classified reads.
	minReads := 1000
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "unresolved-result-type census: reached-consumers floor NOT checked "+
			"(-test.run=%q narrowed the corpus; the floor describes the whole suite). "+
			"The census still reports its counts above; there is no hard zero here to run "+
			"over the reduced population.\n", f.Value.String())
		minReads = 0
	}
	return cascades.AssertUnresolvedResultTypeCensus(w, minReads)
}
