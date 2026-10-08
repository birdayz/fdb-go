package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRejectsNull_ShapesThroughThePortedSet pins rejectsNull over the
// null-rejecting and null-accepting shapes (WS-E design 5.4(j)), now that
// foldPredicateAtNull runs Java's ConstantFoldingRuleSet (ConstantFoldingRules)
// over the null-substituted predicate, as Java's
// ConstantPredicateFoldingUtil.foldPredicateAtNull does. A shape rejects the
// injected null row when it folds to FALSE or NULL; anything that does not fold
// to a constant (a COALESCE, which is not null-strict, or a conjunct over
// another alias) accepts it.
func TestRejectsNull_ShapesThroughThePortedSet(t *testing.T) {
	t.Parallel()
	q := expressions.ForEachNullOnEmptyQuantifier(expressions.InitialOf(nullOnEmptyScan("T")))
	other := expressions.ForEachQuantifier(expressions.InitialOf(nullOnEmptyScan("U")))
	k := fieldOverAlias(q, "k")
	name := fieldOverAlias(q, "name")
	otherK := fieldOverAlias(other, "k")
	five := &values.ConstantValue{Value: int64(5), Typ: values.NotNullLong}
	cmp := func(v values.Value, typ predicates.ComparisonType, operand values.Value) predicates.QueryPredicate {
		return &predicates.ComparisonPredicate{Operand: v, Comparison: predicates.Comparison{Type: typ, Operand: operand}}
	}
	plusOne, err := values.NewArithmeticValue(values.OpAdd, k, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong})
	if err != nil {
		t.Fatal(err)
	}
	coalesce := values.NewScalarFunctionValue("COALESCE", values.NotNullLong, k, five)
	for _, c := range []struct {
		name    string
		p       predicates.QueryPredicate
		rejects bool
	}{
		{"k = 5", cmp(k, predicates.ComparisonEquals, five), true},
		{"k > 5", cmp(k, predicates.ComparisonGreaterThan, five), true},
		{"k IS NOT NULL", cmp(k, predicates.ComparisonIsNotNull, nil), true},
		{"k + 1 = 5 (null-strict collapse)", cmp(plusOne, predicates.ComparisonEquals, five), true},
		{"k = 5 AND name IS NULL", predicates.NewAnd(cmp(k, predicates.ComparisonEquals, five), cmp(name, predicates.ComparisonIsNull, nil)), true},
		// Both disjuncts fold to UNKNOWN and absorption keeps one (Java's
		// applyAbsorptionLaw over two equal minors), so the OR is UNKNOWN.
		{"k = 5 OR name = 5", predicates.NewOr(cmp(k, predicates.ComparisonEquals, five), cmp(name, predicates.ComparisonEquals, five)), true},
		// Children first, as Java's simplifyWithReExploration: `NULL = 5` folds to
		// UNKNOWN before the NOT is visited, so NotOverComparisonRule never sees a
		// comparison, and the set has no NOT over a constant. Not proven, in
		// either engine.
		{"NOT (k = 5)", predicates.NewNot(cmp(k, predicates.ComparisonEquals, five)), false},
		{"k IS NULL", cmp(k, predicates.ComparisonIsNull, nil), false},
		{"name IS NULL", cmp(name, predicates.ComparisonIsNull, nil), false},
		{"COALESCE(k, 5) = 5", cmp(coalesce, predicates.ComparisonEquals, five), false},
		{"k = 5 OR k IS NULL", predicates.NewOr(cmp(k, predicates.ComparisonEquals, five), cmp(k, predicates.ComparisonIsNull, nil)), false},
		{"k = 5 OR other.k = 5", predicates.NewOr(cmp(k, predicates.ComparisonEquals, five), cmp(otherK, predicates.ComparisonEquals, five)), false},
		{"other.k = 5", cmp(otherK, predicates.ComparisonEquals, five), false},
	} {
		got, err := rejectsNull(c.p, q.GetAlias())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.rejects {
			t.Errorf("%s: rejectsNull = %v, want %v", c.name, got, c.rejects)
		}
	}
}
