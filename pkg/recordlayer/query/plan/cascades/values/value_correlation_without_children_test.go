package values

import "testing"

func TestGetCorrelatedToWithoutChildrenOfValue(t *testing.T) {
	t.Parallel()

	alias := NamedCorrelationIdentifier("root")
	assertOnlyAlias := func(t *testing.T, got map[CorrelationIdentifier]struct{}) {
		t.Helper()
		if len(got) != 1 {
			t.Fatalf("root correlations = %v, want {%s}", got, alias)
		}
		if _, present := got[alias]; !present {
			t.Fatalf("root correlations = %v, missing %s", got, alias)
		}
	}

	assertOnlyAlias(t, GetCorrelatedToWithoutChildrenOfValue(
		NewQuantifiedObjectValue(alias),
	))
	assertOnlyAlias(t, GetCorrelatedToWithoutChildrenOfValue(
		NewQuantifiedRecordValue(alias, UnknownType),
	))
	assertOnlyAlias(t, GetCorrelatedToWithoutChildrenOfValue(
		NewConstantObjectValue(alias, "constant", UnknownType),
	))

	field := NewFieldValue(
		NewQuantifiedObjectValue(alias),
		"x",
		NullableLong,
	)
	if got := GetCorrelatedToWithoutChildrenOfValue(field); len(got) != 0 {
		t.Fatalf("FieldValue root correlations = %v, want empty", got)
	}
	if got := GetCorrelatedToWithoutChildrenOfValue(
		NewExistsValueWithChild(NewQuantifiedObjectValue(alias)),
	); len(got) != 0 {
		t.Fatalf("ExistsValue root correlations = %v, want empty", got)
	}
	if got := GetCorrelatedToWithoutChildrenOfValue(
		NewRecordConstructorValue(
			RecordConstructorField{Name: "x", Value: field},
		),
	); len(got) != 0 {
		t.Fatalf("RecordConstructor root correlations = %v, want empty", got)
	}
}
