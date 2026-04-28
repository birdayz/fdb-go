package values

import "testing"

func TestDeconstructRecord_FromRecordConstructor(t *testing.T) {
	t.Parallel()
	a := LiteralValue(int64(1))
	b := LiteralValue("hello")
	rc := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: a},
		RecordConstructorField{Name: "b", Value: b},
	)
	got := DeconstructRecord(rc)
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("DeconstructRecord(rc) = %v, want [a, b]", got)
	}
}

func TestDeconstructRecord_FromRecordTypedValue(t *testing.T) {
	t.Parallel()
	// A FieldValue typed as a RecordType — DeconstructRecord should
	// generate FieldValue accessors per record field.
	rec := &RecordType{
		RecordName: "T",
		Nullable:   false,
		Fields: []Field{
			{Name: "x", FieldType: NotNullLong, Ordinal: 0},
			{Name: "y", FieldType: NotNullString, Ordinal: 1},
		},
	}
	parent := &FieldValue{Field: "row", Typ: rec}
	got := DeconstructRecord(parent)
	if len(got) != 2 {
		t.Fatalf("DeconstructRecord len = %d, want 2", len(got))
	}
	if fv, ok := got[0].(*FieldValue); !ok || fv.Field != "x" {
		t.Fatalf("got[0] = %v, want FieldValue(x)", got[0])
	}
	if fv, ok := got[1].(*FieldValue); !ok || fv.Field != "y" {
		t.Fatalf("got[1] = %v, want FieldValue(y)", got[1])
	}
}

func TestDeconstructRecord_NilReturnsNil(t *testing.T) {
	t.Parallel()
	if got := DeconstructRecord(nil); got != nil {
		t.Fatalf("DeconstructRecord(nil) = %v, want nil", got)
	}
}

func TestDeconstructRecord_NonRecordTypedReturnsNil(t *testing.T) {
	t.Parallel()
	// LiteralValue(int64) — non-record typed → returns nil.
	v := LiteralValue(int64(7))
	if got := DeconstructRecord(v); got != nil {
		t.Fatalf("DeconstructRecord(int) = %v, want nil", got)
	}
}

func TestSimplifyAll_BatchFolds(t *testing.T) {
	t.Parallel()
	in := []Value{
		LiteralValue(int64(1)),
		// Constant arithmetic — should fold to ConstantValue(7).
		&ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(3), Typ: NotNullLong},
			Right: &ConstantValue{Value: int64(4), Typ: NotNullLong},
		},
		LiteralValue("hello"),
	}
	out := SimplifyAll(in)
	if len(out) != 3 {
		t.Fatalf("SimplifyAll len = %d, want 3", len(out))
	}
	// Element 1 should fold to a constant 7.
	if got := out[1].Evaluate(nil); got != int64(7) {
		t.Fatalf("out[1].Evaluate = %v, want 7 (folded)", got)
	}
}

func TestSimplifyAll_PointerStableForUnchanged(t *testing.T) {
	t.Parallel()
	a := LiteralValue(int64(1))
	b := LiteralValue("hello")
	in := []Value{a, b}
	out := SimplifyAll(in)
	// Both elements are leaves — no fold; pointer-equality preserved.
	if out[0] != a || out[1] != b {
		t.Fatalf("SimplifyAll rewrote unchanged leaves")
	}
}

func TestSimplifyAll_EmptyInputReturnsEmpty(t *testing.T) {
	t.Parallel()
	out := SimplifyAll(nil)
	if len(out) != 0 {
		t.Fatalf("SimplifyAll(nil) len = %d, want 0", len(out))
	}
}
