package predicates

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// StructurallyEqual reports whether two predicates are structurally
// equal: same concrete type, same non-child attributes, and
// recursively equal children. Mirrors Java's
// QueryPredicate.semanticEquals for the same-scope case.
func StructurallyEqual(a, b QueryPredicate) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch ap := a.(type) {
	case *ComparisonPredicate:
		bp, ok := b.(*ComparisonPredicate)
		if !ok {
			return false
		}
		if !values.ValuesStructurallyEqual(ap.Operand, bp.Operand) {
			return false
		}
		// Every identity-bearing field of the Comparison, not just Type/Escape/
		// Operand: two TEXT_CONTAINS_ALL predicates differing only in tokenizer
		// read different index data and must not compare equal.
		return comparisonIdentityEqual(ap.Comparison, bp.Comparison)
	case *ValuePredicate:
		bp, ok := b.(*ValuePredicate)
		if !ok {
			return false
		}
		return values.ValuesStructurallyEqual(ap.Value, bp.Value)
	case *ConstantPredicate:
		bp, ok := b.(*ConstantPredicate)
		if !ok {
			return false
		}
		return ap.Value == bp.Value
	case *ExistentialValuePredicate:
		bp, ok := b.(*ExistentialValuePredicate)
		if !ok {
			return false
		}
		if !comparisonIdentityEqual(ap.Comparison, bp.Comparison) {
			return false
		}
		return values.ValuesStructurallyEqual(ap.Value, bp.Value)
	case *Placeholder:
		bp, ok := b.(*Placeholder)
		if !ok {
			return false
		}
		if ap.ParameterAlias != bp.ParameterAlias {
			return false
		}
		return values.ValuesStructurallyEqual(ap.Value, bp.Value)
	case *AndPredicate:
		bp, ok := b.(*AndPredicate)
		if !ok || len(ap.SubPredicates) != len(bp.SubPredicates) {
			return false
		}
		for i := range ap.SubPredicates {
			if !StructurallyEqual(ap.SubPredicates[i], bp.SubPredicates[i]) {
				return false
			}
		}
		return true
	case *OrPredicate:
		bp, ok := b.(*OrPredicate)
		if !ok || len(ap.SubPredicates) != len(bp.SubPredicates) {
			return false
		}
		for i := range ap.SubPredicates {
			if !StructurallyEqual(ap.SubPredicates[i], bp.SubPredicates[i]) {
				return false
			}
		}
		return true
	case *NotPredicate:
		bp, ok := b.(*NotPredicate)
		if !ok {
			return false
		}
		return StructurallyEqual(ap.Child, bp.Child)
	default:
		return a.Explain() == b.Explain()
	}
}
