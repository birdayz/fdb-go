package predicates

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// GetCorrelatedToOfPredicate is a nil-safe wrapper around
// QueryPredicate.GetCorrelatedTo: the transitive set of correlations p and its
// descendants reference, as a fresh map. Returns nil for nil input, and a
// non-nil empty map for a tree without correlations.
//
// It asks each predicate rather than inspecting it. This used to be a manual
// type switch over the predicate shapes that were known when it was written,
// which silently reported NOTHING for every shape added since —
// PredicateWithValueAndRanges in particular, whose correlations can live
// entirely in its range comparands. Callers used the result to decide which
// quantifiers a compensation still needs, so an unseen correlation became a
// dangling alias in a rebuilt expression. Delegation is complete, including
// for shapes not yet invented, as long as each QueryPredicate implementation
// honors the interface's transitive contract by reporting its own carried
// Values/comparisons and the union of its children.
func GetCorrelatedToOfPredicate(p QueryPredicate) map[CorrelationIdentifier]struct{} {
	if p == nil {
		return nil
	}
	correlations := p.GetCorrelatedTo()
	out := make(map[CorrelationIdentifier]struct{}, len(correlations))
	for k := range correlations {
		out[k] = struct{}{}
	}
	return out
}

// CorrelationIdentifier is re-exported as a type alias so package
// consumers don't need to import values just for the map key type.
type CorrelationIdentifier = values.CorrelationIdentifier
