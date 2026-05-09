package values

import "testing"

func TestRebaseValue_QOV(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &QuantifiedObjectValue{Correlation: oldAlias}
	result := RebaseValue(v, AliasMap{oldAlias: newAlias})
	qov, ok := result.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", result)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebaseValue_QOV_NoMatch(t *testing.T) {
	t.Parallel()
	v := &QuantifiedObjectValue{Correlation: NamedCorrelationIdentifier("other")}
	result := RebaseValue(v, AliasMap{
		NamedCorrelationIdentifier("old"): NamedCorrelationIdentifier("new"),
	})
	if result != v {
		t.Fatal("non-matching QOV should return same pointer")
	}
}

func TestRebaseValue_Field(t *testing.T) {
	t.Parallel()
	v := &FieldValue{Field: "x", Typ: UnknownType}
	result := RebaseValue(v, AliasMap{
		NamedCorrelationIdentifier("old"): NamedCorrelationIdentifier("new"),
	})
	if result != v {
		t.Fatal("FieldValue should return same pointer (no correlation)")
	}
}

func TestRebaseValue_Constant(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: 42}
	result := RebaseValue(v, AliasMap{
		NamedCorrelationIdentifier("old"): NamedCorrelationIdentifier("new"),
	})
	if result != v {
		t.Fatal("ConstantValue should return same pointer")
	}
}

func TestRebaseValue_Nil(t *testing.T) {
	t.Parallel()
	result := RebaseValue(nil, AliasMap{})
	if result != nil {
		t.Fatal("nil value should return nil")
	}
}

func TestRebaseValue_EmptyAliasMap(t *testing.T) {
	t.Parallel()
	v := &QuantifiedObjectValue{Correlation: NamedCorrelationIdentifier("x")}
	result := RebaseValue(v, nil)
	if result != v {
		t.Fatal("nil alias map should return same pointer")
	}
}

func TestRebaseValue_ArithmeticRecursion(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &QuantifiedObjectValue{Correlation: oldAlias},
		Right: &ConstantValue{Value: 1},
	}
	result := RebaseValue(v, AliasMap{oldAlias: newAlias})
	arith, ok := result.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", result)
	}
	qov, ok := arith.Left.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected left to be *QuantifiedObjectValue, got %T", arith.Left)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
	if arith.Right != v.Right {
		t.Fatal("right side (constant) should be preserved")
	}
}

func TestRebaseValue_ArithmeticNoChange(t *testing.T) {
	t.Parallel()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: 1},
		Right: &ConstantValue{Value: 2},
	}
	result := RebaseValue(v, AliasMap{
		NamedCorrelationIdentifier("old"): NamedCorrelationIdentifier("new"),
	})
	if result != v {
		t.Fatal("arithmetic with no matching aliases should return same pointer")
	}
}

func TestRebaseValue_CastRecursion(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := NewCastValue(&QuantifiedObjectValue{Correlation: oldAlias}, TypeInt)
	result := RebaseValue(v, AliasMap{oldAlias: newAlias})
	cast, ok := result.(*CastValue)
	if !ok {
		t.Fatalf("expected *CastValue, got %T", result)
	}
	qov, ok := cast.Child.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected child to be *QuantifiedObjectValue, got %T", cast.Child)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebaseValue_CastNoChange(t *testing.T) {
	t.Parallel()
	v := NewCastValue(&ConstantValue{Value: 42}, TypeInt)
	result := RebaseValue(v, AliasMap{
		NamedCorrelationIdentifier("old"): NamedCorrelationIdentifier("new"),
	})
	if result != v {
		t.Fatal("cast with no matching aliases should return same pointer")
	}
}

func TestRebaseValue_ScalarFunction(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &ScalarFunctionValue{
		FuncName: "COALESCE",
		Args: []Value{
			&QuantifiedObjectValue{Correlation: oldAlias},
			&ConstantValue{Value: 0},
		},
		Typ: UnknownType,
	}
	result := RebaseValue(v, AliasMap{oldAlias: newAlias})
	fn, ok := result.(*ScalarFunctionValue)
	if !ok {
		t.Fatalf("expected *ScalarFunctionValue, got %T", result)
	}
	if fn.FuncName != "COALESCE" {
		t.Fatalf("function name = %q, want COALESCE", fn.FuncName)
	}
	qov, ok := fn.Args[0].(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected arg[0] to be *QuantifiedObjectValue, got %T", fn.Args[0])
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}

func TestRebaseValue_RecordConstructor(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &RecordConstructorValue{
		Fields: []RecordConstructorField{
			{Name: "a", Value: &QuantifiedObjectValue{Correlation: oldAlias}},
			{Name: "b", Value: &ConstantValue{Value: 42}},
		},
	}
	result := RebaseValue(v, AliasMap{oldAlias: newAlias})
	rc, ok := result.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected *RecordConstructorValue, got %T", result)
	}
	if len(rc.Fields) != 2 {
		t.Fatalf("fields count = %d, want 2", len(rc.Fields))
	}
	qov, ok := rc.Fields[0].Value.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected field[0].Value to be *QuantifiedObjectValue, got %T", rc.Fields[0].Value)
	}
	if qov.Correlation != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation)
	}
}
