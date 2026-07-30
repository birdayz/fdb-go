package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
)

// The shapes below are the ones the redundancy pin proves and the one it does
// not. Rendered by hand rather than measured, because this test is about what the
// GATE does with two distinct shapes, not about how a shape is rendered — the
// renderer's own separation is pinned next to it, in
// TestMergedRowShape_SeparatesRowsTheAliasCannot.
const (
	provenShape   = "ST[0,3):ID,C,ARR|OT[3,5):ID,K"
	unprovenShape = "ST[0,3):ID,C,ARR|OT[3,5):ID,K|MA[5,8):ID,C,ARR"
)

// TestMergedLegExclusion_IsScopedToTheProvenShape is the gate-side half of the
// exclusion's scoping, and it exists because the population it needs cannot be
// produced on demand: a SECOND reader of the already-excused alias `ST`, out of a
// merged row the redundancy pin never ran. No query in the corpus makes one
// today. That is exactly why the gate's behaviour on it has to be pinned — the
// day some query does, the answer must be an alarm and not a shrug.
//
// The bare alias `ST` is a plain table name. Keyed on it, one proof about one
// join excuses every future multi-leg read of anything so named, load-bearing or
// not — the same alias-collision argument this census already accepted for
// CLASSIFYING reads (the corpus binds twenty unrelated legs under the outer name
// `X`), applied to excusing them.
func TestMergedLegExclusion_IsScopedToTheProvenShape(t *testing.T) {
	t.Parallel()

	proven := executor.MergedRowRead{Alias: "ST", Shape: provenShape}
	// A NEW reader: same alias, different merged row. Nothing proved this one.
	unproven := executor.MergedRowRead{Alias: "ST", Shape: unprovenShape}

	reads := map[executor.MergedRowRead]int{proven: 6, unproven: 2}
	redundant := map[executor.MergedRowRead]string{
		proven: "TestFDB_MergedLegBinding_ReaderShapeIsRedundant",
	}

	unexcusedReads, unexcusedNames, excusedReads, excusedNames := partitionMergedRowReads(reads, redundant)

	if unexcusedReads != 2 {
		t.Fatalf("a second ST reader out of an UNPROVEN merged row counted %d unexcused "+
			"read(s), want 2.\n"+
			"  reads:     %v\n"+
			"  redundant: %v\n"+
			"  At zero the gate is keyed on the bare alias again: one query's proof is\n"+
			"  excusing another query's reads because both legs happen to be called ST.\n"+
			"  A load-bearing reader arriving under a common table name would then be\n"+
			"  silently excused, which is the whole failure this keying exists to stop.",
			unexcusedReads, reads, redundant)
	}
	if excusedReads != 6 {
		t.Fatalf("the PROVEN shape's reads were not excused: %d excused, want 6 (%v).\n"+
			"  The exclusion must still cover what the pin actually ran, or the gate is\n"+
			"  permanently red on the corpus and says nothing.", excusedReads, excusedNames)
	}

	// The report has to NAME the shape, or a reader distinguished only by its
	// merged row is indistinguishable in the output from the excused one — and the
	// gate's remedy differs between those two cases.
	joined := strings.Join(unexcusedNames, "; ")
	if !strings.Contains(joined, unprovenShape) {
		t.Fatalf("the unexcused read did not report its merged-row shape: %q\n"+
			"  It shares its alias with an excused read, so the shape is the only thing\n"+
			"  in the gate's output that tells the two apart.", joined)
	}
	if excused := strings.Join(excusedNames, "; "); strings.Contains(excused, unprovenShape) {
		t.Fatalf("the UNPROVEN shape appears on the excused side of the report: %q\n"+
			"  Counting it unexcused while reporting it as excused sends the reader\n"+
			"  looking for a shape that is not there.", excused)
	}
}
