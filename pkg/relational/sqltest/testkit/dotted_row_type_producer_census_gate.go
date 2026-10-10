package testkit

import (
	"fmt"
	"io"

	"fdb.dev/pkg/internal/querycensus"
)

// assertDottedRowTypeProducerCensus detects lost derivation and dotted-name traffic.
// Population floors apply only to the whole corpus, not narrowed runs.
func assertDottedRowTypeProducerCensus(w io.Writer) bool {
	// Keep a separate dotted floor: plain derivations can hide its disappearance.
	floor := &querycensus.DottedRowTypeProducerFloor{Derivations: 100, Dotted: 50}
	if f := corpusNarrowing(); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "dotted row-type producer census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The census still reports its counts "+
			"above; only the whole-corpus floors are withheld.\n", f.Value.String())
		floor = nil
	}
	return querycensus.AssertDottedRowTypeProducerCensus(w, floor)
}
