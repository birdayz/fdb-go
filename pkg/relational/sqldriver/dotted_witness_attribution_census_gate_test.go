package sqldriver_test

import (
	"flag"
	"fmt"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// assertDottedWitnessAttributionCensus gates the dotted-witness attribution
// census over this corpus.
//
// Same defect as its sibling next door, found the same way and fixed the same
// way: a POPULATION floor asserted under `-test.run` narrowing reds on the
// filter rather than on a defect. Measured on CLEAN master, no local changes,
// `go test ./pkg/relational/sqldriver/ -run TestFDB_AggregateIndexVacatedGroup`:
//
//	DOTTED-WITNESS ATTRIBUTION FAIL: 0 inner-leg title(s) minted, want >= 1
//	DOTTED ROW-TYPE PRODUCER CENSUS FAIL: 14 derivation(s) over the whole run
//
// Both fire, neither is a defect, and every selected test passed. Two censuses
// out of roughly a dozen had drifted from the family's convention — the rest
// already print a "population floors NOT checked" notice and withhold. Fixing
// one and leaving the other would have left the narrowed run just as red and
// just as uninformative.
//
// WHAT IS STILL ASSERTED under narrowing, and why the split is the right one:
// the COLLISION check — a name answered under more than one owner — is a hard
// zero, and a zero over a sum of non-negative counters is exact under any
// filter. Narrowing can only make it fail, never falsely pass, so it must not be
// skipped. AssertDottedWitnessAttribution runs it before consulting floors and
// returns early on `floors == nil`, so passing nil gets exactly that split with
// no change to the census itself.
//
// The MINTED floor is the one withheld, and it is worth naming what that costs.
// Its own text says it is what "keeps the zero honest" — proof the producers are
// still registering, so an empty observed population reads as a quiet arm rather
// than a dead instrument. That proof is a whole-corpus claim and a narrowed run
// cannot make it; withholding it says so out loud instead of asserting something
// the run never had the population to decide.
func assertDottedWitnessAttributionCensus(w io.Writer) bool {
	// The OBSERVED floor is retired with the arm: RFC-212 §11.3's retitling drove
	// the dotted arm to ZERO answers, so a floor on observed names is now
	// unsatisfiable by construction. The MINTED floor stays and is what keeps the
	// zero honest — it proves the producers are still registering, so an empty
	// observed population means the arm is quiet rather than the instrument dead.
	floors := &values.DottedWitnessFloors{Minted: 1}
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "dotted-witness attribution census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus; the minted floor describes the whole suite). "+
			"The COLLISION hard zero still runs, over whatever population this filter "+
			"reached — at zero it holds VACUOUSLY, and only the unfiltered suite makes it "+
			"a proof.\n", f.Value.String())
		floors = nil
	}
	return values.AssertDottedWitnessAttribution(w, floors)
}
