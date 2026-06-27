package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// EvaluateExpressionCount returns the number of RelationalExpression
// nodes in the tree rooted at expr. Matches Java's
// ExpressionCountProperty.evaluate — a bottom-up sum where each node
// contributes 1 if it passes the filter predicate.
//
// The filter parameter lets callers restrict the count to specific
// expression types (matching Java's ofTrackedTypes pattern). Pass nil
// to count all expressions.
func EvaluateExpressionCount(expr expressions.RelationalExpression, filter func(expressions.RelationalExpression) bool) int {
	if expr == nil {
		return 0
	}
	count := 0
	if filter == nil || filter(expr) {
		count = 1
	}
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			count += EvaluateExpressionCount(m, filter)
		}
	}
	return count
}
