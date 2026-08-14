package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func typedInnerQuantifier(t *testing.T, alias string) (Quantifier, *values.RecordType) {
	t.Helper()
	row := rowOfTypes("A", values.NotNullLong, "B", values.NotNullString)
	return NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(alias),
		InitialOf(&typedStubExpr{name: "src" + alias, typ: row}),
	), row
}

func TestDMLAndAggregateResultsStateTheirActualRows(t *testing.T) {
	t.Parallel()
	inner, innerRow := typedInnerQuantifier(t, "IN")
	target := rowOfTypes("ID", values.NotNullLong)

	deleteExpression := mustExpression(NewDeleteExpression(inner, "T"))
	deleteQOV, ok := values.AsQuantifiedObjectValue(deleteExpression.GetResultValue())
	if !ok || deleteQOV.Correlation() != inner.GetAlias() || !deleteQOV.FlowedType().Equals(innerRow) {
		t.Fatalf("DELETE result = %v, want inner QOV %v", deleteExpression.GetResultValue(), innerRow)
	}

	insertExpression := mustExpression(NewInsertExpression(inner, "T", target))
	if !insertExpression.GetResultValue().Type().Equals(target) {
		t.Fatalf("INSERT result type = %v, want target %v", insertExpression.GetResultValue().Type(), target)
	}
	if len(values.GetCorrelatedToOfValue(insertExpression.GetResultValue())) != 0 {
		t.Fatal("INSERT target result is unexpectedly correlated to its source")
	}

	updateExpression := mustExpression(NewUpdateExpression(inner, "T", target, nil))
	updateRow := updateExpression.GetResultValue().Type().(*values.RecordType)
	if len(updateRow.Fields) != 2 || updateRow.Fields[0].Name != "OLD" || updateRow.Fields[1].Name != "NEW" ||
		!updateRow.Fields[0].FieldType.Equals(innerRow) || !updateRow.Fields[1].FieldType.Equals(target) {
		t.Fatalf("UPDATE result type = %v, want OLD %v / NEW %v", updateRow, innerRow, target)
	}

	groupBy := mustExpression(NewGroupByExpression(
		[]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		[]AggregateSpec{{Function: AggCount}},
		inner,
	))
	groupRow := groupBy.GetResultValue().Type().(*values.RecordType)
	if len(groupRow.Fields) != 2 || !groupRow.Fields[0].FieldType.Equals(values.NotNullLong) ||
		!groupRow.Fields[1].FieldType.Equals(values.NullableLong) {
		t.Fatalf("GROUP BY result type = %v, want grouping key plus nullable COUNT", groupRow)
	}
}

func TestSetOperationsStoreFirstNonExistentialExactQOV(t *testing.T) {
	t.Parallel()
	existential, _ := typedInnerQuantifier(t, "E")
	existential.kind = QuantifierExistential
	forEach, row := typedInnerQuantifier(t, "R")

	for _, expression := range []RelationalExpression{
		mustExpression(NewLogicalUnionExpression([]Quantifier{existential, forEach})),
		mustExpression(NewLogicalIntersectionExpression([]Quantifier{existential, forEach}, nil)),
	} {
		qov, ok := values.AsQuantifiedObjectValue(expression.GetResultValue())
		if !ok || qov.Correlation() != forEach.GetAlias() || !qov.FlowedType().Equals(row) {
			t.Fatalf("set-operation result = %v, want first non-existential QOV", expression.GetResultValue())
		}
		if expression.GetResultValue() != qov {
			t.Fatal("set operation did not return its construction-time stable result")
		}
	}
}
