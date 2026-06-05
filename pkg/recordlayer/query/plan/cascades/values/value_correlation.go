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
		// Source-anchored join RESULT value (RFC-077 F2, exploration-time HIDING).
		// Its leg QOVs are self-bound by the enclosing select's own join
		// quantifiers, so they are NOT external correlations — do NOT descend.
		// This mirrors the retired JoinMergeAllValue Seed=true arm (which reported
		// nothing): reporting the buried leg aliases inflates every enclosing
		// select's correlation order and tips the ≥4-way STAR past the task
		// budget. Partition-time RE-EXPOSURE (the other half of the dual purpose)
		// reads the buried aliases structurally via predicates.AddMergeSeedAliases.
		if rc, ok := node.(*RecordConstructorValue); ok && rc.AnchoredJoin {
			return false
		}
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

// GetCorrelatedToOfAnchoredJoinLegs returns the leg-quantifier correlations of a
// source-anchored join RESULT value (RFC-077 F2, partition-time RE-EXPOSURE).
// GetCorrelatedToOfValue deliberately does NOT descend into an anchored-join RC
// (exploration-time hiding keeps the search space bounded); this is the explicit
// counterpart that DOES, so PartitionSelectRule's predicate classification and
// AddMergeSeedAliases can see the buried leg aliases. It walks each field's value
// tree (FieldValue(QOV(leg), col) — possibly NESTED, when a leg is itself an
// anchored join not yet simplified away) and collects every QuantifiedObjectValue
// correlation, treating the anchored-RC children as ordinary nodes to descend.
//
// Returns nil for a nil or non-anchored input.
func GetCorrelatedToOfAnchoredJoinLegs(rc *RecordConstructorValue) map[CorrelationIdentifier]struct{} {
	if rc == nil || !rc.AnchoredJoin {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	for _, f := range rc.Fields {
		WalkValue(f.Value, func(node Value) bool {
			switch q := node.(type) {
			case *QuantifiedObjectValue:
				out[q.Correlation] = struct{}{}
			case *QuantifiedRecordValue:
				out[q.Alias] = struct{}{}
			case *ObjectValue:
				out[q.Alias] = struct{}{}
			}
			return true
		})
	}
	return out
}
