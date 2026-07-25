package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// ScanComparisonProvider is the interface plan nodes implement to expose their
// per-column scan comparison ranges — the Go analogue of Java's
// RecordQueryPlanWithComparisons, off which ComparisonsProperty reads a node's
// own comparisons. An index scan and a primary scan both provide them.
type ScanComparisonProvider interface {
	GetScanComparisons() []*predicates.ComparisonRange
}

// IntersectionExpression marks expressions whose children's comparison sets are
// INTERSECTED rather than unioned — the Go analogue of ComparisonsProperty's
// visitRecordQueryIntersectionOnKeyExpressionPlan /
// visitRecordQueryIntersectionOnValuesPlan overrides. A marker rather than a
// type switch over the concrete intersection plans, so a new intersection plan
// type joins the fold by implementing it rather than by being remembered.
//
// The walk consuming both interfaces is the cost model's
// collectSargedComparisons. It lives there and not here because it descends a
// Reference to its PHYSICAL member, a notion belonging to the cascades package,
// which imports this one. There is exactly one such walk, matching Java's
// single comparisons() property.
type IntersectionExpression interface {
	IsIntersection()
}
