package values

import "testing"

// ---------------------------------------------------------------------------
// EqualsWithoutChildren
// ---------------------------------------------------------------------------

func TestEqualsWithoutChildren_SamePointer(t *testing.T) {
	t.Parallel()
	v := &FieldValue{Field: "x", Typ: NullableLong}
	if !EqualsWithoutChildren(v, v) {
		t.Fatal("same pointer should return true")
	}
}

func TestEqualsWithoutChildren_BothNil(t *testing.T) {
	t.Parallel()
	// nil == nil for interface values in Go, so the a == b check returns true.
	if !EqualsWithoutChildren(nil, nil) {
		t.Fatal("both nil should return true (nil == nil short-circuits via a == b)")
	}
}

func TestEqualsWithoutChildren_OneNil(t *testing.T) {
	t.Parallel()
	v := &FieldValue{Field: "x", Typ: NullableLong}
	if EqualsWithoutChildren(v, nil) {
		t.Fatal("(non-nil, nil) should return false")
	}
	if EqualsWithoutChildren(nil, v) {
		t.Fatal("(nil, non-nil) should return false")
	}
}

func TestEqualsWithoutChildren_FieldValue_Same(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "COL1", Typ: NullableLong}
	b := &FieldValue{Field: "COL1", Typ: NullableString} // Typ differs — irrelevant
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same field name should be true regardless of Typ")
	}
}

func TestEqualsWithoutChildren_FieldValue_Different(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "COL1", Typ: NullableLong}
	b := &FieldValue{Field: "COL2", Typ: NullableLong}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different field names should return false")
	}
}

func TestEqualsWithoutChildren_ConstantValue_SameInt(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: int64(42), Typ: NullableLong}
	b := &ConstantValue{Value: int64(42), Typ: NullableLong}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same int64 constants should be equal")
	}
}

func TestEqualsWithoutChildren_ConstantValue_DifferentInt(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: int64(1), Typ: NullableLong}
	b := &ConstantValue{Value: int64(2), Typ: NullableLong}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different int64 constants should not be equal")
	}
}

func TestEqualsWithoutChildren_ConstantValue_SameString(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: "hello", Typ: NullableString}
	b := &ConstantValue{Value: "hello", Typ: NullableString}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same string constants should be equal")
	}
}

func TestEqualsWithoutChildren_ConstantValue_DifferentString(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: "hello", Typ: NullableString}
	b := &ConstantValue{Value: "world", Typ: NullableString}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different string constants should not be equal")
	}
}

func TestEqualsWithoutChildren_ConstantValue_BothNilValue(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: nil, Typ: NullableLong}
	b := &ConstantValue{Value: nil, Typ: NullableString}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("both nil Value in ConstantValue should be equal via constantValuesEqual")
	}
}

func TestEqualsWithoutChildren_ConstantValue_OneNilValue(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: nil, Typ: NullableLong}
	b := &ConstantValue{Value: int64(0), Typ: NullableLong}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("one nil Value should not be equal to non-nil")
	}
}

func TestEqualsWithoutChildren_ConstantValue_ByteSlice(t *testing.T) {
	t.Parallel()
	a := &ConstantValue{Value: []byte{1, 2, 3}, Typ: UnknownType}
	b := &ConstantValue{Value: []byte{1, 2, 3}, Typ: UnknownType}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same byte slices should be equal")
	}
	c := &ConstantValue{Value: []byte{1, 2, 4}, Typ: UnknownType}
	if EqualsWithoutChildren(a, c) {
		t.Fatal("different byte slices should not be equal")
	}
}

func TestEqualsWithoutChildren_FieldVsConstant(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "x", Typ: NullableLong}
	b := &ConstantValue{Value: int64(1), Typ: NullableLong}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("FieldValue vs ConstantValue should return false")
	}
}

func TestEqualsWithoutChildren_NullValue_Same(t *testing.T) {
	t.Parallel()
	a := &NullValue{Typ: NullableLong}
	b := &NullValue{Typ: NullableString}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("two NullValues should be equal regardless of Typ")
	}
}

func TestEqualsWithoutChildren_NullVsConstant(t *testing.T) {
	t.Parallel()
	a := &NullValue{Typ: NullableLong}
	b := &ConstantValue{Value: nil, Typ: NullableLong}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("NullValue vs ConstantValue should return false")
	}
}

func TestEqualsWithoutChildren_BooleanValue_SameTrue(t *testing.T) {
	t.Parallel()
	a := NewBooleanValue(true)
	b := NewBooleanValue(true)
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("both true should be equal")
	}
}

func TestEqualsWithoutChildren_BooleanValue_SameFalse(t *testing.T) {
	t.Parallel()
	a := NewBooleanValue(false)
	b := NewBooleanValue(false)
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("both false should be equal")
	}
}

func TestEqualsWithoutChildren_BooleanValue_Different(t *testing.T) {
	t.Parallel()
	a := NewBooleanValue(true)
	b := NewBooleanValue(false)
	if EqualsWithoutChildren(a, b) {
		t.Fatal("true vs false should not be equal")
	}
}

func TestEqualsWithoutChildren_BooleanValue_BothNilPtr(t *testing.T) {
	t.Parallel()
	a := &BooleanValue{Value: nil}
	b := &BooleanValue{Value: nil}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("both nil-ptr BooleanValues should be equal")
	}
}

func TestEqualsWithoutChildren_BooleanValue_OneNilPtr(t *testing.T) {
	t.Parallel()
	a := &BooleanValue{Value: nil}
	b := NewBooleanValue(true)
	if EqualsWithoutChildren(a, b) {
		t.Fatal("nil-ptr vs non-nil BooleanValue should not be equal")
	}
}

func TestEqualsWithoutChildren_BooleanVsConstant(t *testing.T) {
	t.Parallel()
	a := NewBooleanValue(true)
	b := &ConstantValue{Value: true, Typ: NullableBoolean}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("BooleanValue vs ConstantValue should return false")
	}
}

func TestEqualsWithoutChildren_QuantifiedObjectValue_Same(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("q1")
	a := &QuantifiedObjectValue{Correlation: corr, Typ: UnknownType}
	b := &QuantifiedObjectValue{Correlation: corr, Typ: UnknownType}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same correlation should be equal")
	}
}

func TestEqualsWithoutChildren_QuantifiedObjectValue_Different(t *testing.T) {
	t.Parallel()
	a := &QuantifiedObjectValue{Correlation: NamedCorrelationIdentifier("q1"), Typ: UnknownType}
	b := &QuantifiedObjectValue{Correlation: NamedCorrelationIdentifier("q2"), Typ: UnknownType}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different correlations should not be equal")
	}
}

func TestEqualsWithoutChildren_ArithmeticValue_SameOp(t *testing.T) {
	t.Parallel()
	// Children differ — EqualsWithoutChildren should ignore them.
	a := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	b := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(99), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(100), Typ: NullableLong},
	}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same ArithmeticOp should be equal regardless of children")
	}
}

func TestEqualsWithoutChildren_ArithmeticValue_DifferentOp(t *testing.T) {
	t.Parallel()
	a := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	b := &ArithmeticValue{
		Op:    OpSub,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different ArithmeticOps should not be equal")
	}
}

func TestEqualsWithoutChildren_NotValue(t *testing.T) {
	t.Parallel()
	a := &NotValue{Child: NewBooleanValue(true)}
	b := &NotValue{Child: NewBooleanValue(false)}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("two NotValues should be equal regardless of child")
	}
}

func TestEqualsWithoutChildren_AndOrValue_SameOp(t *testing.T) {
	t.Parallel()
	a := NewAndOrValue(AndOrAnd, NewBooleanValue(true), NewBooleanValue(false))
	b := NewAndOrValue(AndOrAnd, NewBooleanValue(false), NewBooleanValue(true))
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same AndOrOp should be equal regardless of children")
	}
}

func TestEqualsWithoutChildren_AndOrValue_DifferentOp(t *testing.T) {
	t.Parallel()
	a := NewAndOrValue(AndOrAnd, NewBooleanValue(true), NewBooleanValue(false))
	b := NewAndOrValue(AndOrOr, NewBooleanValue(true), NewBooleanValue(false))
	if EqualsWithoutChildren(a, b) {
		t.Fatal("AND vs OR should not be equal")
	}
}

func TestEqualsWithoutChildren_ParameterValue_Same(t *testing.T) {
	t.Parallel()
	a := NewParameterValue(1)
	b := NewParameterValue(1)
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same ordinal should be equal")
	}
}

func TestEqualsWithoutChildren_ParameterValue_DifferentOrdinal(t *testing.T) {
	t.Parallel()
	a := NewParameterValue(1)
	b := NewParameterValue(2)
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different ordinals should not be equal")
	}
}

func TestEqualsWithoutChildren_ParameterValue_Named(t *testing.T) {
	t.Parallel()
	a := NewNamedParameterValue("foo")
	b := NewNamedParameterValue("foo")
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same named parameter should be equal")
	}
	c := NewNamedParameterValue("bar")
	if EqualsWithoutChildren(a, c) {
		t.Fatal("different named parameters should not be equal")
	}
}

func TestEqualsWithoutChildren_CastValue_SameTarget(t *testing.T) {
	t.Parallel()
	a := &CastValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, Target: NullableString}
	b := &CastValue{Child: &ConstantValue{Value: int64(99), Typ: NullableLong}, Target: NullableString}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same target type should be equal regardless of child")
	}
}

func TestEqualsWithoutChildren_CastValue_DifferentTarget(t *testing.T) {
	t.Parallel()
	a := &CastValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, Target: NullableString}
	b := &CastValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, Target: NullableDouble}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different target types should not be equal")
	}
}

func TestEqualsWithoutChildren_RecordConstructorValue_SameFieldNames(t *testing.T) {
	t.Parallel()
	a := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "A", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
		{Name: "B", Value: &ConstantValue{Value: int64(2), Typ: NullableLong}},
	}}
	b := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "A", Value: &ConstantValue{Value: int64(99), Typ: NullableLong}},
		{Name: "B", Value: &ConstantValue{Value: int64(100), Typ: NullableLong}},
	}}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same field names should be equal regardless of child values")
	}
}

func TestEqualsWithoutChildren_RecordConstructorValue_DifferentFieldNames(t *testing.T) {
	t.Parallel()
	a := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "A", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
	}}
	b := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "Z", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
	}}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different field names should not be equal")
	}
}

func TestEqualsWithoutChildren_RecordConstructorValue_DifferentFieldCount(t *testing.T) {
	t.Parallel()
	a := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "A", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
	}}
	b := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "A", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
		{Name: "B", Value: &ConstantValue{Value: int64(2), Typ: NullableLong}},
	}}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different field counts should not be equal")
	}
}

func TestEqualsWithoutChildren_ScalarFunctionValue_Same(t *testing.T) {
	t.Parallel()
	a := NewScalarFunctionValue("UPPER", NullableString, &FieldValue{Field: "x", Typ: NullableString})
	b := NewScalarFunctionValue("UPPER", NullableString, &FieldValue{Field: "y", Typ: NullableString})
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same func name and arg count should be equal regardless of args")
	}
}

func TestEqualsWithoutChildren_ScalarFunctionValue_DifferentName(t *testing.T) {
	t.Parallel()
	a := NewScalarFunctionValue("UPPER", NullableString, &FieldValue{Field: "x", Typ: NullableString})
	b := NewScalarFunctionValue("LOWER", NullableString, &FieldValue{Field: "x", Typ: NullableString})
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different func names should not be equal")
	}
}

func TestEqualsWithoutChildren_ScalarFunctionValue_DifferentArgCount(t *testing.T) {
	t.Parallel()
	a := NewScalarFunctionValue("COALESCE", NullableLong,
		&FieldValue{Field: "x", Typ: NullableLong},
	)
	b := NewScalarFunctionValue("COALESCE", NullableLong,
		&FieldValue{Field: "x", Typ: NullableLong},
		&ConstantValue{Value: int64(0), Typ: NullableLong},
	)
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different arg counts should not be equal")
	}
}

func TestEqualsWithoutChildren_AggregateValue_SameOp(t *testing.T) {
	t.Parallel()
	a := &AggregateValue{Op: AggSum, Operand: &FieldValue{Field: "x", Typ: NullableLong}}
	b := &AggregateValue{Op: AggSum, Operand: &FieldValue{Field: "y", Typ: NullableLong}}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same AggOp should be equal regardless of operand")
	}
}

func TestEqualsWithoutChildren_AggregateValue_DifferentOp(t *testing.T) {
	t.Parallel()
	a := &AggregateValue{Op: AggSum, Operand: &FieldValue{Field: "x", Typ: NullableLong}}
	b := &AggregateValue{Op: AggCount, Operand: &FieldValue{Field: "x", Typ: NullableLong}}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different AggOps should not be equal")
	}
}

func TestEqualsWithoutChildren_QuantifiedRecordValue_Same(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("r1")
	a := &QuantifiedRecordValue{Alias: alias}
	b := &QuantifiedRecordValue{Alias: alias}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same alias should be equal")
	}
}

func TestEqualsWithoutChildren_QuantifiedRecordValue_Different(t *testing.T) {
	t.Parallel()
	a := &QuantifiedRecordValue{Alias: NamedCorrelationIdentifier("r1")}
	b := &QuantifiedRecordValue{Alias: NamedCorrelationIdentifier("r2")}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different aliases should not be equal")
	}
}

func TestEqualsWithoutChildren_ObjectValue_Same(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("o1")
	a := &ObjectValue{Alias: alias}
	b := &ObjectValue{Alias: alias}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same alias should be equal")
	}
}

func TestEqualsWithoutChildren_ObjectValue_Different(t *testing.T) {
	t.Parallel()
	a := &ObjectValue{Alias: NamedCorrelationIdentifier("o1")}
	b := &ObjectValue{Alias: NamedCorrelationIdentifier("o2")}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different aliases should not be equal")
	}
}

func TestEqualsWithoutChildren_PromoteValue_SameTarget(t *testing.T) {
	t.Parallel()
	a := &PromoteValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, Target: NullableDouble}
	b := &PromoteValue{Child: &ConstantValue{Value: int64(99), Typ: NullableLong}, Target: NullableDouble}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same target type should be equal")
	}
}

func TestEqualsWithoutChildren_PromoteValue_DifferentTarget(t *testing.T) {
	t.Parallel()
	a := &PromoteValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, Target: NullableDouble}
	b := &PromoteValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, Target: NullableString}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different target types should not be equal")
	}
}

func TestEqualsWithoutChildren_CrossTypeMismatch(t *testing.T) {
	t.Parallel()
	// ArithmeticValue vs CastValue — different types entirely.
	a := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	b := &CastValue{
		Child:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Target: NullableString,
	}
	if EqualsWithoutChildren(a, b) {
		t.Fatal("different concrete types should not be equal")
	}
}

func TestEqualsWithoutChildren_FallbackSameConcreteType(t *testing.T) {
	t.Parallel()
	// OfTypeValue is not in the explicit switch — it falls through to the
	// reflect.TypeOf fallback. Two OfTypeValues with same concrete type
	// should match via the fallback.
	a := &OfTypeValue{Child: &ConstantValue{Value: int64(1), Typ: NullableLong}, ExpectedType: NullableLong}
	b := &OfTypeValue{Child: &ConstantValue{Value: int64(2), Typ: NullableLong}, ExpectedType: NullableString}
	if !EqualsWithoutChildren(a, b) {
		t.Fatal("same concrete type via reflect fallback should be equal")
	}
}

// ---------------------------------------------------------------------------
// WithChildren
// ---------------------------------------------------------------------------

func TestWithChildren_LeafConstant_EmptySlice(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: int64(42), Typ: NullableLong}
	result := WithChildren(v, []Value{})
	// ConstantValue has no case in withChildren — falls to default, returns v.
	if result != v {
		t.Fatal("leaf value with empty children should return same value")
	}
}

func TestWithChildren_LeafField_EmptySlice(t *testing.T) {
	t.Parallel()
	v := &FieldValue{Field: "x", Typ: NullableLong}
	result := WithChildren(v, []Value{})
	if result != v {
		t.Fatal("leaf value with empty children should return same value")
	}
}

func TestWithChildren_NilValue(t *testing.T) {
	t.Parallel()
	result := WithChildren(nil, nil)
	if result != nil {
		t.Fatalf("nil value should return nil, got %v", result)
	}
}

func TestWithChildren_ArithmeticValue_TwoChildren(t *testing.T) {
	t.Parallel()
	original := &ArithmeticValue{
		Op:    OpMul,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	newLeft := &ConstantValue{Value: int64(10), Typ: NullableLong}
	newRight := &ConstantValue{Value: int64(20), Typ: NullableLong}

	result := WithChildren(original, []Value{newLeft, newRight})

	a, ok := result.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", result)
	}
	if a.Op != OpMul {
		t.Fatalf("op should be preserved, got %v", a.Op)
	}
	if a.Left != newLeft {
		t.Fatal("left should be the new child")
	}
	if a.Right != newRight {
		t.Fatal("right should be the new child")
	}
}

func TestWithChildren_ArithmeticValue_WrongChildCount(t *testing.T) {
	t.Parallel()
	original := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	// Pass 1 child instead of 2 — should return original unchanged.
	result := WithChildren(original, []Value{&ConstantValue{Value: int64(10), Typ: NullableLong}})
	if result != original {
		t.Fatal("wrong child count should return original value")
	}
}

func TestWithChildren_RecordConstructorValue(t *testing.T) {
	t.Parallel()
	original := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "X", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
		{Name: "Y", Value: &ConstantValue{Value: int64(2), Typ: NullableLong}},
	}}

	newX := &FieldValue{Field: "A", Typ: NullableString}
	newY := &FieldValue{Field: "B", Typ: NullableString}

	result := WithChildren(original, []Value{newX, newY})

	r, ok := result.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected *RecordConstructorValue, got %T", result)
	}
	if len(r.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(r.Fields))
	}
	if r.Fields[0].Name != "X" || r.Fields[1].Name != "Y" {
		t.Fatal("field names should be preserved")
	}
	if r.Fields[0].Value != newX {
		t.Fatal("first field value should be the new child")
	}
	if r.Fields[1].Value != newY {
		t.Fatal("second field value should be the new child")
	}
}

func TestWithChildren_RecordConstructorValue_WrongCount(t *testing.T) {
	t.Parallel()
	original := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "X", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
	}}
	// Pass 2 children for 1 field — should return original.
	result := WithChildren(original, []Value{
		&ConstantValue{Value: int64(10), Typ: NullableLong},
		&ConstantValue{Value: int64(20), Typ: NullableLong},
	})
	if result != original {
		t.Fatal("wrong child count should return original value")
	}
}

func TestWithChildren_CastValue(t *testing.T) {
	t.Parallel()
	original := &CastValue{
		Child:  &ConstantValue{Value: int64(5), Typ: NullableLong},
		Target: NullableString,
	}
	newChild := &FieldValue{Field: "col", Typ: NullableLong}
	result := WithChildren(original, []Value{newChild})

	c, ok := result.(*CastValue)
	if !ok {
		t.Fatalf("expected *CastValue, got %T", result)
	}
	if c.Child != newChild {
		t.Fatal("child should be replaced")
	}
	if c.Target != NullableString {
		t.Fatal("target should be preserved")
	}
}

func TestWithChildren_NotValue(t *testing.T) {
	t.Parallel()
	original := &NotValue{Child: NewBooleanValue(true)}
	newChild := NewBooleanValue(false)
	result := WithChildren(original, []Value{newChild})

	n, ok := result.(*NotValue)
	if !ok {
		t.Fatalf("expected *NotValue, got %T", result)
	}
	if n.Child != newChild {
		t.Fatal("child should be replaced")
	}
}

func TestWithChildren_ScalarFunctionValue(t *testing.T) {
	t.Parallel()
	original := NewScalarFunctionValue("UPPER", NullableString,
		&FieldValue{Field: "name", Typ: NullableString},
	)
	newArg := &FieldValue{Field: "title", Typ: NullableString}
	result := WithChildren(original, []Value{newArg})

	s, ok := result.(*ScalarFunctionValue)
	if !ok {
		t.Fatalf("expected *ScalarFunctionValue, got %T", result)
	}
	if s.FuncName != "UPPER" {
		t.Fatal("func name should be preserved")
	}
	if len(s.Args) != 1 || s.Args[0] != newArg {
		t.Fatal("args should be the new children")
	}
}

func TestWithChildren_AggregateValue_Sum(t *testing.T) {
	t.Parallel()
	original := &AggregateValue{Op: AggSum, Operand: &FieldValue{Field: "x", Typ: NullableLong}}
	newOperand := &FieldValue{Field: "y", Typ: NullableLong}
	result := WithChildren(original, []Value{newOperand})

	a, ok := result.(*AggregateValue)
	if !ok {
		t.Fatalf("expected *AggregateValue, got %T", result)
	}
	if a.Op != AggSum {
		t.Fatal("op should be preserved")
	}
	if a.Operand != newOperand {
		t.Fatal("operand should be replaced")
	}
}

func TestWithChildren_AggregateCountStar_Unchanged(t *testing.T) {
	t.Parallel()
	original := &AggregateValue{Op: AggCountStar}
	result := WithChildren(original, []Value{})
	if result != original {
		t.Fatal("COUNT(*) should return original regardless of children")
	}
}

func TestWithChildren_PromoteValue(t *testing.T) {
	t.Parallel()
	original := &PromoteValue{
		Child:  &ConstantValue{Value: int64(5), Typ: NullableLong},
		Target: NullableDouble,
	}
	newChild := &FieldValue{Field: "amount", Typ: NullableLong}
	result := WithChildren(original, []Value{newChild})

	p, ok := result.(*PromoteValue)
	if !ok {
		t.Fatalf("expected *PromoteValue, got %T", result)
	}
	if p.Child != newChild {
		t.Fatal("child should be replaced")
	}
	if p.Target != NullableDouble {
		t.Fatal("target should be preserved")
	}
}

func TestWithChildren_AndOrValue(t *testing.T) {
	t.Parallel()
	original := NewAndOrValue(AndOrOr, NewBooleanValue(true), NewBooleanValue(false))
	newLeft := NewBooleanValue(false)
	newRight := NewBooleanValue(true)
	result := WithChildren(original, []Value{newLeft, newRight})

	a, ok := result.(*AndOrValue)
	if !ok {
		t.Fatalf("expected *AndOrValue, got %T", result)
	}
	if a.Op != AndOrOr {
		t.Fatal("op should be preserved")
	}
	if a.Left != newLeft {
		t.Fatal("left should be replaced")
	}
	if a.Right != newRight {
		t.Fatal("right should be replaced")
	}
}
