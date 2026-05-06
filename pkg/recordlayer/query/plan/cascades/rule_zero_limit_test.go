package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestZeroLimitRule_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(0, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewZeroLimitRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	empty, ok := results[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected empty scan, got %T", results[0])
	}
	if len(empty.GetRecordTypes()) != 0 {
		t.Fatalf("expected nil record types (empty scan), got %v", empty.GetRecordTypes())
	}
}

func TestZeroLimitRule_DoesNotFirePositive(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(5, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewZeroLimitRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when limit > 0, got %d results", len(results))
	}
}

func TestZeroLimitRule_DoesNotFireNegative(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(-1, 10, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewZeroLimitRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when limit < 0 (unlimited), got %d results", len(results))
	}
}
