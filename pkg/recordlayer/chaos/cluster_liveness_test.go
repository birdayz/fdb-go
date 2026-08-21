package chaos

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// EVERY ARM OF THE LIVENESS LATCH FIRES.
//
// The latch's whole purpose is to behave differently in a situation the normal
// suite never reaches -- the shared container being dead. A corpus run
// therefore exercises exactly none of it, which is the shape that ships
// untested and gets read as a finding the first time it fires for real. Driven
// here directly, with the package global swapped out so the arms are
// independent of whatever the suite did.
func TestClusterLivenessLatchArms(t *testing.T) {
	// NOT t.Parallel(): this test swaps the package-level latch, and
	// BUILD.bazel pins -test.parallel=1 anyway.
	saved := clusterGone
	t.Cleanup(func() { clusterGone = saved })

	t.Run("death signatures latch", func(t *testing.T) {
		for _, sig := range clusterDeathSignatures {
			clusterGone = atomic.Value{}
			// Wrapped, because a real error arrives under layers of %w.
			err := fmt.Errorf("save: %w", fmt.Errorf("op: %s", sig))
			if !noteOpFailure("SaveRecord", err) {
				t.Errorf("signature %q did not latch; the latch cannot see the "+
					"shape it exists for", sig)
			}
			if got, _ := clusterGone.Load().(string); got != "SaveRecord" {
				t.Errorf("signature %q latched %q, want the op name", sig, got)
			}
		}
	})

	t.Run("an ordinary op failure does not latch", func(t *testing.T) {
		clusterGone = atomic.Value{}
		// The errors the harness injects on purpose, which must stay ordinary
		// failures: latching on these would blame the container for the
		// experiment and mute every later test in the package.
		for _, msg := range []string{
			"not_committed (1020)",
			"commit_unknown_result (1021)",
			"transaction_too_old (1007)",
			"model divergence: 2 violation(s)",
		} {
			if noteOpFailure("SaveRecord", errors.New(msg)) {
				t.Errorf("%q latched; an injected fault is not a dead cluster", msg)
			}
			if v := clusterGone.Load(); v != nil {
				t.Fatalf("%q set the latch to %v", msg, v)
			}
		}
	})

	t.Run("nil never latches", func(t *testing.T) {
		clusterGone = atomic.Value{}
		if noteOpFailure("SaveRecord", nil) {
			t.Error("a nil error latched")
		}
	})

	t.Run("first observer wins", func(t *testing.T) {
		clusterGone = atomic.Value{}
		noteOpFailure("SaveRecord", errors.New("context deadline exceeded"))
		noteOpFailure("DeleteRecord", errors.New("connection refused"))
		if got, _ := clusterGone.Load().(string); got != "SaveRecord" {
			t.Errorf("latch holds %q, want the FIRST observer -- a later op "+
				"overwriting it points the reader at a consequence, not a cause", got)
		}
	})

	t.Run("requireClusterAlive is silent while alive and fatal once gone", func(t *testing.T) {
		clusterGone = atomic.Value{}
		// Alive: must not fail. A latch that fires unconditionally would turn
		// every run red and be indistinguishable from a real death.
		fresh := &recordingTB{TB: t}
		requireClusterAlive(fresh)
		if fresh.failed {
			t.Fatalf("requireClusterAlive failed on a live cluster: %s", fresh.msg)
		}

		clusterGone.Store("SaveRecord")
		gone := &recordingTB{TB: t}
		requireClusterAlive(gone)
		if !gone.failed {
			t.Fatal("requireClusterAlive passed after the latch was set; every " +
				"later test would wait out its own deadline instead")
		}
		if !strings.Contains(gone.msg, "SaveRecord") {
			t.Errorf("message %q does not name the first observer", gone.msg)
		}
	})
}

// recordingTB captures a Fatalf instead of aborting, so both the firing and
// the NOT-firing arm can be asserted from one test.
type recordingTB struct {
	testing.TB
	failed bool
	msg    string
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
	// Deliberately does NOT call runtime.Goexit: the caller is asserting on
	// the outcome, not being aborted by it.
}

func (r *recordingTB) Helper() {}
