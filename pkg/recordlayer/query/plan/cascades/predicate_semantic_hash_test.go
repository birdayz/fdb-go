package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicateSemanticHashCode_AliasInvariant: predicates identical except
// the quantifier alias their Values reference must hash equal (alias-invariant),
// while genuinely-different predicates must not.
func TestPredicateSemanticHashCode_AliasInvariant(t *testing.T) {
	t.Parallel()
	mkCmp := func(c values.CorrelationIdentifier, k int64) predicates.QueryPredicate {
		root, err := values.NewQuantifiedObjectValue(c, values.NotNullLong)
		if err != nil {
			t.Fatalf("construct semantic-hash QOV: %v", err)
		}
		return predicates.NewComparisonPredicate(
			root,
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: k, Typ: values.NotNullLong}},
		)
	}
	a := mkCmp(values.NamedCorrelationIdentifier("q_a"), 1)
	b := mkCmp(values.NamedCorrelationIdentifier("q_b"), 1)
	if predicates.SemanticHashCode(a) != predicates.SemanticHashCode(b) {
		t.Fatal("comparison predicates differing only by quantifier alias must hash equal")
	}
	// Different constant ⇒ different hash.
	if predicates.SemanticHashCode(a) == predicates.SemanticHashCode(mkCmp(values.NamedCorrelationIdentifier("q_a"), 2)) {
		t.Fatal("different RHS constant must hash differently")
	}
	// ExistentialValuePredicate: alias excluded.
	ea := mustExistentialAlias(t, values.NamedCorrelationIdentifier("e_a"))
	eb := mustExistentialAlias(t, values.NamedCorrelationIdentifier("e_b"))
	if predicates.SemanticHashCode(ea) != predicates.SemanticHashCode(eb) {
		t.Fatal("ExistentialValuePredicate must hash alias-invariantly")
	}
	// And of two alias-variant comparisons hashes equal.
	andA := predicates.NewAnd(mkCmp(values.NamedCorrelationIdentifier("q_a"), 1), ea)
	andB := predicates.NewAnd(mkCmp(values.NamedCorrelationIdentifier("q_b"), 1), eb)
	if predicates.SemanticHashCode(andA) != predicates.SemanticHashCode(andB) {
		t.Fatal("And of alias-variant predicates must hash equal")
	}
}
