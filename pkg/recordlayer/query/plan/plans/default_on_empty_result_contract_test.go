package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func defaultOnEmptyContractRow(name string) *values.RecordType {
	return values.NewRecordType(name, false, []values.Field{{
		Name:      "ID",
		FieldType: values.NotNullLong,
		Ordinal:   0,
	}})
}

func defaultOnEmptyContractScan(t testing.TB, rowType values.Type) *RecordQueryScanPlan {
	t.Helper()
	plan, err := NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	if err != nil {
		t.Fatalf("construct DefaultOnEmpty child: %v", err)
	}
	return plan
}

func TestDefaultOnEmptyResultContract(t *testing.T) {
	t.Parallel()

	rowType := defaultOnEmptyContractRow("default_on_empty_row")
	child := defaultOnEmptyContractScan(t, rowType)
	plan, err := NewRecordQueryDefaultOnEmptyPlan(
		child, values.NewNullValue(rowType))
	if err != nil {
		t.Fatalf("construct DefaultOnEmpty: %v", err)
	}

	wantType := values.WithNullability(rowType, true)
	if !plan.GetResultType().Equals(wantType) {
		t.Fatalf("DefaultOnEmpty result type = %v, want %v", plan.GetResultType(), wantType)
	}
	if plan.GetResultValue() != plan.GetResultValue() {
		t.Fatal("DefaultOnEmpty result Value is not stable")
	}
	resultRoot, ok := values.AsQuantifiedObjectValue(plan.GetResultValue())
	if !ok || !resultRoot.FlowedType().Equals(wantType) {
		t.Fatalf("DefaultOnEmpty result = %T/%v, want exact nullable QOV", plan.GetResultValue(), plan.GetResultType())
	}

	childLayout, err := child.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("child layout: %v", err)
	}
	properties, err := plan.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("DefaultOnEmpty properties: %v", err)
	}
	required := properties.RequiredInputLayouts()
	if len(required) != 1 {
		t.Fatalf("DefaultOnEmpty required input layouts = %d, want 1", len(required))
	}
	satisfied, err := required[0].SatisfiedBy(childLayout)
	if err != nil || !satisfied {
		t.Fatalf("child layout does not satisfy DefaultOnEmpty input requirement: satisfied=%v err=%v", satisfied, err)
	}
	outputLayout := properties.ProvidedOutputLayout()
	if outputLayout.Carrier() != resultRoot || !outputLayout.Carrier().FlowedType().Equals(wantType) {
		t.Fatal("DefaultOnEmpty output layout is not owned by its nullable result carrier")
	}
	if satisfied, err := required[0].SatisfiedBy(outputLayout); err != nil || satisfied {
		t.Fatalf("nullable output layout satisfied non-null child requirement: satisfied=%v err=%v", satisfied, err)
	}
}

func TestDefaultOnEmptyRejectsIncompatibleOrUnresolvedDefaults(t *testing.T) {
	t.Parallel()

	childType := defaultOnEmptyContractRow("default_on_empty_row")
	child := defaultOnEmptyContractScan(t, childType)
	tests := []struct {
		name         string
		defaultValue values.Value
	}{
		{name: "nil", defaultValue: nil},
		{name: "unresolved", defaultValue: values.NewNullValue(values.UnknownType)},
		{name: "different exact row", defaultValue: values.NewNullValue(
			defaultOnEmptyContractRow("different_row"))},
		{name: "different exact scalar", defaultValue: values.NewNullValue(values.NotNullLong)},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			plan, err := NewRecordQueryDefaultOnEmptyPlan(child, testCase.defaultValue)
			if err == nil || plan != nil {
				t.Fatalf("NewRecordQueryDefaultOnEmptyPlan(default=%v) = (%v, %v), want (nil, error)",
					testCase.defaultValue, plan, err)
			}
		})
	}
}

func TestDefaultOnEmptyRebuildRevalidatesResultContract(t *testing.T) {
	t.Parallel()

	rowType := defaultOnEmptyContractRow("default_on_empty_row")
	original := defaultOnEmptyContractScan(t, rowType)
	plan, err := NewRecordQueryDefaultOnEmptyPlan(
		original, values.NewNullValue(rowType))
	if err != nil {
		t.Fatalf("construct DefaultOnEmpty: %v", err)
	}

	replacementType := defaultOnEmptyContractRow("incompatible_replacement")
	replacement := defaultOnEmptyContractScan(t, replacementType)
	rebuilt, err := plan.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{replacement}))
	if err == nil || rebuilt != nil {
		t.Fatalf("WithQuantifiers(incompatible child) = (%v, %v), want (nil, error)", rebuilt, err)
	}
}
