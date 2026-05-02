package plandiff

// Divergence marks a corpus entry as a known cross-engine divergence
// rather than a parity assertion. When set, the harness asserts Go's
// behaviour (rows or error) against the embedded expectation but does
// NOT pin Java's actual behaviour — Java may evolve (upstream fix),
// regress, or stay buggy without breaking our test surface.
//
// Direction values categorise the divergence shape so reports can grep
// for upstream-bug counts:
//
//   - DivergenceJavaErrorsGoCorrect: Java throws (NPE / VerifyException
//     / planner-can't-plan / etc.); Go succeeds with SQL-correct rows.
//     Pin Go's rows via GoExpectedRows. The harness asserts Java errored
//     and Go's rows match. If Java upstream fixes the bug and starts
//     succeeding, the assertion fires (`Java unexpectedly succeeded`)
//     prompting an audit.
//
//   - DivergenceJavaWrongRowsGoCorrect: both engines succeed without
//     error; Java returns SQL-incorrect rows (e.g. compound DISTINCT
//     dedup failure). Pin Go's rows via GoExpectedRows. Java's rows
//     are read but not compared — they're documented as wrong.
//
//   - DivergenceBothErrorMessagesDrift: both engines reject the shape;
//     error messages differ in cosmetic ways (e.g. Java quotes the
//     schema name, Go doesn't). Pin Go's error substring via
//     GoErrorContains.
//
//   - DivergenceJavaSucceedsGoRejects: Go is the more restrictive side.
//     Pin Go's error substring via GoErrorContains.
//
// Reason is free-text describing which side is correct and why; goes
// into the test failure message if Go-side regresses.
type Divergence struct {
	Reason          string
	Direction       DivergenceDirection
	GoExpectedRows  [][]any
	GoErrorContains string
}

// DivergenceDirection enumerates the cross-engine divergence shapes.
// Defined as a string type for grep-friendly corpus inspection
// (`grep -c JavaErrorsGoCorrect corpus.go`).
type DivergenceDirection string

const (
	// DivergenceJavaErrorsGoCorrect — Java errors (upstream bug); Go
	// succeeds with SQL-correct rows.
	DivergenceJavaErrorsGoCorrect DivergenceDirection = "JavaErrorsGoCorrect"
	// DivergenceJavaWrongRowsGoCorrect — both engines succeed; Java's
	// rows are SQL-incorrect, Go's are right.
	DivergenceJavaWrongRowsGoCorrect DivergenceDirection = "JavaWrongRowsGoCorrect"
	// DivergenceBothErrorMessagesDrift — both engines reject; error
	// messages differ in cosmetic ways.
	DivergenceBothErrorMessagesDrift DivergenceDirection = "BothErrorMessagesDrift"
	// DivergenceJavaSucceedsGoRejects — Go is the more restrictive side.
	DivergenceJavaSucceedsGoRejects DivergenceDirection = "JavaSucceedsGoRejects"
)
