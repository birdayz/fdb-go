package functions

import (
	"math"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// ---------------------------------------------------------------------------
// CompareValues
// ---------------------------------------------------------------------------

func TestCompareValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b any
		want int
	}{
		// NULL ordering.
		{"nil_nil", nil, nil, 0},
		{"nil_int", nil, int64(1), -1},
		{"int_nil", int64(1), nil, 1},

		// int64 × int64.
		{"int64_equal", int64(42), int64(42), 0},
		{"int64_less", int64(1), int64(2), -1},
		{"int64_greater", int64(5), int64(3), 1},
		{"int64_negative", int64(-10), int64(-5), -1},
		{"int64_zero", int64(0), int64(0), 0},

		// int64 × float64 promotion.
		{"int64_float64_equal", int64(3), float64(3.0), 0},
		{"int64_float64_less", int64(2), float64(2.5), -1},
		{"float64_int64_greater", float64(5.5), int64(5), 1},

		// float64 × float64.
		{"float64_equal", float64(1.5), float64(1.5), 0},
		{"float64_less", float64(1.0), float64(2.0), -1},
		{"float64_greater", float64(3.0), float64(1.0), 1},

		// string.
		{"string_equal", "abc", "abc", 0},
		{"string_less", "abc", "abd", -1},
		{"string_greater", "abd", "abc", 1},
		{"string_empty", "", "", 0},
		{"string_empty_vs_nonempty", "", "a", -1},

		// bool (false < true).
		{"bool_equal_true", true, true, 0},
		{"bool_equal_false", false, false, 0},
		{"bool_false_lt_true", false, true, -1},
		{"bool_true_gt_false", true, false, 1},

		// []byte.
		{"bytes_equal", []byte{1, 2, 3}, []byte{1, 2, 3}, 0},
		{"bytes_less", []byte{1, 2}, []byte{1, 3}, -1},
		{"bytes_greater", []byte{1, 3}, []byte{1, 2}, 1},
		{"bytes_empty", []byte{}, []byte{}, 0},

		// Cross-type: stable non-zero result. We don't care about the
		// specific sign, just that it's not zero (meaning "not equal").
		{"string_vs_bool", "hello", true, 0},      // placeholder; checked below
		{"int64_vs_string", int64(1), "hello", 0}, // placeholder; checked below
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CompareValues(tt.a, tt.b)

			// For cross-type tests we only assert non-zero.
			switch tt.name {
			case "string_vs_bool", "int64_vs_string":
				if got == 0 {
					t.Errorf("CompareValues(%v, %v) = 0, want non-zero for cross-type", tt.a, tt.b)
				}
			default:
				if got != tt.want {
					t.Errorf("CompareValues(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
				}
			}
		})
	}
}

func TestCompareValues_CrossTypeStable(t *testing.T) {
	t.Parallel()
	// The cross-type result must be consistent across calls (stable ordering).
	r1 := CompareValues(int64(1), "hello")
	r2 := CompareValues(int64(1), "hello")
	if r1 != r2 {
		t.Errorf("cross-type compare not stable: %d vs %d", r1, r2)
	}
	if r1 == 0 {
		t.Error("cross-type compare returned 0, expected non-zero")
	}
}

// ---------------------------------------------------------------------------
// AddInt64Checked
// ---------------------------------------------------------------------------

func TestAddInt64Checked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		a, b   int64
		want   int64
		wantOK bool
	}{
		{"simple", 1, 2, 3, true},
		{"zero", 0, 0, 0, true},
		{"negative", -3, -4, -7, true},
		{"mixed", -5, 10, 5, true},
		{"max_plus_one_overflow", math.MaxInt64, 1, 0, false},
		{"min_minus_one_overflow", math.MinInt64, -1, 0, false},
		{"max_plus_zero", math.MaxInt64, 0, math.MaxInt64, true},
		{"min_plus_zero", math.MinInt64, 0, math.MinInt64, true},
		{"positive_boundary", math.MaxInt64 - 1, 1, math.MaxInt64, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := AddInt64Checked(tt.a, tt.b)
			if ok != tt.wantOK {
				t.Fatalf("AddInt64Checked(%d, %d) ok=%v, want %v", tt.a, tt.b, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("AddInt64Checked(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SubInt64Checked
// ---------------------------------------------------------------------------

func TestSubInt64Checked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		a, b   int64
		want   int64
		wantOK bool
	}{
		{"simple", 5, 3, 2, true},
		{"zero", 0, 0, 0, true},
		{"negative_result", 3, 5, -2, true},
		{"min_minus_one_overflow", math.MinInt64, 1, 0, false},
		{"max_minus_neg1_overflow", math.MaxInt64, -1, 0, false},
		{"min_minus_zero", math.MinInt64, 0, math.MinInt64, true},
		{"max_minus_zero", math.MaxInt64, 0, math.MaxInt64, true},
		{"negative_boundary", math.MinInt64 + 1, 1, math.MinInt64, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SubInt64Checked(tt.a, tt.b)
			if ok != tt.wantOK {
				t.Fatalf("SubInt64Checked(%d, %d) ok=%v, want %v", tt.a, tt.b, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("SubInt64Checked(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MulInt64Checked
// ---------------------------------------------------------------------------

func TestMulInt64Checked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		a, b   int64
		want   int64
		wantOK bool
	}{
		{"simple", 3, 4, 12, true},
		{"zero_left", 0, 99, 0, true},
		{"zero_right", 99, 0, 0, true},
		{"zero_both", 0, 0, 0, true},
		{"negative", -3, 4, -12, true},
		{"negative_negative", -3, -4, 12, true},
		{"one", 1, math.MaxInt64, math.MaxInt64, true},
		{"minint64_neg1_overflow", math.MinInt64, -1, 0, false},
		{"neg1_minint64_overflow", -1, math.MinInt64, 0, false},
		{"maxint64_times2_overflow", math.MaxInt64, 2, 0, false},
		{"large_overflow", math.MaxInt64/2 + 1, 2, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := MulInt64Checked(tt.a, tt.b)
			if ok != tt.wantOK {
				t.Fatalf("MulInt64Checked(%d, %d) ok=%v, want %v", tt.a, tt.b, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("MulInt64Checked(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ApplyMathOp
// ---------------------------------------------------------------------------

func TestApplyMathOp(t *testing.T) {
	t.Parallel()

	t.Run("int64_add", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(3), int64(4), "+")
		assertNoErr(t, err)
		assertEq(t, got, int64(7))
	})

	t.Run("int64_sub", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(10), int64(3), "-")
		assertNoErr(t, err)
		assertEq(t, got, int64(7))
	})

	t.Run("int64_mul", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(5), int64(6), "*")
		assertNoErr(t, err)
		assertEq(t, got, int64(30))
	})

	t.Run("int64_div", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(10), int64(3), "/")
		assertNoErr(t, err)
		assertEq(t, got, int64(3)) // truncation toward zero
	})

	t.Run("int64_mod", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(10), int64(3), "%")
		assertNoErr(t, err)
		assertEq(t, got, int64(1))
	})

	t.Run("int64_div_by_zero", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyMathOp(int64(10), int64(0), "/")
		assertErr(t, err)
	})

	t.Run("int64_mod_by_zero", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyMathOp(int64(10), int64(0), "%")
		assertErr(t, err)
	})

	t.Run("int64_overflow_add", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyMathOp(int64(math.MaxInt64), int64(1), "+")
		assertErr(t, err)
	})

	t.Run("int64_div_minint64_neg1_wraps", func(t *testing.T) {
		t.Parallel()
		// Java DIV_LL is an unchecked (long)l / (long)r — MinLong / -1
		// wraps to MinLong instead of raising 22003 (ArithmeticValue.java).
		got, err := ApplyMathOp(int64(math.MinInt64), int64(-1), "/")
		assertNoErr(t, err)
		assertEq(t, got, int64(math.MinInt64))
	})

	t.Run("mixed_int_float_add", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(3), float64(1.5), "+")
		assertNoErr(t, err)
		assertEq(t, got, float64(4.5))
	})

	t.Run("mixed_float_int_sub", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(float64(10.0), int64(3), "-")
		assertNoErr(t, err)
		assertEq(t, got, float64(7.0))
	})

	t.Run("float_div_no_error_for_zero", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(float64(1.0), float64(0.0), "/")
		assertNoErr(t, err)
		if !math.IsInf(got.(float64), 1) {
			t.Errorf("expected +Inf, got %v", got)
		}
	})

	t.Run("float_div_zero_by_zero_nan", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(float64(0.0), float64(0.0), "/")
		assertNoErr(t, err)
		if !math.IsNaN(got.(float64)) {
			t.Errorf("expected NaN, got %v", got)
		}
	})

	t.Run("null_propagation_left", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(nil, int64(1), "+")
		assertNoErr(t, err)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("null_propagation_right", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(int64(1), nil, "+")
		assertNoErr(t, err)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("null_propagation_both", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp(nil, nil, "+")
		assertNoErr(t, err)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("string_concat", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp("foo", "bar", "+")
		assertNoErr(t, err)
		assertEq(t, got, "foobar")
	})

	t.Run("string_concat_empty", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyMathOp("", "bar", "+")
		assertNoErr(t, err)
		assertEq(t, got, "bar")
	})

	t.Run("unsupported_op", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyMathOp(int64(1), int64(2), "**")
		assertErr(t, err)
	})

	t.Run("non_numeric_error", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyMathOp("abc", int64(1), "-")
		assertErr(t, err)
	})

	t.Run("string_sub_error", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyMathOp("abc", "def", "-")
		assertErr(t, err)
	})
}

// ---------------------------------------------------------------------------
// ApplyBitOp
// ---------------------------------------------------------------------------

func TestApplyBitOp(t *testing.T) {
	t.Parallel()

	t.Run("and", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyBitOp(int64(0xFF), int64(0x0F), "&")
		assertNoErr(t, err)
		assertEq(t, got, int64(0x0F))
	})

	t.Run("or", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyBitOp(int64(0xF0), int64(0x0F), "|")
		assertNoErr(t, err)
		assertEq(t, got, int64(0xFF))
	})

	t.Run("xor", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyBitOp(int64(0xFF), int64(0x0F), "^")
		assertNoErr(t, err)
		assertEq(t, got, int64(0xF0))
	})

	t.Run("null_propagation_left", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyBitOp(nil, int64(1), "&")
		assertNoErr(t, err)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("null_propagation_right", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyBitOp(int64(1), nil, "&")
		assertNoErr(t, err)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("non_int64_error", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyBitOp(float64(1.0), int64(1), "&")
		assertErr(t, err)
	})

	t.Run("unsupported_shift_left", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyBitOp(int64(1), int64(2), "<<")
		assertErr(t, err)
	})

	t.Run("unsupported_shift_right", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyBitOp(int64(1), int64(2), ">>")
		assertErr(t, err)
	})
}

// ---------------------------------------------------------------------------
// ToFloat64
// ---------------------------------------------------------------------------

func TestToFloat64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		v      any
		want   float64
		wantOK bool
	}{
		{"int64", int64(42), 42.0, true},
		{"int64_negative", int64(-7), -7.0, true},
		{"int64_zero", int64(0), 0.0, true},
		{"float64", float64(3.14), 3.14, true},
		{"float64_zero", float64(0.0), 0.0, true},
		{"string_false", "hello", 0, false},
		{"nil_false", nil, 0, false},
		{"bool_false", true, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ToFloat64(tt.v)
			if ok != tt.wantOK {
				t.Fatalf("ToFloat64(%v) ok=%v, want %v", tt.v, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ToFloat64(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ToIntegerArg
// ---------------------------------------------------------------------------

func TestToIntegerArg(t *testing.T) {
	t.Parallel()

	t.Run("int64_passthrough", func(t *testing.T) {
		t.Parallel()
		got, err := ToIntegerArg(int64(42), "LEFT", "length")
		assertNoErr(t, err)
		assertEq(t, got, int64(42))
	})

	t.Run("whole_float64", func(t *testing.T) {
		t.Parallel()
		got, err := ToIntegerArg(float64(5.0), "LEFT", "length")
		assertNoErr(t, err)
		assertEq(t, got, int64(5))
	})

	t.Run("fractional_float64_error", func(t *testing.T) {
		t.Parallel()
		_, err := ToIntegerArg(float64(5.5), "LEFT", "length")
		assertErr(t, err)
	})

	t.Run("nan_error", func(t *testing.T) {
		t.Parallel()
		_, err := ToIntegerArg(math.NaN(), "LEFT", "length")
		assertErr(t, err)
	})

	t.Run("inf_error", func(t *testing.T) {
		t.Parallel()
		_, err := ToIntegerArg(math.Inf(1), "LEFT", "length")
		assertErr(t, err)
	})

	t.Run("neg_inf_error", func(t *testing.T) {
		t.Parallel()
		_, err := ToIntegerArg(math.Inf(-1), "LEFT", "length")
		assertErr(t, err)
	})

	t.Run("string_error", func(t *testing.T) {
		t.Parallel()
		_, err := ToIntegerArg("abc", "LEFT", "length")
		assertErr(t, err)
	})

	t.Run("nil_error", func(t *testing.T) {
		t.Parallel()
		_, err := ToIntegerArg(nil, "LEFT", "length")
		assertErr(t, err)
	})
}

// ---------------------------------------------------------------------------
// StripStringLiteralQuotes
// ---------------------------------------------------------------------------

func TestStripStringLiteralQuotes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"quoted", "'hello'", "hello"},
		{"escaped_quote", "'it''s'", "it's"},
		{"no_quotes", "hello", "hello"},
		{"empty_quoted", "''", ""},
		{"single_char_quoted", "'x'", "x"},
		{"multiple_escaped", "'a''b''c'", "a'b'c"},
		{"only_single_quote", "'", "'"},
		{"empty_string", "", ""},
		{"double_quotes_untouched", "\"hello\"", "\"hello\""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := StripStringLiteralQuotes(tt.input)
			if got != tt.want {
				t.Errorf("StripStringLiteralQuotes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LiteralMatchesPKKind
// ---------------------------------------------------------------------------

func TestLiteralMatchesPKKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  any
		kind protoreflect.Kind
		want bool
	}{
		// Integer kinds accept integer types.
		{"int64_to_Int64Kind", int64(1), protoreflect.Int64Kind, true},
		{"int_to_Int64Kind", int(1), protoreflect.Int64Kind, true},
		{"int32_to_Int32Kind", int32(1), protoreflect.Int32Kind, true},
		{"uint64_to_Uint64Kind", uint64(1), protoreflect.Uint64Kind, true},

		// Integer kinds reject non-integer types.
		{"string_to_Int64Kind", "hello", protoreflect.Int64Kind, false},
		{"float64_to_Int64Kind", float64(1.0), protoreflect.Int64Kind, false},
		{"bool_to_Int64Kind", true, protoreflect.Int64Kind, false},
		{"nil_to_Int64Kind", nil, protoreflect.Int64Kind, false},

		// String kind.
		{"string_to_StringKind", "hello", protoreflect.StringKind, true},
		{"int64_to_StringKind", int64(1), protoreflect.StringKind, false},

		// Bytes kind.
		{"bytes_to_BytesKind", []byte{1, 2}, protoreflect.BytesKind, true},
		{"string_to_BytesKind", "hello", protoreflect.BytesKind, false},

		// Bool kind (not handled — returns false).
		{"bool_to_BoolKind", true, protoreflect.BoolKind, false},

		// Float kind (not handled — returns false).
		{"float64_to_FloatKind", float64(1.0), protoreflect.FloatKind, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := LiteralMatchesPKKind(tt.val, tt.kind)
			if got != tt.want {
				t.Errorf("LiteralMatchesPKKind(%v, %v) = %v, want %v",
					tt.val, tt.kind, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func assertEq(t *testing.T, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("got %v (%T), want %v (%T)", got, got, want, want)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkCompareValues_Int64(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(int64(42), int64(99))
	}
}

func BenchmarkCompareValues_Float64(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(float64(3.14), float64(2.71))
	}
}

func BenchmarkCompareValues_MixedNumeric(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(int64(42), float64(42.0))
	}
}

func BenchmarkCompareValues_String(b *testing.B) {
	for b.Loop() {
		_ = CompareValues("hello world", "hello xorld")
	}
}

func BenchmarkCompareValues_Bytes(b *testing.B) {
	a := []byte("hello world this is test data")
	c := []byte("hello world this is test datb")
	for b.Loop() {
		_ = CompareValues(a, c)
	}
}

func BenchmarkCompareValues_NilLeft(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(nil, int64(1))
	}
}

func BenchmarkIsTruthy_Int64(b *testing.B) {
	for b.Loop() {
		_ = IsTruthy(int64(1))
	}
}

func BenchmarkIsTruthy_Nil(b *testing.B) {
	for b.Loop() {
		_ = IsTruthy(nil)
	}
}

func BenchmarkIsTruthy_String(b *testing.B) {
	for b.Loop() {
		_ = IsTruthy("hello")
	}
}

func BenchmarkApplyMathOp_IntAdd(b *testing.B) {
	for b.Loop() {
		_, _ = ApplyMathOp(int64(100), int64(200), "+")
	}
}

func BenchmarkApplyMathOp_FloatMul(b *testing.B) {
	for b.Loop() {
		_, _ = ApplyMathOp(float64(3.14), float64(2.71), "*")
	}
}

func BenchmarkApplyMathOp_IntMod(b *testing.B) {
	for b.Loop() {
		_, _ = ApplyMathOp(int64(100), int64(7), "%")
	}
}

func BenchmarkStripStringLiteralQuotes(b *testing.B) {
	for b.Loop() {
		_ = StripStringLiteralQuotes("'it''s a test'")
	}
}
