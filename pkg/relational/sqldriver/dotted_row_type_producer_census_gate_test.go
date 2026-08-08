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
// leaving implicit. AssertDottedRowTypeProducerCensus asserts ONLY the floor —
// with a nil floor it is a no-op by construction, pinned by
// TestDottedRowTypeProducerCensus_NoFloorNeverFails.
//
// Its failure text calls the census's "usable finding" a ZERO on its DOTTED
// counter, no code anywhere asserts that zero, AND THE ZERO IS FALSE. Measured
// over this corpus, one full run: `DOTTED 683, plain 157511`. The census header
// says it exists because the producer-set claim — that
// `RecordConstructorValue.Type()` is NOT a producer of dotted `LEG.COL` rows —
// "was asserted rather than measured". It has now been measured, 683 times, and
// it does not hold. Nothing surfaces that, because the floor is the only
// assertion and 683 clears it comfortably.
//
// That is a live finding about the leg-table population plan, not a defect in
// this gate, so it is recorded here rather than converted into an assertion: a
// zero asserted now would red the build without deciding what the producer set
// should be, which is a design question. Were the zero ever asserted, it would
// belong OUTSIDE this narrowing skip — a zero over a sum of non-negative
// counters is exact under any filter, so narrowing can only make it fail, never
// falsely pass. The floor is the opposite and must go.
func assertDottedRowTypeProducerCensus(w io.Writer) bool {
	floor := &values.DottedRowTypeProducerFloor{Derivations: 100}
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "dotted row-type producer census: population floor NOT checked "+
			"(-test.run=%q narrowed the corpus). The census still reports its counts "+
			"above; only the whole-corpus floor is withheld.\n", f.Value.String())
		floor = nil
	}
	return values.AssertDottedRowTypeProducerCensus(w, floor)
}
