//go:build race

package sqldriver_test

// duecRaceInstrumented reports whether this binary was built with the race
// detector.
//
// It exists for one reason: the DISTINCT cost probe's wall-clock criteria
// compare two plans that differ only by how much work the dedup operator does,
// and R3's margin over the full operator is 13-17%. Race instrumentation adds a
// per-memory-access cost to BOTH sides, which inflates the shared base and
// COMPRESSES that ratio toward 1.0 — measured at 0.968x under -race against
// 0.82-0.91x without it. The property is unchanged; the regime cannot resolve
// it, exactly as the paged and sub-10k regimes cannot (RFC-210 section 2.1).
//
// So the probe still RUNS under -race and still asserts everything
// non-temporal: plan shapes, row counts, retained-key counts, and the
// auto-commit correctness arms. Only the wall-clock strict-win claims are
// withheld, because a bound that held under instrumentation would have to be so
// loose it no longer excluded the null.
//
// ALLOCATION ratios are NOT withheld. They are counts, not durations: the race
// detector does not change how many bytes the seen-set charges, and the sweep's
// alloc ratios stay at 0.907-0.931 under -race, comfortably inside their bound.
// Dropping them here would give up the one dimension instrumentation leaves
// intact.
const duecRaceInstrumented = true
