package values

import "testing"

// TestSimplifyCoalesce_HeadClassesPerSet pins the remaining COALESCE head
// classes of Java's EvaluateConstantCoalesceRule (WS-E design 5.4(d)/(j)) in
// both value sets, each run to a fixpoint (five passes, the result fed back):
//   - a `NOT FALSE` head is not a BOOLEAN literal (Java keeps `NOT 'false'`
//     unevaluated), so neither set ever folds the COALESCE, however often it
//     runs;
//   - an arithmetic head over literals is not evaluated by either set;
//   - a `CAST(NULL AS BOOLEAN)` head collapses to NULL (the null-strict
//     collapse, both sets) and the PREDICATE set then skips it and returns the
//     next BOOLEAN literal; the DEFAULT set never folds a COALESCE.
func TestSimplifyCoalesce_HeadClassesPerSet(t *testing.T) {
	t.Parallel()
	tail := &fieldValue{Field: "x", Typ: NullableBoolean}
	fixpoint := func(simplify func(Value) Value, v Value) Value {
		for i := 0; i < 5; i++ {
			v = simplify(v)
		}
		return v
	}
	sets := []struct {
		name     string
		simplify func(Value) Value
	}{{"default", SimplifyValue}, {"predicate", SimplifyPredicateValue}}

	notFalse := NewScalarFunctionValue("COALESCE", NullableBoolean, NewNotValue(NewBooleanValue(false)), tail)
	for _, set := range sets {
		got := fixpoint(set.simplify, notFalse)
		sf, ok := got.(*ScalarFunctionValue)
		if !ok || sf.FuncName != "COALESCE" || len(sf.Args) != 2 {
			t.Fatalf("%s set folded COALESCE(NOT FALSE, x) to %s", set.name, ExplainValue(got))
		}
		if _, ok := sf.Args[0].(*NotValue); !ok {
			t.Fatalf("%s set rewrote the NOT FALSE head to %s", set.name, ExplainValue(sf.Args[0]))
		}
	}

	sum, err := NewArithmeticValue(OpAdd, &ConstantValue{Value: int64(1), Typ: NotNullLong}, &ConstantValue{Value: int64(1), Typ: NotNullLong})
	if err != nil {
		t.Fatal(err)
	}
	arith := NewScalarFunctionValue("COALESCE", NullableLong, sum, &fieldValue{Field: "y", Typ: NullableLong})
	for _, set := range sets {
		got := fixpoint(set.simplify, arith)
		sf, ok := got.(*ScalarFunctionValue)
		if !ok || len(sf.Args) != 2 {
			t.Fatalf("%s set folded COALESCE(1 + 1, y) to %s", set.name, ExplainValue(got))
		}
		if _, ok := sf.Args[0].(*ArithmeticValue); !ok {
			t.Fatalf("%s set evaluated the 1 + 1 head to %s", set.name, ExplainValue(sf.Args[0]))
		}
	}

	castNull := NewScalarFunctionValue("COALESCE", NullableBoolean,
		NewCastValue(NewNullValue(NullableBoolean), NullableBoolean), NewBooleanValue(true), tail)
	if got := fixpoint(SimplifyPredicateValue, castNull); effectiveBool(got) != "true" {
		t.Fatalf("predicate set: COALESCE(CAST(NULL AS BOOLEAN), TRUE, x) = %s, want TRUE", ExplainValue(got))
	}
	if got, ok := fixpoint(SimplifyValue, castNull).(*ScalarFunctionValue); !ok || got.FuncName != "COALESCE" {
		t.Fatalf("default set folded the COALESCE")
	}
}

// effectiveBool renders a BOOLEAN literal result ("true", "false", "null"), or
// "" for anything else.
func effectiveBool(v Value) string {
	switch b := v.(type) {
	case *BooleanValue:
		if b.Value == nil {
			return "null"
		}
		if *b.Value {
			return "true"
		}
		return "false"
	case *ConstantValue:
		if bv, ok := b.Value.(bool); ok {
			if bv {
				return "true"
			}
			return "false"
		}
	}
	return ""
}
