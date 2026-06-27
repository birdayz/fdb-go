package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

func TestPushLimitThroughProjectionRule_PreservesOffset(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "name", Typ: values.UnknownType}},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	lim := expressions.NewLogicalLimitExpression(10, 20, projQ)
	ref := expressions.InitialOf(lim)

	rule := NewPushLimitThroughProjectionRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule should fire with offset")
	}

	// Result should be Projection; inner should contain a Limit with offset=20
	result := results[0].(*expressions.LogicalProjectionExpression)
	innerRef := result.GetInner().GetRangesOver()
	found := false
	for _, m := range innerRef.Members() {
		if inner, ok := m.(*expressions.LogicalLimitExpression); ok {
			if inner.GetLimit() != 10 {
				t.Fatalf("limit = %d, want 10", inner.GetLimit())
			}
			if inner.GetOffset() != 20 {
				t.Fatalf("offset = %d, want 20", inner.GetOffset())
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected LogicalLimitExpression with preserved offset inside projection")
	}
}

func TestPushLimitThroughProjectionRule_MultiColumnProjection(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
			&values.FieldValue{Field: "c", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	lim := expressions.NewLogicalLimitExpression(1, 0, projQ)
	ref := expressions.InitialOf(lim)

	rule := NewPushLimitThroughProjectionRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule should fire regardless of projection width")
	}
	result := results[0].(*expressions.LogicalProjectionExpression)
	// Verify projection columns are preserved
	if len(result.GetProjectedValues()) != 3 {
		t.Fatalf("columns = %d, want 3", len(result.GetProjectedValues()))
	}
}
