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
// It regressed exactly that way and was caught by CI, not here, because the
// existing test that shares a scanRef across parallel subtests
// (TestRelationalAliasCompleteness) only tripped -race on 4 runs out of 6. This
// one drives the collision deliberately, so it fails on the FIRST run under
// -race. It is worth nothing without -race — the whole point is the detector.
func TestSharedReferenceSurvivesConcurrentFlowedTypeReads(t *testing.T) {
	t.Parallel()

	shared := InitialOf(mustExpression(
		NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))

	const goroutines = 8
	const iterations = 32
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
			for i := 0; i < iterations; i++ {
				// A fresh quantifier per iteration, over the SHARED reference:
				// this is what an expression constructor does.
				qov, err := ForEachQuantifier(shared).RequireFlowedObjectValue()
				if err != nil {
					failures[slot] = err
					return
				}
				types[slot] = qov.FlowedType()
			}
		}(g)
	}
	start.Done()
	done.Wait()

	for slot, err := range failures {
		if err != nil {
			t.Fatalf("goroutine %d: %v", slot, err)
		}
	}
	// Non-vacuity: every goroutine must have reached the derivation and agreed
	// on the row, or a silently-skipped loop would make the race unreachable.
	for slot := 1; slot < len(types); slot++ {
		if types[slot] == nil {
			t.Fatalf("goroutine %d never derived a flowed type", slot)
		}
		if !types[slot].Equals(types[0]) {
			t.Fatalf("goroutine %d derived %s, goroutine 0 derived %s — one shared "+
				"reference reported two different rows",
				slot, values.DescribeType(types[slot]), values.DescribeType(types[0]))
		}
	}
}
