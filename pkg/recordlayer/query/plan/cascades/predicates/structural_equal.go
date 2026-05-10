package predicates

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

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
		if ap.Comparison.Type != bp.Comparison.Type {
			return false
		}
		if ap.Comparison.Escape != bp.Comparison.Escape {
			return false
		}
		if !values.ValuesStructurallyEqual(ap.Operand, bp.Operand) {
			return false
		}
		return values.ValuesStructurallyEqual(ap.Comparison.Operand, bp.Comparison.Operand)
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
	case *ExistsPredicate:
		bp, ok := b.(*ExistsPredicate)
		if !ok {
			return false
		}
		return ap.ExistentialAlias == bp.ExistentialAlias
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
