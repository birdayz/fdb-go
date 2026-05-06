package values

import "testing"

// Static interface check.
var _ Value = (*NotValue)(nil)

// TestNotValue_Evaluate_TruthTable pins the canonical Kleene 3VL:
// NOT TRUE = FALSE, NOT FALSE = TRUE, NOT NULL = NULL.
func TestNotValue_Evaluate_TruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		child Value
		want  any
	}{
		{"NOT TRUE", NewBooleanValue(true), false},
		{"NOT FALSE", NewBooleanValue(false), true},
		{"NOT NULL (Boolean)", &BooleanValue{Value: nil}, nil},
		{"NOT NULL (NullValue)", NewNullValue(TypeBool), nil},
		{"NOT NULL (ConstantValue nil)", &ConstantValue{Value: nil, Typ: TypeBool}, nil},
		{"NOT TRUE via ConstantValue", &ConstantValue{Value: true, Typ: TypeBool}, false},
		{"NOT FALSE via ConstantValue", &ConstantValue{Value: false, Typ: TypeBool}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewNotValue(tc.child).Evaluate(nil)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNotValue_Evaluate_TypeMismatchDegrades pins the type-degraded
// case: NOT applied to a non-bool value returns nil (UNKNOWN). Per
// the Value-layer convention used by ArithmeticValue, type errors
// surface as nil rather than panic.
func TestNotValue_Evaluate_TypeMismatchDegrades(t *testing.T) {
	t.Parallel()
	cases := []Value{
		&ConstantValue{Value: int64(1), Typ: TypeInt},
		&ConstantValue{Value: "true", Typ: TypeString},
		&ConstantValue{Value: 1.5, Typ: TypeFloat},
	}
	for _, child := range cases {
		got := NewNotValue(child).Evaluate(nil)
		if got != nil {
			t.Fatalf("NotValue on %v: got %v, want nil", child.Name(), got)
		}
	}
}

// TestNotValue_Evaluate_FieldLookup pins integration with the eval
// context — NOT(active) where active is a row column.
func TestNotValue_Evaluate_FieldLookup(t *testing.T) {
	t.Parallel()
	v := NewNotValue(&FieldValue{Field: "active", Typ: TypeBool})
	if got := v.Evaluate(map[string]any{"active": true}); got != false {
		t.Fatalf("active=true: got %v, want false", got)
	}
	if got := v.Evaluate(map[string]any{"active": false}); got != true {
		t.Fatalf("active=false: got %v, want true", got)
	}
	// Missing field → NULL → NULL.
	if got := v.Evaluate(map[string]any{}); got != nil {
		t.Fatalf("missing field: got %v, want nil", got)
	}
}

// TestNotValue_Type pins that NotValue always reports a boolean Type —
// independent of the child's type. NOT is a boolean-typed operator.
func TestNotValue_Type(t *testing.T) {
	t.Parallel()
	v := NewNotValue(&ConstantValue{Value: int64(1), Typ: TypeInt})
	if v.Type().Code() != TypeCodeBoolean {
		t.Fatalf("Type: got %v, want a boolean type", v.Type())
	}
}

// TestNotValue_Children pins Children returning a 1-element slice
// (the wrapped child). Empty when child is nil so the walker is
// nil-safe.
func TestNotValue_Children(t *testing.T) {
	t.Parallel()
	cv := &ConstantValue{Value: true, Typ: TypeBool}
	v := NewNotValue(cv)
	got := v.Children()
	if len(got) != 1 || got[0] != cv {
		t.Fatalf("Children: got %v, want [cv]", got)
	}

	// nil child → empty slice (no nil entry).
	if got := (&NotValue{Child: nil}).Children(); len(got) != 0 {
		t.Fatalf("nil child: got %v, want empty", got)
	}
}

// TestNotValue_WalkValue pins integration with the WalkValue
// pre-order traversal: NOT(Add(field, 1)) visits 4 nodes (Not, Add,
// field, 1) — Children's slice-of-1 is the right shape for the
// walker.
func TestNotValue_WalkValue(t *testing.T) {
	t.Parallel()
	tree := NewNotValue(&ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: TypeInt},
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	})
	visited := 0
	WalkValue(tree, func(Value) bool {
		visited++
		return true
	})
	if visited != 4 {
		t.Fatalf("NOT(Add(field, 1)): walker visited %d nodes, want 4", visited)
	}
}

// TestNotValue_IsConstantValue pins that NotValue(constant) is itself
// considered constant (composite-with-all-constant-children rule),
// while NotValue(field) is not. This is the gate IsFoldableComposite
// + EvaluateConstant rely on for plan-time folding.
func TestNotValue_IsConstantValue(t *testing.T) {
	t.Parallel()
	if !IsConstantValue(NewNotValue(NewBooleanValue(true))) {
		t.Fatal("NOT(constant) should be IsConstantValue")
	}
	if IsConstantValue(NewNotValue(&FieldValue{Field: "x", Typ: TypeBool})) {
		t.Fatal("NOT(field) should NOT be IsConstantValue")
	}
}

// TestNotValue_NilChild_Evaluate pins that a NotValue with no child
// evaluates to nil (UNKNOWN) rather than nil-deref panicking. Defensive
// against rule authors that build NotValue programmatically.
func TestNotValue_NilChild_Evaluate(t *testing.T) {
	t.Parallel()
	v := &NotValue{Child: nil}
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("nil child: got %v, want nil", got)
	}
}

// TestNotValue_Name pins the Name() debug string. Used by Walk +
// Explain for diagnostics.
func TestNotValue_Name(t *testing.T) {
	t.Parallel()
	if got := (&NotValue{}).Name(); got != "not" {
		t.Fatalf("Name(): got %q, want \"not\"", got)
	}
}

// TestNotValue_DoubleNegation_FoldsToLeaf pins that SimplifyValue
// collapses NOT(NOT(TRUE)) to a leaf BooleanValue(true). NotValue
// is in isFoldableComposite (alongside ArithmeticValue / CastValue /
// PromoteValue / ScalarFunctionValue) — Evaluate produces a Go-
// native bool that LiteralValue can faithfully rewrap.
func TestNotValue_DoubleNegation_FoldsToLeaf(t *testing.T) {
	t.Parallel()
	tree := NewNotValue(NewNotValue(NewBooleanValue(true)))
	out := SimplifyValue(tree)
	bv, ok := out.(*BooleanValue)
	if !ok {
		t.Fatalf("expected BooleanValue after fold, got %T", out)
	}
	if bv.Value == nil || *bv.Value != true {
		t.Fatalf("expected NOT(NOT(TRUE)) → TRUE, got %v", bv.Value)
	}
}

// TestNotValue_FoldsConstantToLeaf pins single-NOT folding:
// NOT(TRUE) → BooleanValue(false), NOT(FALSE) → BooleanValue(true),
// NOT(NULL) → NullValue. Each case uses an inline t.Fatalf-based
// asserter (no error-return ceremony); they're parallel-safe via
// the t.Run closure capturing t.
func TestNotValue_FoldsConstantToLeaf(t *testing.T) {
	t.Parallel()

	t.Run("NOT TRUE → FALSE", func(t *testing.T) {
		t.Parallel()
		out := SimplifyValue(NewNotValue(NewBooleanValue(true)))
		bv, ok := out.(*BooleanValue)
		if !ok {
			t.Fatalf("expected BooleanValue, got %T", out)
		}
		if bv.Value == nil || *bv.Value != false {
			t.Fatalf("expected false, got %v", bv.Value)
		}
	})

	t.Run("NOT FALSE → TRUE", func(t *testing.T) {
		t.Parallel()
		out := SimplifyValue(NewNotValue(NewBooleanValue(false)))
		bv, ok := out.(*BooleanValue)
		if !ok {
			t.Fatalf("expected BooleanValue, got %T", out)
		}
		if bv.Value == nil || *bv.Value != true {
			t.Fatalf("expected true, got %v", bv.Value)
		}
	})

	t.Run("NOT NULL(Bool) → NULL", func(t *testing.T) {
		t.Parallel()
		out := SimplifyValue(NewNotValue(&BooleanValue{Value: nil}))
		if _, ok := out.(*NullValue); !ok {
			t.Fatalf("expected NullValue, got %T", out)
		}
	})
}

// TestNotValue_NonConstantChild_NoFold pins that NOT(field) does NOT
// fold — the child isn't constant, so the Not wrapper survives.
func TestNotValue_NonConstantChild_NoFold(t *testing.T) {
	t.Parallel()
	tree := NewNotValue(&FieldValue{Field: "active", Typ: TypeBool})
	out := SimplifyValue(tree)
	if out != Value(tree) {
		// Allow either pointer-stable (child unchanged → return v) or
		// rebuilt-but-still-NotValue. Reject collapse to leaf.
		if _, ok := out.(*NotValue); !ok {
			t.Fatalf("expected NotValue to survive non-constant child, got %T", out)
		}
	}
}
