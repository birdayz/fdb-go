package recordlayer

import (
	"errors"
	"slices"
	"sort"
	"testing"

	"fdb.dev/gen"
)

// TestKeyFunctionRegistry_BuiltInsAreTheTargetsCore pins the names this
// package registers at init to the target's core registry, measured on the JVM
// by conformance/function_registry_conformance_test.go (javaCoreKeyFunctions),
// which also compares each one's bounds, column size and null, plus
// collate_icu, which the target's ICU module registers.
func TestKeyFunctionRegistry_BuiltInsAreTheTargetsCore(t *testing.T) {
	t.Parallel()
	want := []string{
		"add", "bitand", "bitmap_bit_position", "bitmap_bucket_offset", "bitnot", "bitor", "bitxor",
		"cardinality", "collate_icu", "collate_jre", "div", "divide", "mod", "mul", "multiply",
		"order_asc_nulls_first", "order_asc_nulls_last", "order_desc_nulls_first", "order_desc_nulls_last",
		"sub", "subtract",
	}
	var got []string
	for name := range coreKeyFunctions {
		got = append(got, name)
		if _, ok := LookupFunction(name); !ok {
			t.Errorf("core function %s is not in the registry", name)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("core key functions = %v, want the target's %v", got, want)
	}
}

// TestKeyFunctionRegistry_BuildRefusesWhatCreateRefuses pins that Build
// returns FunctionKeyExpression.create's refusal, first in program order.
func TestKeyFunctionRegistry_BuildRefusesWhatCreateRefuses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		expr KeyExpression
		want string
	}{
		{FunctionExpr("get_versionstamp_incarnation", EmptyKey()), "Function not defined"},
		{FunctionExpr("add", Field("price")), "Invalid number of arguments provided to function"},
		{CardinalityExpr(Concat(Field("price"), Field("quantity"))), "Invalid number of arguments provided to function"},
		{Concat(Field("order_id"), FunctionExpr("bitnot", Concat(Field("price"), Field("quantity")))), "Invalid number of arguments provided to function"},
	} {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", NewIndex("fn_idx", c.expr))
		_, err := b.Build()
		var keyErr *KeyExpressionError
		if !errors.As(err, &keyErr) || keyErr.Message != c.want {
			t.Errorf("Build over %v = %v, want KeyExpressionError %q", c.expr.ToKeyExpression(), err, c.want)
		}
	}
	// The earlier of two refusals is the one returned: a Then of one child
	// built before the function.
	one := Concat(Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.AddIndex("Order", NewIndex("fn_idx", FunctionExpr("no_such_function", one)))
	if _, err := b.Build(); err == nil || err.Error() != "Then must have at least 2 children" {
		t.Errorf("Build = %v, want the Then refusal that came first", err)
	}
}
