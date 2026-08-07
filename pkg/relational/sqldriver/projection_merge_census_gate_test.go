package sqldriver_test

import (
	"flag"
	"fmt"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// ProjectionMergeRule's name-matching composition arm was DELETED (RFC-197,
// name-keyed): it selected an inner projection slot by matching the outer read's
// display name, which is the forbidden move — two inner slots may share one
// output name, and one slot may be addressed by two names.
//
// The deletion rests on a claim about the WHOLE relational surface: every outer
// read arriving at that rule carries its output ordinal, because the resolver
// bakes a projection-output reference before the rule sees it. That claim was
// measured over ./pkg/relational/... — FDB sqldriver, four conformance corpora,
// yamsql, rowdiff — at 897 firings and ZERO lazy reads.
//
// THE GUARD MUST COVER WHAT THE CLAIM COVERS. The first reader was placed in
// explaindiff alone, which is ~11% of that population, so a resolver-baking
// regression reachable only from sqldriver or yamsql shapes would not have redded
// anything. That is the same gap in miniature as the one this whole workstream
// exists to close: a number quoted from a run nobody committed. This is the
// sqldriver half; explaindiff keeps its own.
//
// It lives in TestMain rather than in a test for the reason every census on this
// path does: Go gives tests no ordering, so the population is only complete after
// m.Run().
func assertProjectionMergeCensus(w io.Writer) bool {
	floors := &projectionMergeFloors
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "projection-merge census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The lazy-read ZERO and the arm "+
			"partition still run over whatever population this filter reached.\n",
			f.Value.String())
		floors = nil
	}
	return cascades.AssertProjectionMergeCensus(w, floors)
}

// projectionMergeFloors gates the projection-merge census over this corpus.
//
// ORDER-OF-MAGNITUDE below the measurement, like every other per-site floor on
// this path: they catch the rule going dark, not a count that moves whenever a
// test file is added.
//
// The load-bearing assertion is NOT floored and is not listed here — the
// LazyOuterReads count is checked at ZERO unconditionally inside
// cascades.AssertProjectionMergeCensus, along with the arm partition. A
// configurable version of either would be pointless: the zero IS the claim, and
// the partition is what keeps a mis-wired counter from producing that zero for
// the wrong reason.
//
// Measured over the whole real-FDB corpus, one run:
//
//	firings=147 slots=187 baked=181 lazy=0 notComposable=6
var projectionMergeFloors = cascades.ProjectionMergeFloors{
	RuleFirings:         14,
	BakedSingleAccessor: 18,
}
