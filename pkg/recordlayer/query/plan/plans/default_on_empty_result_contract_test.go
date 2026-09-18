package plans

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// defaultOnEmptyContractRow builds a row whose SHAPE is keyed by name: two
// different names give two genuinely different exact rows, one name always
// gives the same one.
//
// The name must reach a FIELD. A record's RecordName is provenance and does not
// participate in type identity (Java's Type.Record.equals compares typeCode,
// nullability and fields only), so a helper that varied only the RecordName
// handed every "incompatible child" / "different exact row" case a row that was
// in fact IDENTICAL — those rejections then had nothing to reject.
func defaultOnEmptyContractRow(name string) *values.RecordType {
	return values.NewRecordType(name, false, []values.Field{{
		Name:      "ID_" + strings.ToUpper(name),
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

func TestFirstOrDefaultResultContract(t *testing.T) {
	t.Parallel()
	for _, childType := range []values.Type{
		values.NotNullLong, values.NullableLong,
		defaultOnEmptyContractRow("first_default"),
		values.WithNullability(defaultOnEmptyContractRow("first_default"), true),
	} {
		for _, defaultNullable := range []bool{false, true} {
			for _, strict := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/default_nullable=%t/strict=%t", childType, defaultNullable, strict), func(t *testing.T) {
					t.Parallel()
					child := defaultOnEmptyContractScan(t, childType)
					wantType := values.WithNullability(childType, childType.IsNullable() || defaultNullable)
					var fallback values.Value = structuralLong(0)
					if record, ok := childType.(*values.RecordType); ok {
						fallback = values.NewRawRecordConstructorValue(values.RecordConstructorField{
							Name: record.Fields[0].Name, Value: structuralLong(0),
						})
					}
					if defaultNullable {
						fallback = values.NewNullValue(childType)
					}
					construct := NewRecordQueryFirstOrDefaultPlan
					if strict {
						construct = NewRecordQueryFirstOrDefaultPlanStrict
					}
					plan, err := construct(child, fallback)
					if err != nil {
						t.Fatal(err)
					}
					if !plan.GetResultType().Equals(wantType) {
						t.Fatalf("FirstOrDefault type = %s, want %s from both result alternatives", plan.GetResultType(), wantType)
					}
					if plan.IsStrict() != strict || plan.GetDefaultValue() != fallback {
						t.Fatal("construction lost strictness or default expression")
					}
					childLayout, err := child.ProvidedOutputLayout()
					if err != nil {
						t.Fatal(err)
					}
					properties, err := plan.OrdinalPhysicalProperties()
					if err != nil {
						t.Fatal(err)
					}
					required := properties.RequiredInputLayouts()
					if len(required) != 1 {
						t.Fatalf("input requirements = %d, want 1", len(required))
					}
					if satisfied, err := required[0].SatisfiedBy(childLayout); err != nil || !satisfied {
						t.Fatalf("child layout rejected: satisfied=%t err=%v", satisfied, err)
					}
					output := properties.ProvidedOutputLayout()
					if output.Carrier() == childLayout.Carrier() {
						t.Fatal("default arm improperly claims the child's current carrier")
					}
					if plan.GetResultValue() != output.Carrier() || !output.Carrier().FlowedType().Equals(wantType) {
						t.Fatalf("output carrier/result identity or declared type lost: %v", output)
					}
					rebuilt, err := plan.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{child}))
					if err != nil {
						t.Fatal(err)
					}
					copyPlan := rebuilt.(*RecordQueryFirstOrDefaultPlan)
					if !copyPlan.GetResultType().Equals(wantType) || copyPlan.IsStrict() != strict || copyPlan.GetDefaultValue() != fallback {
						t.Fatal("rebuild lost derived type, strictness or default expression")
					}
				})
			}
		}
	}
}

func TestFirstOrDefaultRejectsIncompatibleDefaultsAndRebuilds(t *testing.T) {
	t.Parallel()
	childType := defaultOnEmptyContractRow("first_default")
	child := defaultOnEmptyContractScan(t, childType)
	for _, test := range []struct {
		name  string
		value values.Value
	}{
		{"nil", nil},
		{"unresolved", values.NewNullValue(values.UnknownType)},
		{"scalar", values.NewNullValue(values.NotNullLong)},
		{"different_row", values.NewNullValue(defaultOnEmptyContractRow("other"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if plan, err := NewRecordQueryFirstOrDefaultPlan(child, test.value); err == nil || plan != nil {
				t.Fatalf("default %s admitted: plan=%v err=%v", test.name, plan, err)
			}
		})
	}
	plan, err := NewRecordQueryFirstOrDefaultPlan(child, values.NewNullValue(childType))
	if err != nil {
		t.Fatal(err)
	}
	replacement := defaultOnEmptyContractScan(t, defaultOnEmptyContractRow("other"))
	if rebuilt, err := plan.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{replacement})); err == nil || rebuilt != nil {
		t.Fatalf("incompatible child rebuild admitted: plan=%v err=%v", rebuilt, err)
	}
}

func TestDefaultResultLineageUsesOnlyExactChildAndOutputCarriers(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"first", "strict", "all"} {
		for _, scalar := range []bool{false, true} {
			for _, nullableChild := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/scalar=%t/nullable_child=%t", kind, scalar, nullableChild), func(t *testing.T) {
					t.Parallel()
					var typ values.Type = defaultOnEmptyContractRow("lineage")
					if scalar {
						typ = values.NotNullLong
					}
					typ = values.WithNullability(typ, nullableChild)
					child := defaultOnEmptyContractScan(t, typ)
					childLayout := requireProvidedLayout(t, child)
					construct := func(inner RecordQueryPlan) RecordQueryPlan {
						t.Helper()
						var plan RecordQueryPlan
						var err error
						fallback := values.NewNullValue(typ)
						switch kind {
						case "first":
							plan, err = NewRecordQueryFirstOrDefaultPlan(inner, fallback)
						case "strict":
							plan, err = NewRecordQueryFirstOrDefaultPlanStrict(inner, fallback)
						case "all":
							plan, err = NewRecordQueryDefaultOnEmptyPlan(inner, fallback)
						}
						if err != nil {
							t.Fatal(err)
						}
						return plan
					}
					// The first boundary widens a non-null child or changes only the
					// owner of an already-nullable child. The next must cross both.
					inner := construct(child)
					outer := construct(inner)
					output := requireProvidedLayout(t, outer).Carrier()
					lineage := outer.(inputValueMaterializer)
					whole, err := lineage.reanchorInputValueToOutput(childLayout.Carrier())
					if err != nil || whole != output {
						t.Fatalf("whole child did not cross both default boundaries: %v, %v", whole, err)
					}
					resolve := func(root values.QuantifiedObjectValue) values.Value {
						t.Helper()
						if scalar {
							return root
						}
						field, err := values.ResolveFieldOrdinals(root, []int{0})
						if err != nil {
							t.Fatal(err)
						}
						return field
					}
					inputValue, outputValue := resolve(childLayout.Carrier()), resolve(output)
					mixed := &values.ArithmeticValue{Op: values.OpAdd, Left: outputValue, Right: inputValue}
					translated, err := lineage.reanchorInputValueToOutput(mixed)
					if err != nil {
						t.Fatal(err)
					}
					arithmetic, ok := translated.(*values.ArithmeticValue)
					if !ok || arithmetic.Left != outputValue || !translated.Type().Equals(values.NullableLong) {
						t.Fatalf("mixed output/input program changed its output-relative arm or type: %v", translated)
					}
					rightRoot := arithmetic.Right
					if !scalar {
						field, ok := values.AsFieldValue(rightRoot)
						if !ok || len(field.Path().Ordinals()) != 1 || field.Path().Ordinals()[0] != 0 || !field.Type().Equals(values.NullableLong) {
							t.Fatalf("field path or leaf type changed: %v", rightRoot)
						}
						rightRoot = field.ChildValue()
					}
					if rightRoot != output || mixed.Right != inputValue || mixed.Left != outputValue {
						t.Fatal("mixed program was not reanchored copy-on-write onto the exact output")
					}
					foreignPlan := construct(child)
					foreign := resolve(requireProvidedLayout(t, foreignPlan).Carrier())
					unchanged, err := lineage.reanchorInputValueToOutput(foreign)
					if err != nil || unchanged != foreign {
						t.Fatalf("same-shaped foreign current was granted child lineage: %v, %v", unchanged, err)
					}
					if !scalar {
						named, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("FOREIGN"), typ)
						if err != nil {
							t.Fatal(err)
						}
						foreignField := resolve(named)
						unchanged, err := lineage.reanchorInputValueToOutput(foreignField)
						if err != nil || unchanged != foreignField {
							t.Fatalf("same-named field gained unproven lineage: %v, %v", unchanged, err)
						}
					}
				})
			}
		}
	}
}
