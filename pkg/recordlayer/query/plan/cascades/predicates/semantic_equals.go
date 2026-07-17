package predicates

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SemanticEqualsUnderAliasMap is the ALIAS-AWARE, bool counterpart of
// PredicateEquals: predicates differing only in the quantifier alias their
// Values reference are equal when those aliases correspond in `aliases`.
// Operand/child Values compare via values.SemanticEqualsUnderAliasMap.
//
// Lives in the predicates package (RFC-040 040.1b) so expressions' relational
// EqualsWithoutChildren (040.2) can call it without an import cycle. Consistent
// with predicates.SemanticHashCode (equal-under-aliases ⟹ equal hash).
func SemanticEqualsUnderAliasMap(a, b QueryPredicate, aliases values.AliasMap) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch ap := a.(type) {
	case *ConstantPredicate:
		bp, ok := b.(*ConstantPredicate)
		return ok && ap.Value == bp.Value
	case *AndPredicate:
		bp, ok := b.(*AndPredicate)
		return ok && predicateListsSemanticEqual(ap.SubPredicates, bp.SubPredicates, aliases)
	case *OrPredicate:
		bp, ok := b.(*OrPredicate)
		return ok && predicateListsSemanticEqual(ap.SubPredicates, bp.SubPredicates, aliases)
	case *NotPredicate:
		bp, ok := b.(*NotPredicate)
		return ok && SemanticEqualsUnderAliasMap(ap.Child, bp.Child, aliases)
	case *ValuePredicate:
		bp, ok := b.(*ValuePredicate)
		return ok && values.SemanticEqualsUnderAliasMap(ap.Value, bp.Value, aliases)
	case *ComparisonPredicate:
		bp, ok := b.(*ComparisonPredicate)
		if !ok {
			return false
		}
		if ap.Comparison.Type != bp.Comparison.Type ||
			ap.Comparison.Escape != bp.Comparison.Escape ||
			ap.Comparison.ParameterName != bp.Comparison.ParameterName ||
			ap.Comparison.TextTokenizerName != bp.Comparison.TextTokenizerName ||
			ap.Comparison.TextAnalyzerName != bp.Comparison.TextAnalyzerName ||
			ap.Comparison.TextMaxDistance != bp.Comparison.TextMaxDistance ||
			ap.Comparison.TextStrictPrefix != bp.Comparison.TextStrictPrefix {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(ap.Operand, bp.Operand, aliases) {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(ap.Comparison.QueryVector, bp.Comparison.QueryVector, aliases) {
			return false
		}
		if !distanceRankKnobsEqual(&ap.Comparison, &bp.Comparison) {
			return false
		}
		if ap.Comparison.Type.IsUnary() {
			return true
		}
		return values.SemanticEqualsUnderAliasMap(ap.Comparison.Operand, bp.Comparison.Operand, aliases)
	case *ExistentialValuePredicate:
		bp, ok := b.(*ExistentialValuePredicate)
		if !ok {
			return false
		}
		if ap.Comparison.Type != bp.Comparison.Type {
			return false
		}
		return values.SemanticEqualsUnderAliasMap(ap.Value, bp.Value, aliases)
	}
	return false
}

func predicateListsSemanticEqual(a, b []QueryPredicate, aliases values.AliasMap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !SemanticEqualsUnderAliasMap(a[i], b[i], aliases) {
			return false
		}
	}
	return true
}

// distanceRankKnobsEqual compares the optional HNSW knobs (EfSearch /
// IsReturningVectors) by value-or-both-nil — nil ("index default") is a
// distinct identity from an explicit setting. The QueryVector is compared
// separately because this layer is alias-map-relative.
func distanceRankKnobsEqual(a, b *Comparison) bool {
	if (a.EfSearch == nil) != (b.EfSearch == nil) {
		return false
	}
	if a.EfSearch != nil && *a.EfSearch != *b.EfSearch {
		return false
	}
	if (a.IsReturningVectors == nil) != (b.IsReturningVectors == nil) {
		return false
	}
	return a.IsReturningVectors == nil || *a.IsReturningVectors == *b.IsReturningVectors
}
