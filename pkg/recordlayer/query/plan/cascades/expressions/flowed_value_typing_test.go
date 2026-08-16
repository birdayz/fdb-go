package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestExactQOVTypeParticipatesInIdentity(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("Q")
	one := mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullLong))
	two := mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullString))
	if values.EqualsWithoutChildren(one, two) {
		t.Fatal("same-correlation QOVs with different exact types compared equal")
	}
	if values.SemanticEqualsUnderAliasMap(one, two, values.EmptyAliasMap()) {
		t.Fatal("alias-aware equality erased the QOV exact-type discriminator")
	}
	if values.SemanticHashCode(one) == values.SemanticHashCode(two) {
		t.Fatal("differently typed QOVs produced the same semantic hash")
	}
}

// TestLogicalProjectionRejectsWholeRowShapeWithoutPublishingObject drives the
// refusal through the EXPRESSION constructor, over a MACHINERY quantifier —
// the shape whose one emitted slot has no name to take.
//
// The named twin below is the other half, and the pair is the whole point: the
// refusal is about WHOSE row is being wrapped, not about the row being a row.
// A `SELECT x` over a struct-valued source is one named column and must build.
func TestLogicalProjectionRejectsWholeRowShapeWithoutPublishingObject(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(
		InitialOf(&typedStubExpr{name: "source", typ: rowOfTypes("A", values.NotNullLong)}),
	)
	wholeRow := mustExpression(inner.RequireFlowedObjectValue())
	projection, err := NewLogicalProjectionExpression([]values.Value{wholeRow}, inner)
	if projection != nil || !errors.Is(err, values.ErrWholeRowProjection) {
		t.Fatalf("whole-row projection returned (%v, %v), want nil and ErrWholeRowProjection", projection, err)
	}
}

func TestLogicalProjectionOverANamedSourceIsAColumnNotAWholeRowWrap(t *testing.T) {
	t.Parallel()
	inner := NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("X"),
		InitialOf(&typedStubExpr{name: "source", typ: rowOfTypes("A", values.NotNullLong)}),
	)
	element := mustExpression(inner.RequireFlowedObjectValue())
	projection, err := NewLogicalProjectionExpression([]values.Value{element}, inner)
	if err != nil || projection == nil {
		t.Fatalf("projecting a NAMED source whole = (%v, %v), want a projection: it is "+
			"`SELECT x`, one column named x, whatever the element's type", projection, err)
	}
}

func TestLogicalProjectionStoresExactProjectedRow(t *testing.T) {
	t.Parallel()
	inner := NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("IN"),
		InitialOf(&typedStubExpr{name: "source", typ: rowOfTypes("A", values.NotNullLong, "B", values.NotNullString)}),
	)
	projection := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		[]string{"RENAMED"},
		inner,
	))
	result, ok := projection.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("projection result = %T, want RecordConstructorValue", projection.GetResultValue())
	}
	row := result.Type().(*values.RecordType)
	if len(row.Fields) != 1 || row.Fields[0].Name != "RENAMED" || !row.Fields[0].FieldType.Equals(values.NotNullLong) {
		t.Fatalf("projection row = %v, want RENAMED NotNullLong", row)
	}
	if projection.GetResultValue() != result {
		t.Fatal("projection did not return its construction-time stable result")
	}
}
