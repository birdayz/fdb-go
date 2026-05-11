package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// EvaluateFieldWithComparisonCount walks the expression tree and
// counts the number of comparison predicates that appear in filter
// expressions. Matches Java's FieldWithComparisonCountProperty.evaluate
// — in Java this counts FieldWithComparison nodes inside
// RecordQueryFilterPlan's QueryComponent tree. Go's architecture uses
// ComparisonPredicate instead, so we count those within expressions
// that implement RelationalExpressionWithPredicates.
//
// At references with multiple members, Java takes the minimum across
// members (the most optimistic plan). This evaluator takes the sum
// across all members for simplicity; the minimum-at-ref semantic can
// be added when plan selection depends on it.
func EvaluateFieldWithComparisonCount(expr expressions.RelationalExpression) int {
	if expr == nil {
		return 0
	}
	count := 0
	// Count comparison predicates at this node.
	if wp, ok := expr.(expressions.RelationalExpressionWithPredicates); ok {
		for _, p := range wp.GetPredicates() {
			count += countComparisonPredicates(p)
		}
	}
	// Sum from children.
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			count += EvaluateFieldWithComparisonCount(m)
		}
	}
	return count
}

// countComparisonPredicates counts the number of ComparisonPredicate
// leaf nodes in a predicate tree. This is the Go equivalent of Java's
// getFieldWithComparisonCount walking the QueryComponent tree.
func countComparisonPredicates(p predicates.QueryPredicate) int {
	if p == nil {
		return 0
	}
	if _, ok := p.(*predicates.ComparisonPredicate); ok {
		return 1
	}
	count := 0
	for _, child := range p.Children() {
		count += countComparisonPredicates(child)
	}
	return count
}
