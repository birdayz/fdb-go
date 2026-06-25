package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicateSemanticHashCode_AliasInvariant: predicates identical except
// the quantifier alias their Values reference must hash equal (alias-invariant),
// while genuinely-different predicates must not.
func TestPredicateSemanticHashCode_AliasInvariant(t *testing.T) {
	t.Parallel()
	mkCmp := func(c values.CorrelationIdentifier, k int64) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			values.NewQuantifiedObjectValue(c),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: k}},
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
	ea := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier("e_a"))
	eb := predicates.NewExistentialAlias(values.NamedCorrelationIdentifier("e_b"))
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
