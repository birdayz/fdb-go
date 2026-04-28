package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestIntersectionMergeRule_FlattensSingleNested(t *testing.T) {
	t.Parallel()
	keys := []values.Value{values.NewBooleanValue(true)}
	innerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("B"), scanQuant("C")},
		keys,
	)
	innerXQ := expressions.ForEachQuantifier(expressions.InitialOf(innerX))
	outerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("A"), innerXQ},
		keys, // matching keys → flattens
	)
	ref := expressions.InitialOf(outerX)
	rule := NewIntersectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
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
	x := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("A"), scanQuant("B")},
		[]values.Value{values.NewBooleanValue(true)},
	)
	ref := expressions.InitialOf(x)
	rule := NewIntersectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on flat Intersection — yielded %d, want 0", len(yielded))
	}
}

func TestIntersectionMergeRule_DeclinesOnDifferentKeys(t *testing.T) {
	t.Parallel()
	innerKeys := []values.Value{values.NewBooleanValue(true)}
	outerKeys := []values.Value{values.NewBooleanValue(false)} // DIFFERENT!
	innerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("B"), scanQuant("C")},
		innerKeys,
	)
	innerXQ := expressions.ForEachQuantifier(expressions.InitialOf(innerX))
	outerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("A"), innerXQ},
		outerKeys,
	)
	ref := expressions.InitialOf(outerX)
	rule := NewIntersectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired despite different comparison keys — yielded %d, want 0", len(yielded))
	}
}

func TestIntersectionMergeRule_DeclinesOnEmptyInner(t *testing.T) {
	t.Parallel()
	// Outer with one child whose inner is an empty Intersection — rule
	// should NOT flatten (the empty inner has degenerate semantics).
	emptyInner := expressions.NewLogicalIntersectionExpression(nil, []values.Value{values.NewBooleanValue(true)})
	emptyQ := expressions.ForEachQuantifier(expressions.InitialOf(emptyInner))
	outerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("A"), emptyQ},
		[]values.Value{values.NewBooleanValue(true)},
	)
	ref := expressions.InitialOf(outerX)
	rule := NewIntersectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired despite empty inner intersection — yielded %d, want 0", len(yielded))
	}
}

func TestIntersectionMergeRule_PreservesOuterKeys(t *testing.T) {
	t.Parallel()
	keys := []values.Value{values.NewBooleanValue(true), values.NewBooleanValue(false)}
	innerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("B")},
		keys,
	)
	innerXQ := expressions.ForEachQuantifier(expressions.InitialOf(innerX))
	outerX := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{scanQuant("A"), innerXQ},
		keys,
	)
	ref := expressions.InitialOf(outerX)
	yielded := FireExpressionRule(NewIntersectionMergeRule(), ref)
	merged := yielded[0].(*expressions.LogicalIntersectionExpression)
	if got := len(merged.GetComparisonKeyValues()); got != 2 {
		t.Fatalf("merged keys=%d, want 2", got)
	}
}
