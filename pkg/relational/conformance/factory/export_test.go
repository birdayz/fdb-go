package factory

import (
	"context"
	"database/sql"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// RowsDiffForTest exposes the second-plan oracle's row comparator, including
// the ordered/unordered switch that decides whether it can see an ordering
// divergence at all.
//
// The comparator is the oracle's entire eye. Disabling MatchLeafRule removes
// the ORDERINGS an index provides, so a sorted query is precisely where the
// perturbation bites — and a comparator that sorts both sides before comparing
// them is blind exactly there, while looking identical in every log.
func RowsDiffForTest(ordered bool, a, b [][]any) string { return rowsDiff(ordered, a, b) }

// CrossEngineDiffForTest exposes the cross-engine comparator, whose scalar
// rendering has to distinguish two int64s that a float64 cannot.
func CrossEngineDiffForTest(goRows, javaRows [][]any) string {
	return crossEngineDiff(goRows, javaRows)
}

// ExplainOnForTest exposes the plan-text reader the second-plan oracle's
// precondition compares. The precondition is a string equality, so what that
// string erases and what it keeps IS the precondition.
func ExplainOnForTest(ctx context.Context, conn *sql.Conn, sqlText string) (string, error) {
	return explainOn(ctx, conn, sqlText)
}

// TLPEligibleForTest exposes the partition oracle's eligibility predicate.
// Eligibility is a claim about whether the property HOLDS for a query spec,
// and a spec the current generator never draws is exactly the one nothing else
// can hand the predicate.
func TLPEligibleForTest(q rowdiff.Query) bool { return tlpEligible(q) }

// CheckPartitionForTest exposes the TLP partition property to the package's
// external tests.
//
// The property is not exported on the production surface because nothing
// outside this package should be able to declare a partition sound; it is
// exposed to tests because a checker with no detector is a checker that
// blesses everything, and the detector has to be able to hand it partitions a
// real run cannot produce on demand.
func CheckPartitionForTest(unfiltered, pos, neg, unknown [][]any) string {
	return checkPartition(unfiltered, pos, neg, unknown)
}
