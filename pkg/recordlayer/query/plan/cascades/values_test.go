package cascades

import "testing"

// Static interface assertions.
var (
	_ Value = (*ConstantValue)(nil)
	_ Value = (*FieldValue)(nil)
	_ Value = (*ArithmeticValue)(nil)
	_ Value = (*BooleanValue)(nil)
	_ Value = (*CastValue)(nil)
)

func TestConstantValue_Evaluate(t *testing.T) {
	t.Parallel()
	c := &ConstantValue{Value: int64(42), Typ: TypeInt}
	if got := c.Evaluate(nil); got != int64(42) {
		t.Fatalf("constant int: got %v", got)
	}
	// Context is ignored for constants.
	if got := c.Evaluate(map[string]any{"x": 1}); got != int64(42) {
		t.Fatalf("constant ignores ctx: got %v", got)
	}
	// NULL literal.
	null := &ConstantValue{Value: nil, Typ: TypeInt}
	if got := null.Evaluate(nil); got != nil {
		t.Fatalf("NULL literal: got %v", got)
	}
}

func TestFieldValue_Evaluate(t *testing.T) {
	t.Parallel()
	f := &FieldValue{Field: "name", Typ: TypeString}
	row := map[string]any{"name": "Alice", "age": int64(30)}
	if got := f.Evaluate(row); got != "Alice" {
		t.Fatalf("field lookup: got %v", got)
	}
	// Missing field: NULL.
	missing := &FieldValue{Field: "nope", Typ: TypeString}
	if got := missing.Evaluate(row); got != nil {
		t.Fatalf("missing field: got %v", got)
	}
	// nil ctx.
	if got := f.Evaluate(nil); got != nil {
		t.Fatalf("nil ctx: got %v", got)
	}
	// Wrong ctx type.
	if got := f.Evaluate("not a map"); got != nil {
		t.Fatalf("wrong ctx type: got %v", got)
	}
}

func TestArithmeticValue_Evaluate(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}

	cases := []struct {
		op   ArithmeticOp
		a, b int64
		want any
	}{
		{OpAdd, 3, 4, int64(7)},
		{OpSub, 10, 3, int64(7)},
		{OpMul, 6, 7, int64(42)},
		{OpDiv, 20, 4, int64(5)},
	}
	for _, tc := range cases {
		av := &ArithmeticValue{Op: tc.op, Left: a, Right: b}
		got := av.Evaluate(map[string]any{"a": tc.a, "b": tc.b})
		if got != tc.want {
			t.Fatalf("op %v: got %v, want %v", tc.op, got, tc.want)
		}
	}

	// Division by zero returns nil (UNKNOWN-at-Value-layer).
	divZ := &ArithmeticValue{Op: OpDiv, Left: a, Right: b}
	if got := divZ.Evaluate(map[string]any{"a": int64(5), "b": int64(0)}); got != nil {
		t.Fatalf("div by zero: got %v", got)
	}

	// NULL propagation.
	sum := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := sum.Evaluate(map[string]any{"a": nil, "b": int64(1)}); got != nil {
		t.Fatalf("NULL lhs: got %v", got)
	}
	if got := sum.Evaluate(map[string]any{"a": int64(1), "b": nil}); got != nil {
		t.Fatalf("NULL rhs: got %v", got)
	}

	// Type mismatch returns nil.
	tm := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := tm.Evaluate(map[string]any{"a": "foo", "b": int64(1)}); got != nil {
		t.Fatalf("type mismatch: got %v", got)
	}
}

func TestBooleanValue(t *testing.T) {
	t.Parallel()
	tv := NewBooleanValue(true)
	if got := tv.Evaluate(nil); got != true {
		t.Fatalf("true literal: got %v", got)
	}
	fv := NewBooleanValue(false)
	if got := fv.Evaluate(nil); got != false {
		t.Fatalf("false literal: got %v", got)
	}
	// UNKNOWN literal.
	uv := &BooleanValue{Value: nil}
	if got := uv.Evaluate(nil); got != nil {
		t.Fatalf("UNKNOWN literal: got %v", got)
	}
	if tv.Type() != TypeBool {
		t.Fatal("BooleanValue.Type should be TypeBool")
	}
}

func TestCastValue(t *testing.T) {
	t.Parallel()
	// int → string
	strC := NewCastValue(&ConstantValue{Value: int64(42), Typ: TypeInt}, TypeString)
	if got := strC.Evaluate(nil); got != "42" {
		t.Fatalf("int→string: got %v", got)
	}

	// bool → int: true=1, false=0.
	boolToInt := NewCastValue(NewBooleanValue(true), TypeInt)
	if got := boolToInt.Evaluate(nil); got != int64(1) {
		t.Fatalf("true→int: got %v", got)
	}
	boolToInt = NewCastValue(NewBooleanValue(false), TypeInt)
	if got := boolToInt.Evaluate(nil); got != int64(0) {
		t.Fatalf("false→int: got %v", got)
	}

	// int → bool: 0=false, non-zero=true.
	intToBool := NewCastValue(&ConstantValue{Value: int64(0), Typ: TypeInt}, TypeBool)
	if got := intToBool.Evaluate(nil); got != false {
		t.Fatalf("0→bool: got %v", got)
	}
	intToBool = NewCastValue(&ConstantValue{Value: int64(7), Typ: TypeInt}, TypeBool)
	if got := intToBool.Evaluate(nil); got != true {
		t.Fatalf("7→bool: got %v", got)
	}

	// NULL propagates.
	nullC := NewCastValue(&ConstantValue{Value: nil, Typ: TypeInt}, TypeString)
	if got := nullC.Evaluate(nil); got != nil {
		t.Fatalf("NULL cast: got %v", got)
	}

	// Unknown conversion: int → bool via the reverse path is OK,
	// but string → int isn't wired in the seed (returns nil).
	strToInt := NewCastValue(&ConstantValue{Value: "3", Typ: TypeString}, TypeInt)
	if got := strToInt.Evaluate(nil); got != nil {
		t.Fatalf("string→int not yet wired: expected nil, got %v", got)
	}

	// Children wiring + Type.
	if len(strC.Children()) != 1 {
		t.Fatalf("cast children: expected 1, got %d", len(strC.Children()))
	}
	if strC.Type() != TypeString {
		t.Fatal("cast.Type should be Target")
	}
}
