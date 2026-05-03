package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushLimitThroughProjectionRule_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "x", Typ: values.UnknownType}},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	lim := expressions.NewLogicalLimitExpression(5, 0, projQ)
	ref := expressions.InitialOf(lim)

	rule := NewPushLimitThroughProjectionRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	// Result should be Projection over Limit
	result, ok := results[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected LogicalProjectionExpression at top, got %T", results[0])
	}

	// Check inner is a limit
	innerRef := result.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner Reference is nil")
	}
	found := false
	for _, m := range innerRef.Members() {
		if lim, ok := m.(*expressions.LogicalLimitExpression); ok {
			if lim.GetLimit() != 5 {
				t.Fatalf("limit = %d, want 5", lim.GetLimit())
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected LogicalLimitExpression inside projection")
	}
}

func TestPushLimitThroughProjectionRule_DoesNotFireOnFilter(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Limit over scan directly (no projection)
	lim := expressions.NewLogicalLimitExpression(5, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewPushLimitThroughProjectionRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when inner is not a projection, got %d results", len(results))
	}
}
