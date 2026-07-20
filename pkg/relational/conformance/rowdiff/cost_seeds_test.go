//go:build !race

package rowdiff

// costSweepDefaultSeeds is the committed budget for TestCostInvariant_
// PurePlannerSweep: every seed drives a full Cascades planning pass per
// query×projection (thousands of plans total). Under the race detector the
// planner runs several times slower, so a smaller budget applies there (see
// cost_seeds_race_test.go) to stay within the test timeout — the sweep is a
// single serial loop, so -race adds no coverage, only cost.
const costSweepDefaultSeeds = 250
