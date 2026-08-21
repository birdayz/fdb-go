package chaos

import (
	"strings"
	"sync/atomic"
	"testing"
)

// ONE DEAD CLUSTER MUST NOT LOOK LIKE N BROKEN TESTS.
//
// The whole package shares a single FDB container (TestMain). The container's
// bring-up is retried -- pkg/testcontainers/foundationdb covers exactly that --
// but nothing watches it AFTERWARDS, and a mid-run death is the case that
// actually costs: every remaining scenario fails at its FIRST op with a
// context deadline, each paying the full per-op timeout, and the run ends on a
// package-level panic whose stack points at whichever test happened to be last.
//
// Observed in CI: one container died at op 17 of a fault-injection scenario and
// produced 14 failures plus a 15-minute package timeout, none of which named
// the container. The tests were not concurrent -- BUILD.bazel pins
// -test.parallel=1 -- so they died one after another, in a queue, each waiting
// out its own deadline against a server that was already gone.
//
// This latch converts that into one attributed failure and N fast ones. It is
// deliberately NOT a retry: a dead container mid-suite is a real event that
// must stay red. The goal is only that the FIRST report names the cause and
// the rest stop paying for it.
var clusterGone atomic.Value // string: the op that first observed the death

// clusterDeathSignatures are the shapes an op takes on when the server is no
// longer there, as opposed to a fault the harness injected on purpose.
//
// Injected faults never look like these: the fault set mints 1020/1021/1007 and
// read errors, and FaultCommitUnknown injects no error object at all -- it
// re-runs the closure. So a match here is the cluster, not the experiment.
//
// NOT covered, deliberately: a slow-but-alive cluster can produce a deadline
// under load and would latch here too. That is why the latch only changes the
// MESSAGE and the waiting, never the verdict -- a false latch still fails the
// run, it just blames the wrong thing, and the alternative (waiting out every
// deadline) blames nothing at all.
var clusterDeathSignatures = []string{
	"context deadline exceeded",
	"connection to server failed",
	"broken_promise",
	"connection refused",
}

// noteOpFailure records an op error and reports whether it looks like the
// shared cluster died rather than the op failing on its own terms.
func noteOpFailure(op string, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sig := range clusterDeathSignatures {
		if strings.Contains(msg, sig) {
			clusterGone.CompareAndSwap(nil, op)
			return true
		}
	}
	return false
}

// requireClusterAlive fails a scenario immediately if an earlier one already
// found the cluster gone, instead of letting it wait out its own deadline.
func requireClusterAlive(t testing.TB) {
	t.Helper()
	if first, ok := clusterGone.Load().(string); ok {
		t.Fatalf("chaos: the shared FDB container is gone -- first seen at %s. "+
			"This test never ran against a live cluster; do not read it as a "+
			"failure of what it tests. Root-cause the container death.", first)
	}
}
