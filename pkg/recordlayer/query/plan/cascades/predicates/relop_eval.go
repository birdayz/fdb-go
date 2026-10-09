package predicates

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// RelOpComparisonType is the predicate comparison a RelOpValue's comparison
// becomes (RelOpValue.toQueryPredicate).
var RelOpComparisonType = map[values.RelOpComparison]ComparisonType{
	values.RelOpEquals: ComparisonEquals, values.RelOpNotEquals: ComparisonNotEquals,
	values.RelOpLessThan: ComparisonLessThan, values.RelOpLessThanOrEquals: ComparisonLessThanOrEq,
	values.RelOpGreaterThan: ComparisonGreaterThan, values.RelOpGreaterThanOrEquals: ComparisonGreaterThanEq,
	values.RelOpIsDistinctFrom: ComparisonIsDistinctFrom, values.RelOpNotDistinctFrom: ComparisonNotDistinctFrom,
	values.RelOpIsNull: ComparisonIsNull, values.RelOpNotNull: ComparisonIsNotNull,
}

// A RelOpValue evaluates as the comparison predicate it lifts to, so a value
// and a filter over the same comparison cannot disagree.
func init() {
	values.SetRelOpEvaluator(func(comparison values.RelOpComparison, left, right any) (any, error) {
		t, err := Comparison{Type: RelOpComparisonType[comparison]}.EvalAgainst(left, right)
		if err != nil || t == nil {
			return nil, err
		}
		return *t, nil
	})
}
