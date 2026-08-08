package sqldriver_test

import (
	"flag"
	"fmt"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// assertDottedRowTypeProducerCensus gates the dotted row-type producer census
// over this corpus.
//
// It exists to make this census agree with the rest of the family on ONE point:
// a POPULATION FLOOR is meaningless under `-test.run` narrowing, and asserting it
// anyway produces a spurious red for anyone running a focused test.
//
// The floor was previously passed inline at the call site with no narrowing
// check, while assertOrientationGateCensus, assertProjectionMergeCensus and their
// siblings all skip theirs. Measured: `go test ./pkg/relational/sqldriver/
// -run TestFDB_AggregateIndexVacatedGroup` reported
// `DOTTED ROW-TYPE PRODUCER CENSUS FAIL: 14 derivation(s) over the whole run,
// want >= 100` — a red produced entirely by the filter, on a run where every
// selected test passed. A gate that reds on a narrowed run teaches people to
// ignore it, which costs exactly the signal the floor exists to give.
//
// What is NOT skipped: nothing, today, and that is worth stating rather than
// leaving implicit. AssertDottedRowTypeProducerCensus asserts ONLY floors — with
// a nil floor it is a no-op by construction, pinned by
// TestDottedRowTypeProducerCensus_NoFloorNeverFails.
//
// THE SECOND FLOOR, AND WHY IT IS A FLOOR RATHER THAN A ZERO. This gate used to
// pass one number, Derivations, and record in prose that the census's "usable
// finding" — a ZERO on the DOTTED counter — was both unasserted and FALSE:
// measured over this corpus, `DOTTED 683, plain 157511`. A live finding written
// into a comment is unreachable work, so it is now asserted instead.
//
// It is asserted in the direction the measurement left it. The claim the census
// was built to check was DOTTED == 0 (that `RecordConstructorValue.Type()` is not
// a producer of dotted `LEG.COL` rows); the measurement refuted it, so zero
// stopped being the steady state and the alarm flipped from "growth above zero"
// to COLLAPSE toward it. A hard zero here would now be unsatisfiable — it would
// red every full run against a fact nobody disputes.
//
// Because it is a floor over a traffic-dependent count and not a zero, it is
// withheld under narrowing with its sibling: a zero over a sum of non-negative
// counters is exact under any filter, but a floor of 50 over a corpus the filter
// just cut down is a red produced by the filter. The Derivations floor cannot
// stand in for it — plain outnumbers dotted about 230:1, so DOTTED can fall to
// zero with the total still three orders of magnitude clear.
func assertDottedRowTypeProducerCensus(w io.Writer) bool {
	// Both floored an order of magnitude below their full-corpus measurements.
	// Four readings, DOTTED then plain: 683/157511, 841/157935, 681/157699 and
	// 743/157905 — an observed DOTTED spread of 681..841, which is what "the
	// integer is corpus-traffic dependent" looks like and why 50 has that much
	// headroom below it.
	//
	// The last of those is the run that first exercised the DOTTED floor rather
	// than merely supplying a number for it: whole corpus, floors NOT withheld,
	// 1487 tests passed. A floor justified only by readings taken before it
	// existed has never been shown to pass anything.
	floor := &values.DottedRowTypeProducerFloor{Derivations: 100, Dotted: 50}
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "dotted row-type producer census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The census still reports its counts "+
			"above; only the whole-corpus floors are withheld.\n", f.Value.String())
		floor = nil
	}
	return values.AssertDottedRowTypeProducerCensus(w, floor)
}
