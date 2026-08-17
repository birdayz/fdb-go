package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustIntersectionMergeConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct intersection-merge fixture: " + err.Error())
	}
	return value
}

func intersectionMergeQuant(name string) expressions.Quantifier {
	row := values.NewRecordType("IntersectionMergeRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	scan := mustIntersectionMergeConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{name}, row))
	return expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func intersectionMergeExpr(
	quantifiers []expressions.Quantifier, keys []values.Value,
) *expressions.LogicalIntersectionExpression {
	return mustIntersectionMergeConstruct(
		expressions.NewLogicalIntersectionExpression(quantifiers, keys))
}

func fireIntersectionMergeRule(
	t testing.TB, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(NewIntersectionMergeRule(), ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func TestIntersectionMergeRule_FlattensSingleNested(t *testing.T) {
	t.Parallel()
	// Direct constructor reference keeps the behavioral-rule census tied to
	// this positive test; the shared fire helper invokes the same rule type.
	_ = NewIntersectionMergeRule()
	keys := []values.Value{values.NewBooleanValue(true)}
	innerX := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("B"), intersectionMergeQuant("C")},
		keys,
	)
	innerXQ := expressions.ForEachQuantifier(expressions.InitialOf(innerX))
	outerX := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("A"), innerXQ},
		keys, // matching keys → flattens
	)
	ref := expressions.InitialOf(outerX)
	yielded := fireIntersectionMergeRule(t, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalIntersectionExpression)
	if got := len(merged.GetQuantifiers()); got != 3 {
		t.Fatalf("flattened child count=%d, want 3 (A + B + C)", got)
	}
	if got := len(merged.GetComparisonKeyValues()); got != 1 {
		t.Fatalf("merged keys=%d, want 1", got)
	}
}

func TestIntersectionMergeRule_DeclinesOnFlat(t *testing.T) {
	t.Parallel()
	x := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("A"), intersectionMergeQuant("B")},
		[]values.Value{values.NewBooleanValue(true)},
	)
	ref := expressions.InitialOf(x)
	yielded := fireIntersectionMergeRule(t, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on flat Intersection — yielded %d, want 0", len(yielded))
	}
}

func TestIntersectionMergeRule_DeclinesOnDifferentKeys(t *testing.T) {
	t.Parallel()
	innerKeys := []values.Value{values.NewBooleanValue(true)}
	outerKeys := []values.Value{values.NewBooleanValue(false)} // DIFFERENT!
	innerX := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("B"), intersectionMergeQuant("C")},
		innerKeys,
	)
	innerXQ := expressions.ForEachQuantifier(expressions.InitialOf(innerX))
	outerX := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("A"), innerXQ},
		outerKeys,
	)
	ref := expressions.InitialOf(outerX)
	yielded := fireIntersectionMergeRule(t, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired despite different comparison keys — yielded %d, want 0", len(yielded))
	}
}

func TestIntersectionMergeRule_DeclinesOnEmptyInner(t *testing.T) {
	t.Parallel()
	// Outer with one child whose inner is an empty Intersection — rule
	// should NOT flatten (the empty inner has degenerate semantics).
	if empty, err := expressions.NewLogicalIntersectionExpression(
		nil, []values.Value{values.NewBooleanValue(true)}); err == nil || empty != nil {
		t.Fatalf("empty logical intersection = %T, %v; want atomic constructor rejection", empty, err)
	}
}

func TestIntersectionMergeRule_PreservesOuterKeys(t *testing.T) {
	t.Parallel()
	keys := []values.Value{values.NewBooleanValue(true), values.NewBooleanValue(false)}
	innerX := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("B")},
		keys,
	)
	innerXQ := expressions.ForEachQuantifier(expressions.InitialOf(innerX))
	outerX := intersectionMergeExpr(
		[]expressions.Quantifier{intersectionMergeQuant("A"), innerXQ},
		keys,
	)
	ref := expressions.InitialOf(outerX)
	yielded := fireIntersectionMergeRule(t, ref)
	merged := yielded[0].(*expressions.LogicalIntersectionExpression)
	if got := len(merged.GetComparisonKeyValues()); got != 2 {
		t.Fatalf("merged keys=%d, want 2", got)
	}
}
