package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestSemanticEqualsUnderAliasMap_AliasAware(t *testing.T) {
	t.Parallel()
	mk := func(c values.CorrelationIdentifier) QueryPredicate {
		return NewComparisonPredicate(
			values.NewQuantifiedObjectValue(c),
			Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
		)
	}
	qa := values.NamedCorrelationIdentifier("q_a")
	qb := values.NamedCorrelationIdentifier("q_b")
	a, b := mk(qa), mk(qb)

	aliases := values.AliasMap{qa: qb}
	if !SemanticEqualsUnderAliasMap(a, b, aliases) {
		t.Fatal("alias-variant predicates must be equal under the mapping")
	}
	if SemanticEqualsUnderAliasMap(a, b, values.AliasMap{}) {
		t.Fatal("must NOT be equal under empty alias map (different aliases)")
	}
	// Consistency with the alias-invariant hash.
	if SemanticHashCode(a) != SemanticHashCode(b) {
		t.Fatal("equal-under-aliases predicates must have equal SemanticHashCode")
	}
	// Identity: same alias equal under empty map.
	if !SemanticEqualsUnderAliasMap(mk(qa), mk(qa), values.AliasMap{}) {
		t.Fatal("identical predicates must be equal under empty map")
	}
	// Negative: different constant.
	c := NewComparisonPredicate(values.NewQuantifiedObjectValue(qa),
		Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(2)}})
	if SemanticEqualsUnderAliasMap(a, c, aliases) {
		t.Fatal("different RHS constant must not be equal")
	}
}
