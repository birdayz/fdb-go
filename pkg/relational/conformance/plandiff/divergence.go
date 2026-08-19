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
//   - DivergenceUnorderedRowOrderDiffers: the ONE direction where NEITHER
//     engine is wrong. Both succeed, both return the SAME MULTISET, and the
//     query has no ORDER BY — so SQL guarantees nothing about sequence and
//     each engine's order is a planner artefact. Pin Go's order via
//     GoExpectedRows.
//
//     It carries a guard the others do not need, and the guard is the whole
//     reason this category is safe to have: Java's rows must be a PERMUTATION
//     of Go's. Without that, "the order differs" would absorb a genuine
//     wrong-rows bug — a dropped row, a duplicated row, a wrong value — under
//     an annotation that reads as benign. With it, the only thing this
//     direction can ever excuse is sequence.
//
//     Use it ONLY for a divergence whose cause is understood and recorded.
//     The one it was added for is a cost-model tie that Go resolves with an
//     identifier-sensitive hash while Java prunes to one member and never
//     reaches the tie (RFC-235 section 17); the same query with the tables
//     renamed plans the opposite nesting, which is what makes it a coin flip
//     rather than a rule about FROM order.
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
	// rows are deterministically SQL-incorrect, Go's are right. The
	// harness fires a stale-annotation guard if Java's rows happen to
	// match Go's expected; for INTERMITTENT Java bugs (e.g. UNION ALL
	// outer ORDER BY where Java sometimes returns the right order),
	// use DivergenceJavaIntermittentGoCorrect instead.
	DivergenceJavaWrongRowsGoCorrect DivergenceDirection = "JavaWrongRowsGoCorrect"
	// DivergenceJavaIntermittentGoCorrect — both engines succeed; Java
	// returns SQL-incorrect rows on SOME runs but may return correct
	// rows on others (planner non-determinism). Go is deterministic
	// and correct. Same shape as JavaWrongRowsGoCorrect minus the
	// stale-annotation guard, since Java's row-for-row match is
	// expected to be intermittent.
	DivergenceJavaIntermittentGoCorrect DivergenceDirection = "JavaIntermittentGoCorrect"
	// DivergenceBothErrorMessagesDrift — both engines reject; error
	// messages differ in cosmetic ways.
	DivergenceBothErrorMessagesDrift DivergenceDirection = "BothErrorMessagesDrift"
	// DivergenceJavaSucceedsGoRejects — Go is the more restrictive side.
	DivergenceJavaSucceedsGoRejects DivergenceDirection = "JavaSucceedsGoRejects"
	// DivergenceUnorderedRowOrderDiffers — both engines succeed and return the
	// SAME MULTISET; only the sequence differs, on a query with no ORDER BY.
	// The harness ASSERTS the permutation relation, so this direction cannot
	// excuse a dropped, duplicated or altered row — only an ordering artefact.
	DivergenceUnorderedRowOrderDiffers DivergenceDirection = "UnorderedRowOrderDiffers"
)
