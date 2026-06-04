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
		case *JoinMergeAllValue:
			// A translator SEED (Seed=true) reports NO correlations — exactly as the
			// retired binary JoinMergeResultValue did (it stored its aliases as plain
			// struct fields that this walk never read; see physical_flat_map_wrapper).
			// That is load-bearing: a seed's named aliases are its two immediate source
			// legs, not a correlation set, and reporting them inflates every enclosing
			// select's correlation set, changing correlation-order/exploration (measured:
			// +~32% planner tasks, tipping the ≥4-way STAR past budget). A re-enumeration
			// merge (Seed=false) DOES report its aliases — that is the live set the
			// partition rule's exact branch reads (as JoinMergeAllValue always did).
			if !q.Seed {
				for _, a := range q.Aliases {
					out[a] = struct{}{}
				}
			}
		}
		return true
	})
	return out
}
