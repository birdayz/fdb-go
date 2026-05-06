package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestNoOpLimitElimRule_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(-1, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewNoOpLimitElimRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	if _, ok := results[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected scan (inner) to be yielded, got %T", results[0])
	}
}

func TestNoOpLimitElimRule_DoesNotFireWithLimit(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(10, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewNoOpLimitElimRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when limit is positive, got %d results", len(results))
	}
}

func TestNoOpLimitElimRule_DoesNotFireWithOffset(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(-1, 5, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewNoOpLimitElimRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when offset>0, got %d results", len(results))
	}
}
