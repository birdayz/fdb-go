package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestExactDataAccessResultTypesAgree(t *testing.T) {
	t.Parallel()

	rowA := values.NewRecordType("T", false, []values.Field{{
		Name:      "ID",
		Ordinal:   0,
		FieldType: values.NullableLong,
	}})
	rowB := values.NewRecordType("T", false, []values.Field{{
		Name:      "ID",
		Ordinal:   0,
		FieldType: values.NullableLong,
	}})

	scanA, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowA, false)
	if err != nil {
		t.Fatalf("construct first exact scan: %v", err)
	}
	scanB, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowB, false)
	if err != nil {
		t.Fatalf("construct second exact scan: %v", err)
	}
	if !exactDataAccessResultTypesAgree(scanA, scanB) {
		t.Fatal("separate expressions with the same exact row contract must agree")
	}

	notNullExplode, err := expressions.NewExplodeExpression(
		values.NewNullValue(&values.ArrayType{ElementType: values.NotNullLong}),
	)
	if err != nil {
		t.Fatalf("construct scalar matched expression: %v", err)
	}
	if exactDataAccessResultTypesAgree(scanA, notNullExplode) {
		t.Fatal("a candidate-top record must not enter a scalar matched Reference")
	}

	nullableExplode, err := plans.NewRecordQueryExplodePlan(
		values.NewNullValue(&values.ArrayType{ElementType: values.NullableLong}),
	)
	if err != nil {
		t.Fatalf("construct nullable scalar physical expression: %v", err)
	}
	if exactDataAccessResultTypesAgree(nullableExplode, notNullExplode) {
		t.Fatal("the result contract must preserve exact nullability")
	}
}
