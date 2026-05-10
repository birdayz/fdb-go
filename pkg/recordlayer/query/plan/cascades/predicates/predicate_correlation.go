package predicates

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// GetCorrelatedToOfPredicate walks p + its descendants AND the Value
// trees those predicates carry, returning the union of every
// QuantifiedObjectValue's correlation. The result is a fresh map.
//
// Returns nil for nil input. Returns a non-nil empty map for trees
// without any correlation.
//
// Predicates that carry Values (ValuePredicate, ComparisonPredicate)
// have those Values walked too — so a `WHERE q.col = 5` with q being
// a Quantifier alias will surface q in the correlation set, even
// though the predicate-side WalkPredicate alone would only visit the
// ComparisonPredicate node.
//
// Java's equivalent walks `QueryPredicate.getCorrelatedTo()` which
// delegates per-impl. Same bridge story as values.GetCorrelatedToOfValue.
func GetCorrelatedToOfPredicate(p QueryPredicate) map[CorrelationIdentifier]struct{} {
	if p == nil {
		return nil
	}
	out := map[CorrelationIdentifier]struct{}{}
	WalkPredicate(p, func(node QueryPredicate) bool {
		switch np := node.(type) {
		case *ValuePredicate:
			for k := range values.GetCorrelatedToOfValue(np.Value) {
				out[k] = struct{}{}
			}
		case *ComparisonPredicate:
			for k := range values.GetCorrelatedToOfValue(np.Operand) {
				out[k] = struct{}{}
			}
			for k := range values.GetCorrelatedToOfValue(np.Comparison.Operand) {
				out[k] = struct{}{}
			}
		case *Placeholder:
			out[np.ParameterAlias] = struct{}{}
			for k := range values.GetCorrelatedToOfValue(np.Value) {
				out[k] = struct{}{}
			}
		}
		return true
	})
	return out
}

// CorrelationIdentifier is re-exported as a type alias so package
// consumers don't need to import values just for the map key type.
type CorrelationIdentifier = values.CorrelationIdentifier
