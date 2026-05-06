package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestLimitMergeRule_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inner := expressions.NewLogicalLimitExpression(100, 0, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	outer := expressions.NewLogicalLimitExpression(10, 0, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged, ok := results[0].(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("expected LogicalLimitExpression, got %T", results[0])
	}
	if merged.GetLimit() != 10 {
		t.Fatalf("limit = %d, want 10", merged.GetLimit())
	}
	if merged.GetOffset() != 0 {
		t.Fatalf("offset = %d, want 0", merged.GetOffset())
	}
}

func TestLimitMergeRule_WithOffsets(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Inner: skip 10, take 50 → rows 10..59 from source
	inner := expressions.NewLogicalLimitExpression(50, 10, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Outer: skip 5, take 20 from inner result → rows 15..34 from source
	// Combined offset = 10 + 5 = 15
	// Available from inner after outer's skip = 50 - 5 = 45
	// Combined limit = min(20, 45) = 20
	outer := expressions.NewLogicalLimitExpression(20, 5, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetLimit() != 20 {
		t.Fatalf("limit = %d, want 20", merged.GetLimit())
	}
	if merged.GetOffset() != 15 {
		t.Fatalf("offset = %d, want 15", merged.GetOffset())
	}
}

func TestLimitMergeRule_OuterSkipsAll(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Inner takes 5 rows
	inner := expressions.NewLogicalLimitExpression(5, 0, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Outer skips 10 (more than inner produces) → 0 rows
	outer := expressions.NewLogicalLimitExpression(100, 10, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetLimit() != 0 {
		t.Fatalf("limit = %d, want 0 (outer skips all of inner)", merged.GetLimit())
	}
}

func TestLimitMergeRule_InnerUnlimited(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Inner: no limit (pure offset)
	inner := expressions.NewLogicalLimitExpression(-1, 20, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Outer: take 10, skip 5
	outer := expressions.NewLogicalLimitExpression(10, 5, innerQ)
	ref := expressions.InitialOf(outer)

	rule := NewLimitMergeRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	merged := results[0].(*expressions.LogicalLimitExpression)
	if merged.GetLimit() != 10 {
		t.Fatalf("limit = %d, want 10", merged.GetLimit())
	}
	if merged.GetOffset() != 25 {
		t.Fatalf("offset = %d, want 25 (20+5)", merged.GetOffset())
	}
}

func TestLimitMergeRule_DoesNotFireOnNonLimit(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Limit over scan (not over another limit)
	lim := expressions.NewLogicalLimitExpression(10, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewLimitMergeRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when inner is not a limit, got %d results", len(results))
	}
}
