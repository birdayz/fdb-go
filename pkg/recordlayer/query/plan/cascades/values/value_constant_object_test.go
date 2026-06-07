package values

import "testing"

// stubConstantDeref implements the ConstantDeref capability for
// testing.
type stubConstantDeref struct {
	values map[constantKey]any
}

type constantKey struct {
	alias      CorrelationIdentifier
	constantID string
}

func (s *stubConstantDeref) DereferenceConstant(alias CorrelationIdentifier, constantID string) any {
	return s.values[constantKey{alias: alias, constantID: constantID}]
}

func TestConstantObjectValue_LeafShape(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NotNullLong)
	if len(v.Children()) != 0 {
		t.Fatal("ConstantObjectValue should be a leaf")
	}
	if v.Alias.Name() != "a" {
		t.Fatalf("Alias=%q", v.Alias.Name())
	}
	if v.ConstantID != "c1" {
		t.Fatalf("ConstantID=%q", v.ConstantID)
	}
}

func TestConstantObjectValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NullableString)
	if !v.Type().Equals(NullableString) {
		t.Fatalf("Type=%v, want NullableString", v.Type())
	}
}

func TestConstantObjectValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}

func TestConstantObjectValue_EvaluateNoDereferReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NotNullLong)
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate without ConstantDeref = %v, want nil", got)
	}
	if got := mustEvalForTest(v, "not a deref"); got != nil {
		t.Fatalf("Evaluate with non-ConstantDeref = %v, want nil", got)
	}
}

func TestConstantObjectValue_EvaluateLooksUpBinding(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NotNullLong)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int64(42),
	}}
	if got := mustEvalForTest(v, stub); got != int64(42) {
		t.Fatalf("Evaluate = %v, want int64(42)", got)
	}
}

func TestConstantObjectValue_EvaluateMissingBinding(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NotNullLong)
	stub := &stubConstantDeref{values: map[constantKey]any{}}
	if got := mustEvalForTest(v, stub); got != nil {
		t.Fatalf("Evaluate missing binding = %v, want nil", got)
	}
}

func TestConstantObjectValue_CorrelatedToAlias(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NotNullLong)
	cs := v.GetCorrelatedTo()
	if len(cs) != 1 {
		t.Fatalf("CorrelatedTo size = %d, want 1", len(cs))
	}
	if _, ok := cs[alias]; !ok {
		t.Fatalf("CorrelatedTo missing alias %v", alias)
	}
}

// --- Type promotion tests (D-11) ---

func TestConstantObjectValue_PromoteInt32ToLong(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableLong)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int32(7),
	}}
	got := mustEvalForTest(v, stub)
	if got != int64(7) {
		t.Fatalf("Evaluate = %v (%T), want int64(7)", got, got)
	}
}

func TestConstantObjectValue_PromoteInt32ToDouble(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableDouble)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int32(3),
	}}
	got := mustEvalForTest(v, stub)
	if got != float64(3) {
		t.Fatalf("Evaluate = %v (%T), want float64(3)", got, got)
	}
}

func TestConstantObjectValue_PromoteInt64ToDouble(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableDouble)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int64(42),
	}}
	got := mustEvalForTest(v, stub)
	if got != float64(42) {
		t.Fatalf("Evaluate = %v (%T), want float64(42)", got, got)
	}
}

func TestConstantObjectValue_PromoteFloat32ToDouble(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableDouble)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: float32(1.5),
	}}
	got := mustEvalForTest(v, stub)
	if got != float64(float32(1.5)) {
		t.Fatalf("Evaluate = %v (%T), want float64(1.5)", got, got)
	}
}

func TestConstantObjectValue_Int64ToLongNoPromotion(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableLong)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int64(99),
	}}
	got := mustEvalForTest(v, stub)
	if got != int64(99) {
		t.Fatalf("Evaluate = %v (%T), want int64(99)", got, got)
	}
}

func TestConstantObjectValue_StringNoPromotion(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableString)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: "hello",
	}}
	got := mustEvalForTest(v, stub)
	if got != "hello" {
		t.Fatalf("Evaluate = %v (%T), want \"hello\"", got, got)
	}
}

func TestConstantObjectValue_PromoteNilReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableLong)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: nil,
	}}
	got := mustEvalForTest(v, stub)
	if got != nil {
		t.Fatalf("Evaluate = %v, want nil", got)
	}
}

func TestConstantObjectValue_PromoteNoDerefReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NullableLong)
	got := mustEvalForTest(v, "not a deref")
	if got != nil {
		t.Fatalf("Evaluate = %v, want nil", got)
	}
}

func TestConstantObjectValue_PromoteInt32ToFloat(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableFloat)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int32(5),
	}}
	got := mustEvalForTest(v, stub)
	if got != float32(5) {
		t.Fatalf("Evaluate = %v (%T), want float32(5)", got, got)
	}
}

func TestConstantObjectValue_PromoteInt64ToFloat(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableFloat)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int64(10),
	}}
	got := mustEvalForTest(v, stub)
	if got != float32(10) {
		t.Fatalf("Evaluate = %v (%T), want float32(10)", got, got)
	}
}

func TestConstantObjectValue_PromoteGoIntToInt64(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NullableLong)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int(42),
	}}
	got := mustEvalForTest(v, stub)
	i64, ok := got.(int64)
	if !ok {
		t.Fatalf("Evaluate = %v (%T), want int64 (not bare int)", got, got)
	}
	if i64 != 42 {
		t.Fatalf("Evaluate = %d, want 42", i64)
	}
}

func TestConstantObjectValue_RelationTypePassThrough(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	relType := NewRelationType(NullableLong)
	v := NewConstantObjectValue(alias, "c1", relType)
	sentinel := &struct{ data string }{data: "stream"}
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: sentinel,
	}}
	got := mustEvalForTest(v, stub)
	if got != sentinel {
		t.Fatalf("Evaluate = %v, want sentinel (relation pass-through)", got)
	}
}
