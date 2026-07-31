package bench

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Reproducibility gate for the version-pinned conflict differentials.
//
// Those scenarios pin both clients to the setup transaction's commit version so the conflict
// outcome is a deterministic function of the scenario rather than of GRV timing. Under full-suite
// load that pinning is not sufficient: a spurious not_committed(1020) arrives that no client
// caused. Measured, on this cluster shape (single resolver, 4 proxies):
//
//   - 18 go/cgo disagreements across 204 full-package runs, arriving in bursts across unrelated
//     prefixes, and in BOTH directions (libfdb_c spuriously conflicts too).
//   - The Go client's shipped read-conflict ranges for a failing transaction, read out of the
//     commit path, are exactly the intended narrow ranges plus its own \xff/SC/<uid> range.
//   - The setup's writes ARE visible at the pinned version (unique-nonce read-back), so the pin
//     is sound and GetCommittedVersion does not under-report.
//   - libfdb_c's conflicting-keys read-back names the transaction's own USER read range as the
//     one the resolver rejected — not the self-conflict range, which a separate canary
//     (TestDifferential_SelfConflictRangeCanary) shows is never written over.
//   - Bisecting the read version puts the covering write several thousand versions ABOVE the
//     setup, at a commit version shared by conflicts in unrelated prefixes, and no committed Go
//     write-conflict range overlaps it.
//
// A single disagreement is therefore not proof of a semantic divergence. But a semantic
// divergence is DETERMINISTIC — it is a property of what the clients register, not of when they
// run — so it survives a replay at a fresh read version, and the environmental one does not.
// Hence: on disagreement ONLY (agreement is never re-run), replay the exact scenario once more
// on fresh prefixes. Reproduces → fail, quoting both attempts. Does not → count it below.
//
// The non-reproducing class is absorbed into a COUNT, never into silence: it stays measured, and
// the count itself is an assertion.

// conflictEnvCeiling bounds non-reproducing disagreements per package run. Derived from the
// measured rate — 18 across 204 full-package runs (~0.09/run) with the largest observed
// simultaneous burst at 3 — so 4 clears normal noise while a material degradation of the
// cluster's spurious-conflict rate still trips it. Passing it is itself a real signal: it means
// the environment is too noisy for these differentials to certify anything.
const conflictEnvCeiling = 4

// confirmAttemptBudget bounds how many replays one confirmation may spend clearing transient
// errors before it gives up. Giving up counts as REPRODUCED — an inconclusive confirmation must
// never be able to downgrade a real divergence to noise.
const confirmAttemptBudget = 6

var (
	conflictEnvMu    sync.Mutex
	conflictEnvCount int
	conflictEnvSeen  []string

	// conflictVerdicts counts conclusive go-vs-cgo scenario pairs this process has produced.
	conflictVerdicts atomic.Int64
)

// noteConflictVerdict records one conclusive scenario pair. It is what makes the ceiling a RATE
// rather than a raw count: the census is process-wide, so `-test.count=N` accumulates across
// iterations and a fixed ceiling would fail on arithmetic rather than on evidence.
func noteConflictVerdict() { conflictVerdicts.Add(1) }

// conflictEnvAllowance is the ceiling for this process. A single package run produces ~19
// conclusive verdicts and is allowed conflictEnvCeiling; every further 64 verdicts (~3.4 more
// package runs) adds one, against a measured ~0.09 non-reproducing disagreements per run. So a
// long repeat run scales, while a material regression in the spurious-conflict rate — an order of
// magnitude above measured — still trips it at any length.
func conflictEnvAllowance() int {
	return conflictEnvCeiling + int(conflictVerdicts.Load()/64)
}

// confirmConflictDivergence replays a diverging scenario at a fresh read version. `replay` must
// use prefixes unique to its attempt number so each replay gets its own setup commit, and must
// report ok=false when the attempt hit a transient error and carries no verdict.
func confirmConflictDivergence(replay func() (goOut, cOut conflictOutcome, ok bool)) (reproduced bool, detail string) {
	for i := 0; i < confirmAttemptBudget; i++ {
		g, c, ok := replay()
		if !ok {
			continue
		}
		return g.conflicted != c.conflicted, fmt.Sprintf("go=%v cgo=%v", g.conflicted, c.conflicted)
	}
	return true, "confirmation never cleared transient errors — treated as reproduced"
}

// noteEnvironmentalDivergence records a disagreement that did NOT reproduce, and fails once the
// per-package-run ceiling is passed. Note the census is process-wide: under `-test.count=N` it
// accumulates across counts, so repeat-run verification belongs in separate processes
// (bazel --runs_per_test), which is what "per package run" means here.
func noteEnvironmentalDivergence(t *testing.T, name, first, confirm string) {
	t.Helper()
	conflictEnvMu.Lock()
	conflictEnvCount++
	n := conflictEnvCount
	conflictEnvSeen = append(conflictEnvSeen, fmt.Sprintf("%s{first %s, confirm %s}", name, first, confirm))
	seen := strings.Join(conflictEnvSeen, "; ")
	conflictEnvMu.Unlock()

	allowed := conflictEnvAllowance()
	t.Logf("environmental conflict divergence %d/%d at %s: first %s, did not reproduce at a fresh read version (%s)",
		n, allowed, name, first, confirm)
	if n > allowed {
		t.Fatalf("environmental spurious-conflict rate exceeded its ceiling: %d non-reproducing go/cgo "+
			"disagreements against an allowance of %d over %d conclusive verdicts (base %d, derived from a "+
			"measured 18 across 204 full-package runs). The cluster is too noisy for the conflict "+
			"differentials to certify anything — this is a real signal about the environment, not a flaky "+
			"assertion. Seen: %s",
			n, allowed, conflictVerdicts.Load(), conflictEnvCeiling, seen)
	}
}
