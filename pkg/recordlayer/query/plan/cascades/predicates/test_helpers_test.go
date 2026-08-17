package predicates

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func predicateTestField(t testing.TB, name string, typ values.Type) values.Value {
	t.Helper()
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		typ = values.NullableLong
	}
	rowType := values.NewRecordType("predicate_test_"+name, false, []values.Field{
		{Name: name, FieldType: typ, Ordinal: 0},
	})
	root := mustQOV(t, values.NamedCorrelationIdentifier("predicate_test_"+name), rowType)
	request, err := values.FieldByNameAndOrdinal(name, 0)
	if err != nil {
		t.Fatalf("field %q request: %v", name, err)
	}
	field, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
	if err != nil {
		t.Fatalf("field %q: %v", name, err)
	}
	return field
}

func predicateTestFieldForAlias(t testing.TB, alias values.CorrelationIdentifier, name string, typ values.Type) values.Value {
	t.Helper()
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		typ = values.NullableLong
	}
	rowType := values.NewRecordType("predicate_alias_"+name, false, []values.Field{
		{Name: name, FieldType: typ, Ordinal: 0},
	})
	root := mustQOV(t, alias, rowType)
	resolved, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("field %q: %v", name, err)
	}
	return resolved
}

func mustResolveFieldOrdinals(t testing.TB, child values.Value, ordinals ...int) values.Value {
	t.Helper()
	resolved, err := values.ResolveFieldOrdinals(child, ordinals)
	if err != nil {
		t.Fatalf("resolve field ordinals %v: %v", ordinals, err)
	}
	return resolved
}

func predicateTestFieldAt(t testing.TB, name string, ordinal int, typ values.Type) values.Value {
	t.Helper()
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		typ = values.NullableLong
	}
	fields := make([]values.Field, ordinal+1)
	for i := range fields {
		fieldName := fmt.Sprintf("_padding_%d", i)
		if i == ordinal {
			fieldName = name
		}
		fields[i] = values.Field{Name: fieldName, FieldType: typ, Ordinal: i}
	}
	rowType := values.NewRecordType(fmt.Sprintf("predicate_test_%s_%d", name, ordinal), false, fields)
	root := mustQOV(t, values.NamedCorrelationIdentifier(fmt.Sprintf("predicate_test_%s_%d", name, ordinal)), rowType)
	request, err := values.FieldByNameAndOrdinal(name, ordinal)
	if err != nil {
		t.Fatalf("field %q request: %v", name, err)
	}
	field, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
	if err != nil {
		t.Fatalf("field %q: %v", name, err)
	}
	return field
}

func mustQOV(t testing.TB, correlation values.CorrelationIdentifier, flowed ...values.Type) values.QuantifiedObjectValue {
	t.Helper()
	typ := values.NullableLong
	if len(flowed) > 0 {
		typ = flowed[0]
	}
	qov, err := values.NewQuantifiedObjectValue(correlation, typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%q, %v): %v", correlation, typ, err)
	}
	return qov
}

func mustExistentialAlias(t testing.TB, alias values.CorrelationIdentifier) *ExistentialValuePredicate {
	t.Helper()
	predicate, err := NewExistentialAlias(alias, values.NullableLong)
	if err != nil {
		t.Fatalf("NewExistentialAlias(%q): %v", alias, err)
	}
	return predicate
}

func mustAliasMap(t testing.TB, pairs ...values.AliasPair) values.AliasMap {
	t.Helper()
	aliases, err := values.NewAliasMap(pairs)
	if err != nil {
		t.Fatalf("NewAliasMap(%v): %v", pairs, err)
	}
	return aliases
}
