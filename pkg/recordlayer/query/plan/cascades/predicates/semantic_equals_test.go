package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSemanticEqualsUnderAliasMap_BooleanSets(t *testing.T) {
	t.Parallel()
	a, b := values.NamedCorrelationIdentifier("a"), values.NamedCorrelationIdentifier("b")
	aliases := mustAliasMap(t, values.AliasPair{Source: a, Target: b})
	mk := func(alias values.CorrelationIdentifier, n int64) QueryPredicate {
		return NewComparisonPredicate(mustQOV(t, alias), Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(n)})
	}
	for _, or := range []bool{false, true} {
		for _, atomic := range []bool{false, true} {
			join := func(children ...QueryPredicate) QueryPredicate {
				if or {
					return WithAtomicity(NewOr(children...), atomic)
				}
				return WithAtomicity(NewAnd(children...), atomic)
			}
			left := join(mk(a, 1), mk(a, 2), mk(a, 3))
			for _, order := range [][]int64{{1, 2, 3}, {3, 2, 1}, {2, 1, 3}, {3, 1, 2}, {2, 3, 1}, {1, 3, 2}, {3, 3, 1, 2}} {
				var children []QueryPredicate
				for _, n := range order {
					children = append(children, mk(b, n))
				}
				right := join(children...)
				if !SemanticEqualsUnderAliasMap(left, right, aliases) || SemanticHashCode(left) != SemanticHashCode(right) {
					t.Errorf("or=%t atomic=%t order=%v: set equality/hash contract failed", or, atomic, order)
				}
				if SemanticEqualsUnderAliasMap(left, right, nil) {
					t.Error("set equality lost external alias identity")
				}
			}
			for _, other := range []QueryPredicate{
				join(mk(b, 1), mk(b, 2)),
				join(mk(b, 1), mk(b, 2), mk(b, 4)),
				WithAtomicity(join(mk(b, 1), mk(b, 2), mk(b, 3)), !atomic),
			} {
				if SemanticEqualsUnderAliasMap(left, other, aliases) {
					t.Error("set equality ignored a missing/different child or atomicity")
				}
			}
		}
	}
}

func TestSemanticEqualsUnderAliasMap_AliasAware(t *testing.T) {
	t.Parallel()
	mk := func(c values.CorrelationIdentifier) QueryPredicate {
		return NewComparisonPredicate(
			mustQOV(t, c),
			Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
		)
	}
	qa := values.NamedCorrelationIdentifier("q_a")
	qb := values.NamedCorrelationIdentifier("q_b")
	a, b := mk(qa), mk(qb)

	aliases := mustAliasMap(t, values.AliasPair{Source: qa, Target: qb})
	if !SemanticEqualsUnderAliasMap(a, b, aliases) {
		t.Fatal("alias-variant predicates must be equal under the mapping")
	}
	if SemanticEqualsUnderAliasMap(a, b, nil) {
		t.Fatal("must NOT be equal under empty alias map (different aliases)")
	}
	// Consistency with the alias-invariant hash.
	if SemanticHashCode(a) != SemanticHashCode(b) {
		t.Fatal("equal-under-aliases predicates must have equal SemanticHashCode")
	}
	// Identity: same alias equal under empty map.
	if !SemanticEqualsUnderAliasMap(mk(qa), mk(qa), nil) {
		t.Fatal("identical predicates must be equal under empty map")
	}
	// Negative: different constant.
	c := NewComparisonPredicate(mustQOV(t, qa),
		Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(2)}})
	if SemanticEqualsUnderAliasMap(a, c, aliases) {
		t.Fatal("different RHS constant must not be equal")
	}
}
