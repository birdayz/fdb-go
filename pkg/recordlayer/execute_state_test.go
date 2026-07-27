package recordlayer

import "testing"

// TestExecuteState_ResetRecursionLevel pins ResetRecursionLevel's direct
// contract: it zeroes (by deleting) the count tracked under id, a nil
// receiver is a no-op, and resetting an id that was never incremented is
// also a no-op — none of these should panic or leave stale state behind.
// See ResetRecursionLevel's doc comment for why this exists: it is the
// invocation-boundary reset that recursive_union_cursor.go's
// newRecursiveUnionCursor calls when a fresh (continuation==nil) invocation
// starts, so independent invocations of the same recursive-CTE plan node
// within one statement don't have their level counts summed together.
func TestExecuteState_ResetRecursionLevel(t *testing.T) {
	t.Parallel()

	var nilState *ExecuteState
	nilState.ResetRecursionLevel("id") // must not panic

	s := NewExecuteState(0)
	s.ResetRecursionLevel("never-touched") // must not panic on an empty map

	id := "plan-node"
	if got := s.IncrementRecursionLevel(id); got != 1 {
		t.Fatalf("IncrementRecursionLevel: got %d, want 1", got)
	}
	if got := s.IncrementRecursionLevel(id); got != 2 {
		t.Fatalf("IncrementRecursionLevel: got %d, want 2", got)
	}

	s.ResetRecursionLevel(id)

	if got := s.IncrementRecursionLevel(id); got != 1 {
		t.Fatalf("after ResetRecursionLevel, IncrementRecursionLevel: got %d, want 1 (reset did not zero the count)", got)
	}

	// A different id's count is untouched by resetting id.
	other := "other-plan-node"
	s.IncrementRecursionLevel(other)
	s.IncrementRecursionLevel(other)
	s.ResetRecursionLevel(id)
	if got := s.IncrementRecursionLevel(other); got != 3 {
		t.Fatalf("ResetRecursionLevel(id) must not affect a DIFFERENT id's count: got %d, want 3", got)
	}
}
