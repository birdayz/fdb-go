package values

// GetCorrelatedToOfValue walks v + its descendants and returns the
// union of every correlation-bearing leaf Value's alias. Handles
// QuantifiedObjectValue, QuantifiedRecordValue, ExistsValue,
// ScalarSubqueryValue, ObjectValue, UnmatchedAggregateValue, and
// ConstantObjectValue.
//
// Returns nil for nil input. Returns a non-nil empty map for trees
// with no correlations.
//
// Ports Java's Value.getCorrelatedTo().
func GetCorrelatedToOfValue(v Value) map[CorrelationIdentifier]struct{} {
	if v == nil {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	WalkValue(v, func(node Value) bool {
		switch q := node.(type) {
		case *QuantifiedObjectValue:
			out[q.Correlation] = struct{}{}
		case *QuantifiedRecordValue:
			out[q.Alias] = struct{}{}
		case *ExistsValue:
			out[q.Alias] = struct{}{}
		case *ScalarSubqueryValue:
			out[q.Alias] = struct{}{}
		case *ObjectValue:
			out[q.Alias] = struct{}{}
		case *UnmatchedAggregateValue:
			out[q.UnmatchedID] = struct{}{}
		case *ConstantObjectValue:
			out[q.Alias] = struct{}{}
		}
		return true
	})
	return out
}
