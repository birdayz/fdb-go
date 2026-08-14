package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestUnionSingletonElimRule_FiresOnSingleton(t *testing.T) {
	t.Parallel()
	scan := smallRewriteScan("T")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	u := mustSmallRewriteConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{q}))
	ref := expressions.InitialOf(u)
	yielded := fireSmallRewriteRule(t, NewUnionSingletonElimRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded type=%T, want *FullUnorderedScanExpression", yielded[0])
	}
}

func TestUnionSingletonElimRule_DeclinesOnTwoChildren(t *testing.T) {
	t.Parallel()
	leaf := smallRewriteScan("T")
	u := mustSmallRewriteConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(leaf)),
		expressions.ForEachQuantifier(expressions.InitialOf(leaf)),
	}))
	ref := expressions.InitialOf(u)
	yielded := fireSmallRewriteRule(t, NewUnionSingletonElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on 2-child union — yielded %d, want 0", len(yielded))
	}
}

func TestUnionSingletonElimRule_DeclinesOnEmpty(t *testing.T) {
	t.Parallel()
	if _, err := expressions.NewLogicalUnionExpression(nil); err == nil {
		t.Fatal("empty UNION was admitted before the singleton rewrite boundary")
	}
}

func TestIntersectionSingletonElimRule_FiresOnSingleton(t *testing.T) {
	t.Parallel()
	scan := smallRewriteScan("T")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	x := mustSmallRewriteConstruct(expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{q},
		[]values.Value{values.NewBooleanValue(true)},
	))
	ref := expressions.InitialOf(x)
	yielded := fireSmallRewriteRule(t, NewIntersectionSingletonElimRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
}

func TestIntersectionSingletonElimRule_DeclinesOnTwoChildren(t *testing.T) {
	t.Parallel()
	leaf := smallRewriteScan("T")
	x := mustSmallRewriteConstruct(expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(leaf)),
			expressions.ForEachQuantifier(expressions.InitialOf(leaf)),
		},
		[]values.Value{values.NewBooleanValue(true)},
	))
	ref := expressions.InitialOf(x)
	yielded := fireSmallRewriteRule(t, NewIntersectionSingletonElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on 2-child intersection — yielded %d, want 0", len(yielded))
	}
}
