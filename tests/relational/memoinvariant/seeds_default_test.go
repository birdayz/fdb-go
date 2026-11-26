//go:build !race

package memoinvariant

// seedCount is the number of rowdiff seeds the generative sweep plans.
//
// Build-tagged low under -race (see seeds_race_test.go): planning runs the full
// Cascades pipeline per query and -race multiplies that cost ~10x, so a large
// sweep would blow the test timeout. This is the non-race budget — generous
// enough that every compensating-rule family the generator can reach shows up,
// and the ReasonNoQuantifier / edge aggregates stabilize.
const seedCount = 160
