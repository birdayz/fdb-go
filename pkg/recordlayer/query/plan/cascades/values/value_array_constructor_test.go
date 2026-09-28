package values

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArrayConstructorValue_Type(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, []Value{
		LiteralValue(int64(1)),
	})
	got := v.Type()
	at, ok := got.(*ArrayType)
	if !ok {
		t.Fatalf("Type = %T, want *ArrayType", got)
	}
	if !at.ElementType.Equals(NotNullLong) {
		t.Fatalf("ElementType = %v, want NotNullLong", at.ElementType)
	}
	if at.Nullable {
		t.Fatalf("Nullable = true, want false (constructor produces non-nullable arrays)")
	}
}

// The UNTYPED empty literal `[]` (element type NONE) types as the bare
// NONE type, not Array(NONE) — Java's emptyArrayOfNone
// (AbstractArrayConstructorValue.java:304). NONE is what the
// promotion lattice keys on (NONE_TO_ARRAY, MaximumType's NONE arms),
// so `arr = []` promotes instead of failing an Array(NONE) recursion.
func TestArrayConstructorValue_EmptyUntypedIsNoneType(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NoneType, nil)
	if got := v.Type(); got != NoneType {
		t.Fatalf("Type = %v, want NoneType", got)
	}
	// Evaluation is still the empty slice, NOT nil — NONE is the TYPE of
	// `[]`, while its VALUE is an empty array.
	res, err := v.Evaluate(nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if s, ok := res.([]any); !ok || len(s) != 0 {
		t.Fatalf("Evaluate = %#v, want empty []any", res)
	}
}

func TestArrayConstructorValue_NilElementTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(nil, nil)
	at := v.Type().(*ArrayType)
	if at.ElementType != UnknownType {
		t.Fatalf("ElementType = %v, want UnknownType", at.ElementType)
	}
}

func TestArrayConstructorValue_Name(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, nil)
	if got := v.Name(); got != "array" {
		t.Fatalf("Name = %q, want array", got)
	}
}

func TestArrayConstructorValue_Children(t *testing.T) {
	t.Parallel()
	a := LiteralValue(int64(1))
	b := LiteralValue(int64(2))
	v := NewArrayConstructorValue(NotNullLong, []Value{a, b})
	cs := v.Children()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatalf("Children = %v, want [a, b]", cs)
	}
}

func TestArrayConstructorValue_EvaluateConcreteValues(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, []Value{
		LiteralValue(int64(1)),
		LiteralValue(int64(2)),
		LiteralValue(int64(3)),
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(1), int64(2), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_EvaluateEmptyArray(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NotNullLong, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	gotSlice, ok := got.([]any)
	if !ok {
		t.Fatalf("Evaluate = %T, want []any", got)
	}
	if len(gotSlice) != 0 {
		t.Fatalf("Evaluate empty = %v, want empty slice", gotSlice)
	}
	if gotSlice == nil {
		t.Fatalf("Evaluate empty = nil — empty array must be non-nil to distinguish from NULL")
	}
}

func TestArrayConstructorValue_EvaluatePassesThroughNULLs(t *testing.T) {
	t.Parallel()
	v := NewArrayConstructorValue(NullableLong, []Value{
		LiteralValue(int64(1)),
		LiteralValue(nil), // SQL NULL
		LiteralValue(int64(3)),
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(1), nil, int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate w/ NULL = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_NilChildToleratedAsNil(t *testing.T) {
	t.Parallel()
	// The raw Go constructor tolerates a nil Value child (different from
	// a Value evaluating to nil) and evaluates it as a nil element.
	// Java rejects nil children when copying its constructor arguments.
	v := NewArrayConstructorValue(NullableLong, []Value{
		LiteralValue(int64(1)),
		nil,
		LiteralValue(int64(3)),
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(1), any(nil), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate w/ nil child = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_HeterogeneousElements(t *testing.T) {
	t.Parallel()
	// Element-type validation is the planner's responsibility — the
	// constructor doesn't reject mismatched children; each child's
	// evaluation flows through verbatim.
	v := NewArrayConstructorValue(NullableString, []Value{
		LiteralValue("hello"),
		LiteralValue(int64(42)), // int in a string-typed array
	})
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{"hello", int64(42)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate hetero = %v, want %v", got, want)
	}
}

func TestArrayConstructorValue_DefensiveCopyOfElements(t *testing.T) {
	t.Parallel()
	original := []Value{LiteralValue(int64(1))}
	v := NewArrayConstructorValue(NotNullLong, original)
	original[0] = LiteralValue(int64(999))
	tmpEv0, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	got := tmpEv0.([]any)
	if got[0] == int64(999) {
		t.Fatalf("Elements aliased caller's slice — not defensively copied")
	}
}

func TestArrayConstructorValue_CheckedRebuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		element     Type
		children    []Value
		want        []any
		wantErr     bool
		wantEvalErr bool
	}{
		{name: "exact", element: NotNullLong, children: []Value{&ConstantValue{Value: int64(7), Typ: NotNullLong}}, want: []any{int64(7)}},
		{name: "nullable", element: NullableLong, children: []Value{NewNullValue(NotNullLong)}, want: []any{nil}},
		{name: "nullable_widening", element: NotNullLong, children: []Value{NewNullValue(NotNullLong)}, want: []any{nil}},
		{name: "nullable_narrowing", element: NullableLong, children: []Value{&ConstantValue{Value: int64(7), Typ: NotNullLong}}, want: []any{int64(7)}},
		{name: "type_drift", element: NotNullLong, children: []Value{&ConstantValue{Value: int32(7), Typ: NotNullInt}}, want: []any{int64(7)}},
		{name: "incompatible", element: NotNullLong, children: []Value{&ConstantValue{Value: "x", Typ: NotNullString}, &ConstantValue{Value: int64(7), Typ: NotNullLong}}, wantErr: true},
		{name: "numeric_promotion", element: NotNullLong, children: []Value{&ConstantValue{Value: int32(3), Typ: NotNullInt}, &ConstantValue{Value: int64(7), Typ: NotNullLong}}, want: []any{int64(3), int64(7)}},
		{name: "any", element: AnyType, children: []Value{LiteralValue("x"), LiteralValue(int64(7))}, want: []any{"x", int64(7)}},
		{name: "nil_child", element: NullableLong, children: []Value{nil}, wantErr: true},
		{name: "typed_nil_child", element: NullableLong, children: []Value{(*ConstantValue)(nil)}, wantErr: true},
		{name: "nested_array_drift", element: NewArrayType(false, NotNullLong), children: []Value{
			&ConstantValue{Value: []any{nil}, Typ: NewArrayType(false, NullableLong)},
		}, wantEvalErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := NewArrayConstructorValue(tc.element, []Value{LiteralValue(int64(1))})
			beforeType, beforeChild := original.Type(), original.Elements[0]
			rebuilt, err := withChildrenChecked(original, tc.children)
			if !original.Type().Equals(beforeType) || original.Elements[0] != beforeChild {
				t.Fatal("rebuild mutated the original array")
			}
			if tc.wantErr {
				var resolutionErr *ResolutionError
				require.ErrorAs(t, err, &resolutionErr)
				if rebuilt != nil || original.WithChildren(tc.children) != nil || WithChildren(original, tc.children) != nil {
					t.Fatal("failed array reconstruction published a partial or typed-nil value")
				}
				return
			}
			require.NoError(t, err)
			if rebuilt == nil || !rebuilt.Type().Equals(beforeType) {
				t.Fatalf("rebuild changed the declared type: %v", rebuilt)
			}
			got, err := rebuilt.Evaluate(nil)
			if tc.wantEvalErr {
				var nullAssignment *NonNullableFieldError
				require.ErrorAs(t, err, &nullAssignment)
				return
			}
			require.NoError(t, err)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rebuilt evaluation = %#v, want %#v", got, tc.want)
			}
			if tc.name == "numeric_promotion" {
				promoted, ok := rebuilt.Children()[0].(*PromoteValue)
				if !ok || promoted.Child != tc.children[0] || !promoted.Type().Equals(NotNullLong) {
					t.Fatal("numeric reconstruction must promote the int child, not relabel its array")
				}
			}
			tc.children[0] = LiteralValue(int64(999))
			got, err = rebuilt.Evaluate(nil)
			require.NoError(t, err)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatal("rebuilt constructor retained the caller's mutable child slice")
			}
		})
	}
	for _, element := range []Type{NoneType, NotNullLong, AnyType} {
		t.Run("empty_"+element.String(), func(t *testing.T) {
			t.Parallel()
			original := NewArrayConstructorValue(element, nil)
			rebuilt, err := withChildrenChecked(original, nil)
			require.NoError(t, err)
			if rebuilt != original || original.WithChildren(nil) != original {
				t.Fatal("empty reconstruction must retain the original typed or untyped empty array")
			}
		})
	}
}

func TestArrayConstructorValue_CheckedRebuildNestedNumericCarriers(t *testing.T) {
	t.Parallel()
	var copyArray func([]any) []any
	copyArray = func(input []any) []any {
		out := make([]any, len(input))
		for i, value := range input {
			if nested, ok := value.([]any); ok {
				out[i] = copyArray(nested)
			} else {
				out[i] = value
			}
		}
		return out
	}
	for _, tc := range []struct {
		name           string
		source, target Type
		before, want   []any
		other          []any
	}{
		{
			name: "int_to_long", source: NewArrayType(false, NotNullInt), target: NewArrayType(false, NotNullLong),
			before: []any{int32(3)}, want: []any{int64(3)}, other: []any{int64(7)},
		},
		{
			name: "long_to_double", source: NewArrayType(false, NotNullLong), target: NewArrayType(false, NotNullDouble),
			before: []any{int64(3)}, want: []any{float64(3)}, other: []any{float64(7)},
		},
		{
			name: "nested_int_to_long", source: NewArrayType(false, NewArrayType(false, NotNullInt)), target: NewArrayType(false, NewArrayType(false, NotNullLong)),
			before: []any{[]any{int32(3)}}, want: []any{[]any{int64(3)}}, other: []any{[]any{int64(7)}},
		},
		{
			name: "nullable_elements", source: NewArrayType(false, NullableInt), target: NewArrayType(false, NullableLong),
			before: []any{int32(3), nil}, want: []any{int64(3), nil}, other: []any{nil, int64(7)},
		},
		{
			name: "empty", source: NewArrayType(false, NotNullInt), target: NewArrayType(false, NotNullLong),
			before: []any{}, want: []any{}, other: []any{int64(7)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := &ConstantValue{Value: copyArray(tc.before), Typ: tc.source}
			other := &ConstantValue{Value: tc.other, Typ: tc.target}
			original := NewArrayConstructorValue(tc.target, []Value{other, other})
			beforeType := original.Type()
			rebuilt, err := WithChildrenChecked(original, []Value{child, other})
			require.NoError(t, err)
			require.True(t, beforeType.Equals(rebuilt.Type()))
			promoted, ok := rebuilt.Children()[0].(*PromoteValue)
			require.True(t, ok, "the rebuild must insert a conversion, not relabel the child")
			require.Same(t, child, promoted.Child)
			require.True(t, promoted.Type().Equals(tc.target))
			got, err := rebuilt.Evaluate(nil)
			require.NoError(t, err)
			require.Equal(t, []any{tc.want, tc.other}, got, "nested carriers must agree with the advertised element type")
			unchanged, err := child.Evaluate(nil)
			require.NoError(t, err)
			require.Equal(t, tc.before, unchanged)
			require.True(t, child.Type().Equals(tc.source))
			require.Same(t, other, original.Elements[0])
			require.Same(t, other, original.Elements[1])
		})
	}
}

func TestArrayConstructorValue_FieldMapFailureIsAtomic(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("", false, []Field{{Name: "ID", FieldType: NotNullLong}})
	root := mustLayoutCurrentQOV(t, rowType)
	field, err := ResolveFieldOrdinals(root, []int{0})
	require.NoError(t, err)
	array := NewArrayConstructorValue(NotNullLong, []Value{field})
	for name, original := range map[string]Value{
		"array":  array,
		"record": NewRecordConstructorValue(RecordConstructorField{Name: "A", Value: array}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mapped := MapFieldValues(original, func(*fieldValue) Value { return LiteralValue("incompatible") })
			if mapped != nil {
				t.Fatalf("%T mapping published a partially rebuilt value or typed nil: %T", original, mapped)
			}
			if !field.Type().Equals(NotNullLong) || array.Elements[0] != field {
				t.Fatal("failed field mapping mutated the original array or field")
			}
		})
	}
}

func TestArrayConstructorValue_WithChildren(t *testing.T) {
	t.Parallel()
	original := NewArrayConstructorValue(NotNullLong, []Value{&ConstantValue{Value: int64(1), Typ: NotNullLong}})
	rebuilt := original.WithChildren([]Value{
		&ConstantValue{Value: int64(10), Typ: NotNullLong},
		&ConstantValue{Value: int64(20), Typ: NotNullLong},
	})
	got, errEv0 := rebuilt.Evaluate(nil)
	require.NoError(t, errEv0)
	want := []any{int64(10), int64(20)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt.Evaluate = %v, want %v", got, want)
	}
	// Element type carries through.
	at := rebuilt.Type().(*ArrayType)
	if !at.ElementType.Equals(NotNullLong) {
		t.Fatalf("rebuilt.ElementType = %v, want NotNullLong (carried through)", at.ElementType)
	}
}

func TestArrayConstructorValue_CheckedRebuildNilChildType(t *testing.T) {
	t.Parallel()
	for name, child := range map[string]Value{
		"derived_nil":       &DerivedValue{},
		"record_nil":        &QuantifiedRecordValue{},
		"derived_typed_nil": &DerivedValue{ResultType: (*RecordType)(nil)},
		"record_typed_nil":  &QuantifiedRecordValue{ResultType: (*ArrayType)(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, element := range []Type{NotNullLong, AnyType} {
				originalChild := LiteralValue(int64(1))
				original := NewArrayConstructorValue(element, []Value{originalChild})
				rebuilt, err := WithChildrenChecked(original, []Value{child})
				var diagnostic *ResolutionError
				require.ErrorAs(t, err, &diagnostic)
				require.Equal(t, TypeNil, diagnostic.Code())
				require.Nil(t, rebuilt)
				require.Nil(t, original.WithChildren([]Value{child}))
				require.Nil(t, WithChildren(original, []Value{child}))
				require.Same(t, originalChild, original.Elements[0])
				require.Same(t, element, original.ElementType)
			}
		})
	}
}
