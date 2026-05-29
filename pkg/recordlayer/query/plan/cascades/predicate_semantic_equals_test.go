package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicateSemanticEquals_AliasAware: comparison predicates over QOVs with
// DIFFERENT aliases are equal under a veq that maps those aliases, and NOT
// equal under the empty veq (alias-blind) — the core 040.1 property. Also
// consistent with the hash: equal-under-veq ⟹ equal PredicateSemanticHashCode.
func TestPredicateSemanticEquals_AliasAware(t *testing.T) {
	t.Parallel()
	mk := func(c values.CorrelationIdentifier) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			values.NewQuantifiedObjectValue(c),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
		)
	}
	qa := values.NamedCorrelationIdentifier("q_a")
	qb := values.NamedCorrelationIdentifier("q_b")
	a := mk(qa)
	b := mk(qb)

	bld := NewAliasMapBuilder()
	bld.Put(qa, qb)
	veq := NewAliasMapValueEquivalence(bld.Build())

	if !PredicateSemanticEquals(a, b, veq) {
		t.Fatal("alias-variant comparison predicates must be equal under the mapping veq")
	}
	if PredicateSemanticEquals(a, b, EmptyValueEquivalence()) {
		t.Fatal("must NOT be equal under empty veq (aliases differ, no mapping)")
	}
	// Consistency with the alias-invariant hash.
	if PredicateSemanticHashCode(a) != PredicateSemanticHashCode(b) {
		t.Fatal("equal-under-veq predicates must have equal PredicateSemanticHashCode")
	}
	// Negative: different op ⇒ not equal even under veq.
	c := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(qa),
		predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: &values.ConstantValue{Value: int64(1)}},
	)
	if PredicateSemanticEquals(a, c, veq) {
		t.Fatal("different comparison ops must not be equal")
	}
}
