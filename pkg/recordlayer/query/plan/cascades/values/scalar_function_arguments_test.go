package values

import "testing"

// TestDiagnoseScalarFunctionArguments pins Java's TWO-STEP argument admission
// for GREATEST/LEAST, and pins the two steps SEPARATELY because they carry
// different SQLSTATEs that the Java corpus asserts individually
// (functions.yamsql: `greatest(a9, '0')` wants 22000, `least(a6, a6)` wants
// 22F00).
//
// A test that only checked "these are rejected" would pass with the two codes
// swapped, which is the exact defect this replaced: Go answered 22000 for
// both, and then — with a naive operator gate — 22F00 for both.
func TestDiagnoseScalarFunctionArguments(t *testing.T) {
	t.Parallel()

	structType := &RecordType{Nullable: true, Fields: []Field{
		{Name: "A", FieldType: NullableLong, Ordinal: 0},
	}}

	for _, tc := range []struct {
		name string
		fn   string
		args []Value
		want ScalarFunctionArgumentDiagnosis
	}{
		// Step 2: the types agree, but no physical operator implements the
		// comparison for them. Java: FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES.
		{
			"struct_has_no_operator", "LEAST",
			[]Value{typed(structType), typed(structType)},
			ScalarFunctionArgumentsNoOperator,
		},
		{
			"bytes_has_no_operator", "GREATEST",
			[]Value{typed(NullableBytes), typed(NullableBytes)},
			ScalarFunctionArgumentsNoOperator,
		},

		// Step 1: the types have no common type at all. Java: INCOMPATIBLE_TYPE.
		// This is the arm Go used to get WRONG in the permissive direction —
		// CommonValueType folds (BYTES, STRING) to BYTES, so the call planned
		// and returned a value where Java rejects the query.
		{
			"bytes_vs_string_is_incompatible", "GREATEST",
			[]Value{typed(NullableBytes), typed(NullableString)},
			ScalarFunctionArgumentsIncompatible,
		},
		{
			"string_vs_long_is_incompatible", "LEAST",
			[]Value{typed(NullableString), typed(NullableLong)},
			ScalarFunctionArgumentsIncompatible,
		},

		// Admissible: a registered operator exists.
		{
			"long_is_fine", "LEAST",
			[]Value{typed(NullableLong), typed(NullableLong)},
			ScalarFunctionArgumentsOK,
		},
		{
			"boolean_is_fine", "GREATEST",
			[]Value{typed(NullableBoolean), typed(NullableBoolean)},
			ScalarFunctionArgumentsOK,
		},
		// Numeric widening folds rather than conflicting — Java's maximumType
		// promotes INT with DOUBLE to DOUBLE, which has an operator.
		{
			"int_widens_to_double", "LEAST",
			[]Value{typed(NullableInt), typed(NullableDouble)},
			ScalarFunctionArgumentsOK,
		},
		// A NULL literal is absorbed into the other operand's type rather than
		// conflicting with it, so `least(x, null)` is typed by x alone.
		{
			"null_literal_is_absorbed", "LEAST",
			[]Value{typed(NullableString), typed(NullType)},
			ScalarFunctionArgumentsOK,
		},
		// A struct beside a NULL is still a struct, so it is still the
		// no-operator arm and NOT the incompatible one.
		{
			"struct_with_null_is_still_no_operator", "LEAST",
			[]Value{typed(structType), typed(NullType)},
			ScalarFunctionArgumentsNoOperator,
		},

		// A function that declares no operator map is unrestricted; COALESCE
		// follows Java's own (different) admission and must not be caught here.
		{
			"coalesce_is_unrestricted", "COALESCE",
			[]Value{typed(structType), typed(structType)},
			ScalarFunctionArgumentsOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DiagnoseScalarFunctionArguments(tc.fn, tc.args); got != tc.want {
				t.Fatalf("DiagnoseScalarFunctionArguments(%s) = %v, want %v",
					tc.fn, got, tc.want)
			}
		})
	}
}

// typed builds a Value carrying only a type — enough for argument admission,
// which never evaluates.
func typed(t Type) Value { return &ConstantValue{Value: nil, Typ: t} }
