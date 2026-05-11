package properties

import (
	"testing"
)

func TestPropertyMap_GetBool_Present(t *testing.T) {
	t.Parallel()
	m := PropertyMap{PropDistinctRecords: true}
	if !m.GetBool(PropDistinctRecords) {
		t.Fatal("GetBool should return true for present true value")
	}
}

func TestPropertyMap_GetBool_PresentFalse(t *testing.T) {
	t.Parallel()
	m := PropertyMap{PropDistinctRecords: false}
	if m.GetBool(PropDistinctRecords) {
		t.Fatal("GetBool should return false for present false value")
	}
}

func TestPropertyMap_GetBool_Absent(t *testing.T) {
	t.Parallel()
	m := PropertyMap{}
	if m.GetBool(PropDistinctRecords) {
		t.Fatal("GetBool should return false for absent key")
	}
}

func TestPropertyMap_GetBool_NonBoolValue(t *testing.T) {
	t.Parallel()
	m := PropertyMap{PropDistinctRecords: "not a bool"}
	if m.GetBool(PropDistinctRecords) {
		t.Fatal("GetBool should return false for non-bool value")
	}
}

func TestPropertyMap_GetBool_NilMap(t *testing.T) {
	t.Parallel()
	var m PropertyMap
	if m.GetBool(PropDistinctRecords) {
		t.Fatal("GetBool should return false for nil map")
	}
}

func TestPropertyMap_GetOrdering_Present(t *testing.T) {
	t.Parallel()
	want := Ordering{IsKnown: true}
	m := PropertyMap{PropOrdering: want}
	got := m.GetOrdering()
	if got.IsKnown != want.IsKnown {
		t.Fatalf("GetOrdering = %+v, want %+v", got, want)
	}
}

func TestPropertyMap_GetOrdering_Absent(t *testing.T) {
	t.Parallel()
	m := PropertyMap{}
	got := m.GetOrdering()
	if got.IsKnown {
		t.Fatal("GetOrdering on absent key should return zero Ordering (IsKnown=false)")
	}
}

func TestPropertyMap_GetOrdering_NonOrderingValue(t *testing.T) {
	t.Parallel()
	m := PropertyMap{PropOrdering: 42}
	got := m.GetOrdering()
	if got.IsKnown {
		t.Fatal("GetOrdering with non-Ordering value should return zero Ordering")
	}
}

func TestPropertyMap_GetOrdering_NilMap(t *testing.T) {
	t.Parallel()
	var m PropertyMap
	got := m.GetOrdering()
	if got.IsKnown {
		t.Fatal("GetOrdering on nil map should return zero Ordering")
	}
}

func TestExpressionProperty_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prop *ExpressionProperty
		want string
	}{
		{PropOrdering, "ordering"},
		{PropDistinctRecords, "distinctRecords"},
		{PropStoredRecord, "storedRecord"},
		{PropPrimaryKey, "primaryKey"},
	}
	for _, tt := range tests {
		if got := tt.prop.String(); got != tt.want {
			t.Errorf("ExpressionProperty.String() = %q, want %q", got, tt.want)
		}
	}
}

func TestAllPlanProperties_Completeness(t *testing.T) {
	t.Parallel()
	expected := map[*ExpressionProperty]bool{
		PropOrdering:                  false,
		PropDistinctRecords:           false,
		PropStoredRecord:              false,
		PropPrimaryKey:                false,
		PropCardinalities:             false,
		PropComparisons:               false,
		PropExpressionCount:           false,
		PropFieldWithComparisonCount:  false,
		PropPredicateComplexity:       false,
		PropPredicateCountByLevel:     false,
		PropRecordTypes:               false,
		PropReferencesAndDependencies: false,
		PropUsedTypes:                 false,
		PropDerivations:               false,
	}
	if len(AllPlanProperties) != len(expected) {
		t.Fatalf("AllPlanProperties has %d entries, want %d", len(AllPlanProperties), len(expected))
	}
	for _, p := range AllPlanProperties {
		if _, ok := expected[p]; !ok {
			t.Fatalf("AllPlanProperties contains unexpected entry: %s", p.String())
		}
		expected[p] = true
	}
	for p, found := range expected {
		if !found {
			t.Fatalf("AllPlanProperties missing: %s", p.String())
		}
	}
}

func TestExpressionProperty_PointerIdentity(t *testing.T) {
	t.Parallel()
	// Singletons: two references to the same property must be the same pointer.
	a := PropDistinctRecords
	b := PropDistinctRecords
	if a != b {
		t.Fatal("ExpressionProperty singletons should be pointer-identical")
	}
	// Different properties must NOT be the same pointer.
	if PropDistinctRecords == PropStoredRecord {
		t.Fatal("different ExpressionProperty singletons should not be pointer-identical")
	}
}

func TestPropertyMap_MultipleProperties(t *testing.T) {
	t.Parallel()
	m := PropertyMap{
		PropDistinctRecords: true,
		PropStoredRecord:    false,
		PropOrdering:        Ordering{IsKnown: true},
		PropPrimaryKey:      "some-pk",
	}
	if !m.GetBool(PropDistinctRecords) {
		t.Fatal("distinctRecords should be true")
	}
	if m.GetBool(PropStoredRecord) {
		t.Fatal("storedRecord should be false")
	}
	if !m.GetOrdering().IsKnown {
		t.Fatal("ordering should be known")
	}
	if m[PropPrimaryKey] != "some-pk" {
		t.Fatalf("primaryKey = %v, want some-pk", m[PropPrimaryKey])
	}
}
