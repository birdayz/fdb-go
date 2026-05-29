package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// PredicateSemanticEquals is the ALIAS-AWARE counterpart of
// predicates.PredicateEquals: it compares two QueryPredicates threading a
// ValueEquivalence through their operand/child Values via ValueSemanticEquals,
// so predicates that differ only in the quantifier alias their Values
// reference are recognized as equal when those aliases correspond under veq.
//
// Mirrors Java QueryPredicate.semanticEquals(other, ValueEquivalence.fromAliasMap).
// Lives in the cascades package (predicates cannot import cascades's
// ValueEquivalence/ValueSemanticEquals). RFC-040 040.1. Inert until relational
// EqualsWithoutChildren is switched to it (040.2).
//
// Consistency with PredicateSemanticHashCode: when this returns true under
// some veq, PredicateSemanticHashCode must agree (both are alias-invariant on
// the value references) — gated by the consistency fuzz.
func PredicateSemanticEquals(a, b predicates.QueryPredicate, veq ValueEquivalence) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch ap := a.(type) {
	case *predicates.ConstantPredicate:
		bp, ok := b.(*predicates.ConstantPredicate)
		return ok && ap.Value == bp.Value
	case *predicates.AndPredicate:
		bp, ok := b.(*predicates.AndPredicate)
		return ok && predicateListsSemanticEqual(ap.SubPredicates, bp.SubPredicates, veq)
	case *predicates.OrPredicate:
		bp, ok := b.(*predicates.OrPredicate)
		return ok && predicateListsSemanticEqual(ap.SubPredicates, bp.SubPredicates, veq)
	case *predicates.NotPredicate:
		bp, ok := b.(*predicates.NotPredicate)
		return ok && PredicateSemanticEquals(ap.Child, bp.Child, veq)
	case *predicates.ValuePredicate:
		bp, ok := b.(*predicates.ValuePredicate)
		return ok && ValueSemanticEquals(ap.Value, bp.Value, veq).IsTrue()
	case *predicates.ComparisonPredicate:
		bp, ok := b.(*predicates.ComparisonPredicate)
		if !ok {
			return false
		}
		if ap.Comparison.Type != bp.Comparison.Type || ap.Comparison.Escape != bp.Comparison.Escape {
			return false
		}
		if ap.Comparison.ParameterName != bp.Comparison.ParameterName {
			return false
		}
		if !ValueSemanticEquals(ap.Operand, bp.Operand, veq).IsTrue() {
			return false
		}
		if ap.Comparison.Type.IsUnary() {
			return true
		}
		return ValueSemanticEquals(ap.Comparison.Operand, bp.Comparison.Operand, veq).IsTrue()
	case *predicates.ExistsPredicate:
		bp, ok := b.(*predicates.ExistsPredicate)
		if !ok {
			return false
		}
		// Existential aliases must be identical OR correspond under veq.
		return ap.ExistentialAlias == bp.ExistentialAlias ||
			veq.IsDefinedEqualAlias(ap.ExistentialAlias, bp.ExistentialAlias).IsTrue()
	}
	return false
}

func predicateListsSemanticEqual(a, b []predicates.QueryPredicate, veq ValueEquivalence) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !PredicateSemanticEquals(a[i], b[i], veq) {
			return false
		}
	}
	return true
}
