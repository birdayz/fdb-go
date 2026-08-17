package expressions

import (
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSharedReferenceSurvivesConcurrentFlowedTypeReads pins that deriving a
// quantifier's flowed object value does not WRITE to the Reference in a way two
// goroutines can collide on.
//
// The property is not "the planner is concurrent" — it is not; the task loop is
// sequential. It is that a Reference is an ordinary handle, and
// RequireFlowedObjectValue sits under nearly every expression CONSTRUCTOR, so
// "constructing two expressions over one reference" is a shape that tests and
// helpers reach for freely. It was safe until the flowed-type memo existed:
// before that, the derivation only read the member list.
//
// A FRESH REFERENCE PER ROUND IS THE WHOLE DESIGN. Sharing ONE reference across
// all iterations — the shape this test had first — leaves the memo filled after
// the first hit, so every later iteration is a read-only hit and the entire
// collision window is the single write at t=0. That is enough to be detected but
// it is INCIDENTAL: it depends on the scheduler interleaving one write against the
// reads. Minting a reference per round makes the window structural, one first-write
// race per round, so the detector does not rest on timing.
//
// Both shapes were measured against the mutation this pins — the memo's
// atomic.Pointer reverted to a plain *flowedTypeMemo, which is the shape that
// shipped the race — and on this machine both reddened 15 runs in 15. So the
// restructure buys robustness, not a red rate: the number to state is that the
// mutation is caught 15/15 at goroutines=8, rounds=32.
//
// It is worth nothing without -race — the whole point is the detector.
func TestSharedReferenceSurvivesConcurrentFlowedTypeReads(t *testing.T) {
	t.Parallel()

	const goroutines = 8
	const rounds = 32

	for round := 0; round < rounds; round++ {
		// Fresh, so its memo is EMPTY and every goroutine below races the first
		// write rather than reading a filled slot.
		shared := InitialOf(mustExpression(
			NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))

		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		failures := make([]error, goroutines)
		types := make([]values.Type, goroutines)
		for g := 0; g < goroutines; g++ {
			done.Add(1)
			go func(slot int) {
				defer done.Done()
				start.Wait()
				// A fresh quantifier over the shared reference: this is what an
				// expression constructor does.
				qov, err := ForEachQuantifier(shared).RequireFlowedObjectValue()
				if err != nil {
					failures[slot] = err
					return
				}
				types[slot] = qov.FlowedType()
			}(g)
		}
		start.Done()
		done.Wait()

		for slot, err := range failures {
			if err != nil {
				t.Fatalf("round %d goroutine %d: %v", round, slot, err)
			}
		}
		// Non-vacuity: every goroutine must have reached the derivation and agreed
		// on the row, or a silently-skipped loop would make the race unreachable.
		for slot := range types {
			if types[slot] == nil {
				t.Fatalf("round %d goroutine %d never derived a flowed type", round, slot)
			}
			if !types[slot].Equals(types[0]) {
				t.Fatalf("round %d goroutine %d derived %s, goroutine 0 derived %s — one "+
					"shared reference reported two different rows",
					round, slot, values.DescribeType(types[slot]), values.DescribeType(types[0]))
			}
		}
	}
}
