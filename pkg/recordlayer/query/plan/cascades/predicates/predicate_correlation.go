package predicates

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// GetCorrelatedToOfPredicate returns transitive correlations as a fresh map.
// Nil input returns nil; an uncorrelated predicate returns a non-nil empty map.
func GetCorrelatedToOfPredicate(p QueryPredicate) map[CorrelationIdentifier]struct{} {
	if p == nil {
		return nil
	}
	// Keep allocation here so inlined read-only callers can use a stack map.
	out := make(map[CorrelationIdentifier]struct{})
	collectPredicateCorrelations(p, out)
	return out
}

// CollectCorrelatedToOfPredicate unions p's correlations into out, allocating
// out when nil. Callers folding several predicates can share one result set.
func CollectCorrelatedToOfPredicate(p QueryPredicate, out map[CorrelationIdentifier]struct{}) map[CorrelationIdentifier]struct{} {
	if out == nil {
		out = make(map[CorrelationIdentifier]struct{})
	}
	collectPredicateCorrelations(p, out)
	return out
}

// Fold the built-in connectives into one set rather than one map per node.
// Other concrete types retain their transitive GetCorrelatedTo contract,
// including types embedding a built-in predicate but overriding that method.
func collectPredicateCorrelations(p QueryPredicate, out map[CorrelationIdentifier]struct{}) {
	switch p := p.(type) {
	case nil, *ConstantPredicate:
	case *AndPredicate:
		for _, child := range p.SubPredicates {
			collectPredicateCorrelations(child, out)
		}
	case *OrPredicate:
		for _, child := range p.SubPredicates {
			collectPredicateCorrelations(child, out)
		}
	case *NotPredicate:
		collectPredicateCorrelations(p.Child, out)
	case *ComparisonPredicate:
		values.CollectCorrelatedToOfValue(p.Operand, out)
		p.Comparison.collectCorrelations(out)
	case *ValuePredicate:
		values.CollectCorrelatedToOfValue(p.Value, out)
	case *PredicateWithValueAndRanges:
		values.CollectCorrelatedToOfValue(p.value, out)
		for _, constraint := range p.ranges {
			for alias := range constraint.GetCorrelatedTo() {
				out[alias] = struct{}{}
			}
		}
	default:
		for alias := range p.GetCorrelatedTo() {
			out[alias] = struct{}{}
		}
	}
}

// CorrelationIdentifier is re-exported as a type alias so package
// consumers don't need to import values just for the map key type.
type CorrelationIdentifier = values.CorrelationIdentifier
