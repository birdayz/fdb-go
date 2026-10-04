package values

import "testing"

func TestSemanticEqualitySharedValueRespectsAliasMap(t *testing.T) {
	t.Parallel()
	a, b := NamedCorrelationIdentifier("a"), NamedCorrelationIdentifier("b")
	root, err := NewQuantifiedObjectValue(a, NewRecordType("T", false, []Field{{Name: "id", FieldType: NotNullLong}}))
	if err != nil {
		t.Fatal(err)
	}
	field, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	rename, err := NewAliasMap([]AliasPair{{Source: a, Target: b}})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewAliasMap([]AliasPair{{Source: a, Target: a}})
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]Value{"object": root, "field": field, "record": NewRawRecordConstructorValue(RecordConstructorField{Name: "id", Value: field})} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !SemanticEqualsUnderAliasMap(v, v, EmptyAliasMap()) || !SemanticEqualsUnderAliasMap(v, v, identity) {
				t.Fatal("shared value must equal itself under identity aliases")
			}
			if SemanticEqualsUnderAliasMap(v, v, rename) {
				t.Fatal("pointer identity bypassed a changed alias binding")
			}
		})
	}
}
