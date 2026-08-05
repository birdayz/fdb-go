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

// TestExecuteStateRecursionSnapshotRestore pins the semantics the SQL paging
// loop depends on when it rolls a retried page's position back.
//
// The interesting cases are the ones a "subtract what you added" implementation
// would get wrong: a snapshot taken before anything was counted must restore to
// EMPTY (not be a no-op), and an attempt that RESET an id — which checkDepth
// does whenever an invocation ends or aborts — must have that reset undone too,
// which only wholesale replacement achieves.
func TestExecuteStateRecursionSnapshotRestore(t *testing.T) {
	t.Parallel()

	t.Run("empty snapshot restores to empty", func(t *testing.T) {
		t.Parallel()
		s := NewExecuteState(0)
		before := s.SnapshotRecursionLevels()
		if before != nil {
			t.Fatalf("snapshot of an untouched state = %v, want nil", before)
		}
		for i := 0; i < 5; i++ {
			s.IncrementRecursionLevel("k")
		}
		s.RestoreRecursionLevels(before)
		if got := s.IncrementRecursionLevel("k"); got != 1 {
			t.Fatalf("after restoring a nil snapshot the next level is %d, want 1: a nil "+
				"snapshot means 'nothing had been counted yet' and must restore to empty, "+
				"not be treated as 'no state to restore'", got)
		}
	})

	t.Run("restores mid-flight counts", func(t *testing.T) {
		t.Parallel()
		s := NewExecuteState(0)
		s.IncrementRecursionLevel("k") // 1
		s.IncrementRecursionLevel("k") // 2
		snap := s.SnapshotRecursionLevels()
		for i := 0; i < 10; i++ {
			s.IncrementRecursionLevel("k")
		}
		s.RestoreRecursionLevels(snap)
		if got := s.IncrementRecursionLevel("k"); got != 3 {
			t.Fatalf("next level after restore = %d, want 3 (the snapshot held 2): the retried "+
				"work must re-derive exactly the levels it walked, not stack them on top", got)
		}
	})

	t.Run("undoes a reset performed after the snapshot", func(t *testing.T) {
		t.Parallel()
		s := NewExecuteState(0)
		s.IncrementRecursionLevel("k") // 1
		s.IncrementRecursionLevel("k") // 2
		snap := s.SnapshotRecursionLevels()
		// checkDepth frees an entry when an invocation ends or aborts; a failed
		// attempt can therefore leave an id LOWER than the snapshot, not higher.
		s.ResetRecursionLevel("k")
		s.RestoreRecursionLevels(snap)
		if got := s.IncrementRecursionLevel("k"); got != 3 {
			t.Fatalf("next level = %d, want 3: the failed attempt RESET this id, so a "+
				"subtract-what-you-added rollback would leave it at 0 and the retry would "+
				"under-count. Only wholesale replacement restores it", got)
		}
	})

	t.Run("snapshot is decoupled from later mutation", func(t *testing.T) {
		t.Parallel()
		s := NewExecuteState(0)
		s.IncrementRecursionLevel("k")
		snap := s.SnapshotRecursionLevels()
		s.IncrementRecursionLevel("k")
		s.IncrementRecursionLevel("other")
		s.RestoreRecursionLevels(snap)
		if got := s.IncrementRecursionLevel("other"); got != 1 {
			t.Fatalf("an id first seen AFTER the snapshot is at %d, want 1 after restore: the "+
				"snapshot must not alias the live map", got)
		}
		// Restoring twice from the same snapshot must be stable.
		s.RestoreRecursionLevels(snap)
		if got := s.IncrementRecursionLevel("k"); got != 2 {
			t.Fatalf("restoring the same snapshot twice gave next level %d, want 2: the "+
				"snapshot was mutated by a previous restore", got)
		}
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		t.Parallel()
		var s *ExecuteState
		if got := s.SnapshotRecursionLevels(); got != nil {
			t.Fatalf("nil receiver snapshot = %v, want nil", got)
		}
		s.RestoreRecursionLevels(map[any]int{"k": 3}) // must not panic
	})
}
