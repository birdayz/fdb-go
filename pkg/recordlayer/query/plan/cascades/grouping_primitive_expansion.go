package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// expandGroupingKeysToPrimitives flattens each grouping key into its
// primitive leaf accessors — the Go form of Java's grouping-path use of
// Values.primitiveAccessorsForType (Values.java:99-121). Java carries ONE
// grouping value whose record type wraps the keys and expands its result
// type; Go's GroupByExpression carries the keys as a slice, so the
// expansion applies per key: a primitive key passes through unchanged, a
// RECORD-typed key becomes its leaves in declared field order, recursively.
// The three Java call sites this serves are
// ImplementStreamingAggregationRule.java:111 (the required pre-aggregate
// ordering), GroupByExpression.java:434 (grouping subsumption during
// aggregate-index matching) and
// PushRequestedOrderingThroughGroupByRule.java:141 (the ordering constraint
// pushed below the GROUP BY).
//
// The error is Java's ORDERING_IS_OF_INCOMPATIBLE_TYPE arm: a key with no
// leaf decomposition (ARRAY, RELATION).
func expandGroupingKeysToPrimitives(groupingKeys []values.Value) ([]values.Value, error) {
	out := make([]values.Value, 0, len(groupingKeys))
	for _, gk := range groupingKeys {
		gk := gk
		leaves, err := values.PrimitiveAccessorsForType(gk.Type(), func() values.Value { return gk })
		if err != nil {
			return nil, err
		}
		out = append(out, leaves...)
	}
	return out, nil
}
