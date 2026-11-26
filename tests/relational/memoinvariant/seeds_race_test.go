//go:build race

package memoinvariant

// seedCount under -race: a fraction of the non-race budget (seeds_default_test.go).
//
// The race detector multiplies planning cost by roughly an order of magnitude,
// so the sweep is kept small enough to finish well inside the test timeout. The
// aggregate assertions all use <= against a baseline measured at the non-race
// budget, so a smaller sweep can only under-count — it never spuriously fails.
// Family coverage is still guaranteed because the targeted probes (which do not
// scale with seedCount) exercise every required family unconditionally.
const seedCount = 24
