package values

import (
	"testing"
)

func TestPullUpValue_ExactMatch(t *testing.T) {
	t.Parallel()
	// v equals resultValue → QuantifiedObjectValue(alias)
	alias := NamedCorrelationIdentifier("q1")
	v := &FieldValue{Field: "x", Typ: NullableString}
	result := &FieldValue{Field: "x", Typ: NullableString}

	pulled := PullUpValue(v, result, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result")
	}
	qov, ok := pulled.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QuantifiedObjectValue, got %T", pulled)
	}
	if qov.Correlation != alias {
		t.Fatalf("expected alias %v, got %v", alias, qov.Correlation)
	}
}

func TestPullUpValue_ThroughRecordConstructor(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	// PullUp FV("x") → FV("a")
	pulled := PullUpValue(&FieldValue{Field: "x", Typ: NullableLong}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for FV(x)")
	}
	fv, ok := pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "a" {
		t.Fatalf("expected field 'a', got %q", fv.Field)
	}

	// PullUp FV("y") → FV("b")
	pulled = PullUpValue(&FieldValue{Field: "y", Typ: NullableString}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for FV(y)")
	}
	fv, ok = pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "b" {
		t.Fatalf("expected field 'b', got %q", fv.Field)
	}
}

func TestPullUpValue_ThroughRecordConstructor_NotFound(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
	)

	// FV("z") is not in the constructor.
	pulled := PullUpValue(&FieldValue{Field: "z", Typ: NullableLong}, resultValue, alias)
	if pulled != nil {
		t.Fatalf("expected nil for unmapped field, got %v", pulled)
	}
}

func TestPullUpValue_ThroughRecordConstructor_ArithmeticChild(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(sum = (x + y))
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: NullableLong},
		Right: &FieldValue{Field: "y", Typ: NullableLong},
	}
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "sum", Value: arith},
	)

	// PullUp (x + y) → FV("sum")
	vArith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: NullableLong},
		Right: &FieldValue{Field: "y", Typ: NullableLong},
	}
	pulled := PullUpValue(vArith, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for arithmetic match")
	}
	fv, ok := pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "sum" {
		t.Fatalf("expected field 'sum', got %q", fv.Field)
	}
}

func TestPullUpValue_ThroughQOV(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q_out")
	innerAlias := NamedCorrelationIdentifier("q_in")

	// resultValue = QOV(q_in) — passthrough
	resultValue := &QuantifiedObjectValue{Correlation: innerAlias, Typ: UnknownType}

	// PullUp FV("col") through passthrough → FV("col")
	pulled := PullUpValue(&FieldValue{Field: "col", Typ: NullableLong}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result")
	}
	fv, ok := pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "col" {
		t.Fatalf("expected field 'col', got %q", fv.Field)
	}
}

func TestPullUpValue_Nil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")
	if PullUpValue(nil, &FieldValue{Field: "x"}, alias) != nil {
		t.Fatal("expected nil for nil v")
	}
	if PullUpValue(&FieldValue{Field: "x"}, nil, alias) != nil {
		t.Fatal("expected nil for nil resultValue")
	}
}

func TestPushDownValue_ThroughRecordConstructor(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	// PushDown FV("a") → FV("x")
	pushed := PushDownValue(&FieldValue{Field: "a", Typ: NullableLong}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result for FV(a)")
	}
	fv, ok := pushed.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "x" {
		t.Fatalf("expected field 'x', got %q", fv.Field)
	}

	// PushDown FV("b") → FV("y")
	pushed = PushDownValue(&FieldValue{Field: "b", Typ: NullableString}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result for FV(b)")
	}
	fv, ok = pushed.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "y" {
		t.Fatalf("expected field 'y', got %q", fv.Field)
	}
}

func TestPushDownValue_QOVReplacedByResultValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
	)

	// PushDown QOV(q1) → resultValue itself
	v := &QuantifiedObjectValue{Correlation: alias, Typ: UnknownType}
	pushed := PushDownValue(v, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result")
	}
	rc, ok := pushed.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue, got %T", pushed)
	}
	if len(rc.Fields) != 1 || rc.Fields[0].Name != "a" {
		t.Fatalf("expected 1-field record constructor with field 'a', got %v", rc.Fields)
	}
}

func TestPushDownValue_ThroughRecordConstructor_NotFound(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
	)

	// FV("z") not in constructor.
	pushed := PushDownValue(&FieldValue{Field: "z"}, resultValue, alias)
	if pushed != nil {
		t.Fatalf("expected nil for unmapped field, got %v", pushed)
	}
}

func TestPushDownValue_ThroughQOV(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q_out")
	innerAlias := NamedCorrelationIdentifier("q_in")

	// Passthrough result
	resultValue := &QuantifiedObjectValue{Correlation: innerAlias, Typ: UnknownType}

	// PushDown FV("col") through passthrough → FV("col")
	pushed := PushDownValue(&FieldValue{Field: "col", Typ: NullableLong}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result")
	}
	fv, ok := pushed.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "col" {
		t.Fatalf("expected field 'col', got %q", fv.Field)
	}
}

func TestPushDownValue_Nil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")
	if PushDownValue(nil, &FieldValue{Field: "x"}, alias) != nil {
		t.Fatal("expected nil for nil v")
	}
	if PushDownValue(&FieldValue{Field: "x"}, nil, alias) != nil {
		t.Fatal("expected nil for nil resultValue")
	}
}

func TestPullUpPushDown_RoundTrip(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	original := &FieldValue{Field: "x", Typ: NullableLong}

	// PullUp: FV("x") → FV("a")
	pulled := PullUpValue(original, resultValue, alias)
	if pulled == nil {
		t.Fatal("pullUp failed")
	}

	// PushDown: FV("a") → FV("x")
	pushed := PushDownValue(pulled, resultValue, alias)
	if pushed == nil {
		t.Fatal("pushDown failed")
	}

	if ExplainValue(pushed) != ExplainValue(original) {
		t.Fatalf("round-trip failed: got %q, want %q",
			ExplainValue(pushed), ExplainValue(original))
	}
}

func TestPullUpValues_Batch(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	vs := []Value{
		&FieldValue{Field: "x", Typ: NullableLong},
		&FieldValue{Field: "y", Typ: NullableString},
		&FieldValue{Field: "z", Typ: NullableLong}, // not in constructor
	}

	result := PullUpValues(vs, resultValue, alias)
	if len(result) != 2 {
		t.Fatalf("expected 2 mapped values, got %d", len(result))
	}
}

func TestPushDownValues_Batch(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	vs := []Value{
		&FieldValue{Field: "a", Typ: NullableLong},
		&FieldValue{Field: "b", Typ: NullableString},
		&FieldValue{Field: "z", Typ: NullableLong}, // not in constructor
	}

	result := PushDownValues(vs, resultValue, alias)
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	if result[0] == nil || ExplainValue(result[0]) != "x" {
		t.Fatalf("expected FV(x), got %v", result[0])
	}
	if result[1] == nil || ExplainValue(result[1]) != "y" {
		t.Fatalf("expected FV(y), got %v", result[1])
	}
	if result[2] != nil {
		t.Fatalf("expected nil for unmapped field, got %v", result[2])
	}
}

func TestSemanticEqual(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "x", Typ: NullableLong}
	b := &FieldValue{Field: "x", Typ: NullableLong}
	c := &FieldValue{Field: "y", Typ: NullableLong}

	if !semanticEqual(a, b) {
		t.Fatal("expected a == b")
	}
	if semanticEqual(a, c) {
		t.Fatal("expected a != c")
	}
	if semanticEqual(nil, a) {
		t.Fatal("expected nil != a")
	}
	if semanticEqual(a, nil) {
		t.Fatal("expected a != nil")
	}
}
