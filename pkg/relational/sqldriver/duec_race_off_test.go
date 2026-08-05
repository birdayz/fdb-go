//go:build !race

package sqldriver_test

// duecRaceInstrumented reports whether this binary was built with the race
// detector. See duec_race_on.go for why the cost probe's WALL-CLOCK criteria
// consult it.
const duecRaceInstrumented = false
