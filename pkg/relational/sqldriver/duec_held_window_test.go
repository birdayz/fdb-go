package sqldriver_test

import "testing"

// TestDuecWithHeldWindow drives every arm of the lost-window retry directly.
//
// The retry exists for a condition the box decides: FDB's 5s MVCC window, which
// the 100k-row fixture clears easily unless the race detector is taxing every
// memory access. So the corpus reaches this code only when the machine is slow
// enough — a green run is the one that never exercised it, and the first time
// it fires for real would otherwise be the first time it ran at all. Its
// FINAL-ATTEMPT arm is worse still: that one fires only when the window has
// stopped holding altogether, which is exactly the moment nobody wants to
// discover an untested branch.
//
// This drives all three: held-immediately, lost-then-held, and exhausted.
func TestDuecWithHeldWindow(t *testing.T) {
	t.Parallel()

	t.Run("held on the first attempt does not retry", func(t *testing.T) {
		t.Parallel()
		calls := 0
		got := duecWithHeldWindow(t, "probe", func(fatal bool) (string, bool) {
			calls++
			if fatal {
				t.Errorf("attempt %d was told it was the last; nothing had been lost yet", calls)
			}
			return "rows", false
		})
		if got != "rows" || calls != 1 {
			t.Errorf("got %q after %d attempts, want %q after 1", got, calls, "rows")
		}
	})

	t.Run("lost then held returns the held attempt", func(t *testing.T) {
		t.Parallel()
		calls := 0
		got := duecWithHeldWindow(t, "probe", func(bool) (int, bool) {
			calls++
			// Two losses, then a window that holds — the shape the race lane
			// actually produces, where the drain is only marginally over.
			return calls, calls <= 2
		})
		if got != 3 || calls != 3 {
			t.Errorf("got %d after %d attempts, want 3 after 3 — the value must come "+
				"from the attempt that HELD, not from a truncated one", got, calls)
		}
	})

	t.Run("the last attempt is told it is the last", func(t *testing.T) {
		t.Parallel()
		calls, sawFatal := 0, 0
		// Never holds. The bound is what stops this being an infinite spin, and
		// `fatal` is how the caller turns the final loss into a reported
		// failure instead of a silent one — so both are asserted here rather
		// than trusted.
		duecWithHeldWindow(t, "probe", func(fatal bool) (struct{}, bool) {
			calls++
			if fatal {
				sawFatal++
				// A real caller t.Fatalf's here; this one reports the window as
				// held so the loop can return and the arms stay assertable.
				return struct{}{}, false
			}
			return struct{}{}, true
		})
		if calls != duecWindowAttempts {
			t.Errorf("made %d attempts, want the %d-attempt bound", calls, duecWindowAttempts)
		}
		if sawFatal != 1 {
			t.Errorf("fatal was signalled %d times, want exactly 1 (only the last attempt)", sawFatal)
		}
	})
}
