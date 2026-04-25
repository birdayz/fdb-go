package cascades

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

// TestNotValue_Type pins that NotValue always reports TypeBool —
// independent of the child's type. NOT is a boolean-typed operator.
func TestNotValue_Type(t *testing.T) {
	t.Parallel()
	v := NewNotValue(&ConstantValue{Value: int64(1), Typ: TypeInt})
	if v.Type() != TypeBool {
		t.Fatalf("Type: got %v, want TypeBool", v.Type())
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

// TestNotValue_DoubleNegation_NotConstantFolded pins that
// NotValue is NOT auto-folded by the seed simplifier today —
// SimplifyValue.isFoldableComposite excludes NotValue, so
// NOT(NOT(TRUE)) survives as a 3-deep tree at plan time.
//
// This is intentional: the seed simplifier folds composites whose
// Evaluate produces a Go-native scalar that LiteralValue can
// faithfully rewrap (Arithmetic, Cast, Promote, ScalarFunction).
// Adding NotValue to the foldable set would also work — explicitly
// marking the gap so a future addition is a deliberate decision,
// not a silent drift.
func TestNotValue_DoubleNegation_NotConstantFolded(t *testing.T) {
	t.Parallel()
	tree := NewNotValue(NewNotValue(NewBooleanValue(true)))
	out := SimplifyValue(tree)
	// CURRENT BEHAVIOUR: NotValue is NOT in isFoldableComposite, so
	// SimplifyValue returns the tree unchanged. Pin this so a future
	// extension is flagged.
	if _, ok := out.(*NotValue); !ok {
		t.Fatalf("SimplifyValue folded NotValue (current seed should not — flag this and update isFoldableComposite if intentional). Got %T", out)
	}
}
