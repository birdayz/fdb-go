package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func projectionElimRowType() *values.RecordType {
	return values.NewRecordType("ProjectionElimRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
}

func mustProjectionElimConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct projection-elim fixture: " + err.Error())
	}
	return value
}

func projectionElimScanQ() (*expressions.FullUnorderedScanExpression, expressions.Quantifier) {
	scan := mustProjectionElimConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, projectionElimRowType()))
	return scan, expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func fireProjectionElimRule(
	t testing.TB, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(NewProjectionElimRule(), ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func TestProjectionElimRule_WholeRowIdentityRejectedAtAdmission(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	root := mustProjectionElimConstruct(q.RequireFlowedObjectValue())
	p, err := expressions.NewLogicalProjectionExpression([]values.Value{root}, q)
	if !errors.Is(err, values.ErrWholeRowProjection) || p != nil {
		t.Fatalf("whole-row identity projection = %T, %v; want ErrWholeRowProjection", p, err)
	}
}

func TestProjectionElimRule_DeclinesOnMultipleColumns(t *testing.T) {
	t.Parallel()
	_ = NewProjectionElimRule() // direct behavioral-census anchor; helper fires it
	_, q := projectionElimScanQ()
	root := mustProjectionElimConstruct(q.RequireFlowedObjectValue())
	p := mustProjectionElimConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{root, values.NewBooleanValue(true)},
		q,
	))
	ref := expressions.InitialOf(p)
	yielded := fireProjectionElimRule(t, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on multi-column projection — yielded %d, want 0", len(yielded))
	}
}

func TestProjectionElimRule_DeclinesOnComputedSingle(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	// Single Value, but it's NOT the flowed object (computed expression).
	p := mustProjectionElimConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{values.NewBooleanValue(true)},
		q,
	))
	ref := expressions.InitialOf(p)
	yielded := fireProjectionElimRule(t, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a computed projection — yielded %d, want 0", len(yielded))
	}
}

func TestProjectionElimRule_DeclinesOnDifferentAlias(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	otherAlias := values.NamedCorrelationIdentifier("OTHER")
	other := mustProjectionElimConstruct(values.NewQuantifiedObjectValue(otherAlias, values.NotNullLong))
	p := mustProjectionElimConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{other},
		q,
	))
	ref := expressions.InitialOf(p)
	yielded := fireProjectionElimRule(t, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on projection of different-alias QOV — yielded %d, want 0", len(yielded))
	}
}

func TestProjectionElimRule_DeclinesOnOutputAlias(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	root := mustProjectionElimConstruct(q.RequireFlowedObjectValue())
	p, err := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{root},
		[]string{"RENAMED_ROW"},
		q,
	)
	if !errors.Is(err, values.ErrWholeRowProjection) || p != nil {
		t.Fatalf("aliased whole-row projection = %T, %v; want ErrWholeRowProjection", p, err)
	}
}

func TestProjectionElimRule_ExplicitEmptyAliasStillRejectsWholeRow(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	root := mustProjectionElimConstruct(q.RequireFlowedObjectValue())
	p, err := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{root},
		[]string{""},
		q,
	)
	if !errors.Is(err, values.ErrWholeRowProjection) || p != nil {
		t.Fatalf("empty-alias whole-row projection = %T, %v; want ErrWholeRowProjection", p, err)
	}
}
