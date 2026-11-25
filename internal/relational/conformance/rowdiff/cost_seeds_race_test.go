//go:build race

package rowdiff

// costSweepDefaultSeeds under the race detector: the full Cascades planner runs
// several times slower with -race, so the un-raced 250-seed budget blows the
// test timeout (that is what turned the race-detector job red). The sweep is a
// single serial loop with no concurrency for -race to inspect, so shrinking the
// budget here loses no race coverage — it only keeps the job inside its
// timeout. The wider net still runs in every non-race build, and
// ROWDIFF_COST_SWEEP_SEEDS=N overrides either way.
const costSweepDefaultSeeds = 50
