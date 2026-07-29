package values

// A lateral UNNEST's Explode reads its array out of the row of the source that
// owns it. When that source is one leg of a merged (multi-source) row, the
// dependency on the OWNING leg is what keeps a bipartition from separating the
// Explode from its array — separate them and the Explode materializes against a
// row where the array column is unbound, which yields zero rows.
//
// That dependency used to be recovered from a STRING: the collection was minted
// as `FieldValue{Field:"A.ARR", Child:QOV(B)}` (B the flow leg, A the buried
// one), GetCorrelatedToOfValue reported only {B}, and a helper here sliced "A"
// off the field name and re-added it. The slice is a name-keyed correlation —
// the identity of a quantifier decided by text — and it is wrong in both
// directions: a leg addressed by a minted binding rather than its SQL alias
// loses the edge entirely, and an unrelated sibling that happens to share the
// alias gains one.
//
// The recovery is gone because the shape is: every Explode collection the
// translator emits is now an ordinal bake over its OWNER's own quantifier
// (unnestBakedRootCollection, unnest_gather, chained_unnest), so
// GetCorrelatedToOfValue reports the owner directly — Java's
// Quantifier.getCorrelatedTo edge, with no recovery step to get wrong.
//
// TestExplodeCollectionsAreOrdinalBaked (pkg/relational/core/query) pins the
// producer side across every routing arm, including that the root ordinal is
// the owner's WINDOW OFFSET in a merged row rather than a first-match by name.
// TestGatheredExplodeOwnerEdgeReachesPartitionOrder and
// …ReachesMatchEnumerator pin the two consumers, and each has a name-model arm
// asserting the owner dependency is ABSENT for a dotted collection — restoring
// the slice here turns those arms red, which is what keeps this deletion from
// being reversible in silence.

// GetCorrelatedToOfValue walks v + its descendants and returns the
// union of every correlation-bearing leaf Value's alias. Handles
// QuantifiedObjectValue, QuantifiedRecordValue, ScalarSubqueryValue,
// ObjectValue, UnmatchedAggregateValue, and ConstantObjectValue.
// ExistsValue is a transparent composite — its child QuantifiedObjectValue
// is reached via the Children() descent.
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
		// ExistsValue is a transparent composite (RFC-141): its child
		// QuantifiedObjectValue carries the correlation and is reached by
		// WalkValue's Children() descent, so no dedicated case is needed.
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

// GetCorrelatedToWithoutChildrenOfValue returns only the correlations carried
// by v itself, excluding correlations contributed by descendant Values.
//
// This is the Go counterpart of Java Value.getCorrelatedToWithoutChildren().
// Keep the switch in lockstep with the correlation-bearing leaf cases in
// GetCorrelatedToOfValue. Composite wrappers such as FieldValue and
// ExistsValue deliberately contribute nothing here even though their full
// subtree correlation set is non-empty.
func GetCorrelatedToWithoutChildrenOfValue(
	v Value,
) map[CorrelationIdentifier]struct{} {
	out := map[CorrelationIdentifier]struct{}{}
	switch correlated := v.(type) {
	case *QuantifiedObjectValue:
		out[correlated.Correlation] = struct{}{}
	case *QuantifiedRecordValue:
		out[correlated.Alias] = struct{}{}
	case *ScalarSubqueryValue:
		out[correlated.Alias] = struct{}{}
	case *ObjectValue:
		out[correlated.Alias] = struct{}{}
	case *UnmatchedAggregateValue:
		out[correlated.UnmatchedID] = struct{}{}
	case *ConstantObjectValue:
		out[correlated.Alias] = struct{}{}
	}
	return out
}
