package hunt

// Shrink minimizes a failing seed to the smallest reproducer: the fewest operations that still
// trip the oracle, verified at every op so the failure pins to the exact offending operation.
// It returns the shrunk Config and its Report. If the seed does not reproduce under per-op
// verification (e.g. it was a flaky artifact — which cannot happen here since Run is fully
// deterministic, but the guard keeps Shrink honest), the original Config and Report come back.
//
// A minimized (seed, NumOps) pair is the whole bug report: hand it to Run and the failure
// reproduces instantly, no cluster, no Docker.
func Shrink(seed uint64, cfg Config) (Config, *Report) {
	cfg = cfg.withDefaults()

	// Verify after every op so a shrunk run fails at the earliest possible op, not at the next
	// VerifyEvery boundary.
	probe := cfg
	probe.VerifyEvery = 1

	full := Run(seed, probe)
	if !full.Failed() {
		return cfg, full
	}

	// Find the fewest ops that still fail. Monotonicity ("fails at N ⇒ fails at N+1") is not
	// guaranteed — a later deleteAll can repair the divergence — so the honest minimum is the
	// first failing prefix, found by scanning up from 1.
	n := minFailingPrefix(full.Ops, func(k int) bool {
		p := probe
		p.NumOps = k
		return Run(seed, p).Failed()
	})

	probe.NumOps = n
	return probe, Run(seed, probe)
}

// FaultDependent reports whether a failing seed still fails with faults disabled. A false means
// the bug needs the commit-fault schedule (a retry/idempotency defect); a true means it
// reproduces on the happy path alone (a plain logic bug, and a more serious one). Only
// meaningful for a seed that already failed under cfg.
func FaultDependent(seed uint64, cfg Config) bool {
	cfg = cfg.withDefaults()
	noFaults := cfg
	// A negative probability disables Buggify in Run (which treats FaultProb<=0 as "faults
	// off") and is left untouched by withDefaults (only exactly 0 is re-defaulted to 0.25).
	noFaults.FaultProb = -1
	return !Run(seed, noFaults).Failed()
}

// minFailingPrefix returns the smallest n in [1,max] for which fails(n) is true, scanning up
// from 1. Returns max if none of the shorter prefixes fail (the caller has already confirmed
// fails(max)). Factored out so the search is unit-testable without a real failing seed.
func minFailingPrefix(max int, fails func(n int) bool) int {
	for n := 1; n < max; n++ {
		if fails(n) {
			return n
		}
	}
	return max
}
