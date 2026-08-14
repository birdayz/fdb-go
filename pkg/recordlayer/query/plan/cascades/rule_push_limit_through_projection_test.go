package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func pushLimitProjectionRowType() *values.RecordType {
	return values.NewRecordType("PushLimitProjectionRow", false, []values.Field{
		{Name: "x", FieldType: values.NullableLong},
		{Name: "name", FieldType: values.NullableString},
		{Name: "a", FieldType: values.NullableLong},
		{Name: "b", FieldType: values.NullableLong},
		{Name: "c", FieldType: values.NullableLong},
	})
}

func mustPushLimitProjectionConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct push-limit/projection fixture: " + err.Error())
	}
	return value
}

func pushLimitProjectionScanQ() (*expressions.FullUnorderedScanExpression, expressions.Quantifier) {
	scan := mustPushLimitProjectionConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, pushLimitProjectionRowType()))
	return scan, expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func pushLimitProjectionField(q expressions.Quantifier, ordinal int) values.Value {
	root := mustPushLimitProjectionConstruct(q.RequireFlowedObjectValue())
	return mustPushLimitProjectionConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func firePushLimitProjectionRule(
	t testing.TB, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(NewPushLimitThroughProjectionRule(), ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func TestPushLimitThroughProjectionRule_Fires(t *testing.T) {
	t.Parallel()
	_ = NewPushLimitThroughProjectionRule() // direct behavioral-census anchor

	_, scanQ := pushLimitProjectionScanQ()

	proj := mustPushLimitProjectionConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{pushLimitProjectionField(scanQ, 0)},
		scanQ,
	))
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	lim := mustPushLimitProjectionConstruct(expressions.NewLogicalLimitExpression(5, 0, projQ))
	ref := expressions.InitialOf(lim)

	results := firePushLimitProjectionRule(t, ref)
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
	if got, want := result.GetInner().GetAlias(), proj.GetInner().GetAlias(); got != want {
		t.Fatalf("rewritten projection input alias = %v, want retained program owner %v", got, want)
	}
	correlated := values.GetCorrelatedToOfValue(result.GetProjectedValues()[0])
	if _, ok := correlated[result.GetInner().GetAlias()]; !ok {
		t.Fatalf("rewritten projection program correlations %v do not contain its input edge %v",
			correlated, result.GetInner().GetAlias())
	}
}

func TestPushLimitThroughProjectionRule_DoesNotFireOnFilter(t *testing.T) {
	t.Parallel()

	_, scanQ := pushLimitProjectionScanQ()

	// Limit over scan directly (no projection)
	lim := mustPushLimitProjectionConstruct(expressions.NewLogicalLimitExpression(5, 0, scanQ))
	ref := expressions.InitialOf(lim)

	results := firePushLimitProjectionRule(t, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when inner is not a projection, got %d results", len(results))
	}
}

func TestPushLimitThroughProjectionRule_PreservesOffset(t *testing.T) {
	t.Parallel()

	_, scanQ := pushLimitProjectionScanQ()

	proj := mustPushLimitProjectionConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{pushLimitProjectionField(scanQ, 1)},
		scanQ,
	))
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	lim := mustPushLimitProjectionConstruct(expressions.NewLogicalLimitExpression(10, 20, projQ))
	ref := expressions.InitialOf(lim)

	results := firePushLimitProjectionRule(t, ref)
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

	_, scanQ := pushLimitProjectionScanQ()

	proj := mustPushLimitProjectionConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{
			pushLimitProjectionField(scanQ, 2),
			pushLimitProjectionField(scanQ, 3),
			pushLimitProjectionField(scanQ, 4),
		},
		scanQ,
	))
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	lim := mustPushLimitProjectionConstruct(expressions.NewLogicalLimitExpression(1, 0, projQ))
	ref := expressions.InitialOf(lim)

	results := firePushLimitProjectionRule(t, ref)
	if len(results) == 0 {
		t.Fatal("rule should fire regardless of projection width")
	}
	result := results[0].(*expressions.LogicalProjectionExpression)
	// Verify projection columns are preserved
	if len(result.GetProjectedValues()) != 3 {
		t.Fatalf("columns = %d, want 3", len(result.GetProjectedValues()))
	}
}
