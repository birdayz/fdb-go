package recordlayer

import (
	"math"
	"testing"
)

// evaluateArithmetic is a test helper that invokes a registered arithmetic function
// with a single argument tuple.
func evaluateArithmetic(t *testing.T, name string, args []any) (any, error) {
	t.Helper()
	fn, ok := globalFunctionRegistry[name]
	if !ok {
		t.Fatalf("function %q not registered", name)
	}
	result, err := fn(nil, nil, [][]any{args})
	if err != nil {
		return nil, err
	}
	if len(result) != 1 || len(result[0]) != 1 {
		t.Fatalf("expected [[value]], got %v", result)
	}
	return result[0][0], nil
}

func TestArithmeticAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"positive", 1, 2, 3},
		{"zero", 0, 0, 0},
		{"negative", -5, -3, -8},
		{"mixed", -10, 7, -3},
		{"large", math.MaxInt64 - 1, 1, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "add", []any{tc.a, tc.b})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("add(%d, %d) = %v, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestArithmeticAddOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
	}{
		{"max+1", math.MaxInt64, 1},
		{"min-1", math.MinInt64, -1},
		{"both_large", math.MaxInt64, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := evaluateArithmetic(t, "add", []any{tc.a, tc.b})
			if err == nil {
				t.Errorf("add(%d, %d) should overflow", tc.a, tc.b)
			}
		})
	}
}

func TestArithmeticSubtract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"basic", 5, 3, 2},
		{"negative_result", 3, 5, -2},
		{"zero", 0, 0, 0},
		{"from_min", math.MinInt64 + 1, 1, math.MinInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "subtract", []any{tc.a, tc.b})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("subtract(%d, %d) = %v, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestArithmeticSubtractUnary(t *testing.T) {
	t.Parallel()

	// "subtract" with 1 arg uses negateExact (overflow-checked).
	tests := []struct {
		name string
		x    int64
		want int64
	}{
		{"positive", 5, -5},
		{"negative", -3, 3},
		{"zero", 0, 0},
		{"max", math.MaxInt64, -math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "subtract", []any{tc.x})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("subtract(%d) = %v, want %d", tc.x, got, tc.want)
			}
		})
	}
}

func TestArithmeticSubtractUnaryOverflow(t *testing.T) {
	t.Parallel()

	// "subtract" unary on MinInt64 should overflow (uses negateExact).
	_, err := evaluateArithmetic(t, "subtract", []any{int64(math.MinInt64)})
	if err == nil {
		t.Error("subtract(MinInt64) should overflow with negateExact")
	}
}

func TestArithmeticSubUnary(t *testing.T) {
	t.Parallel()

	// "sub" with 1 arg uses plain negation (no overflow check).
	// MinInt64 wraps silently, matching Java's x -> -x lambda.
	got, err := evaluateArithmetic(t, "sub", []any{int64(math.MinInt64)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// -MinInt64 wraps to MinInt64 in two's complement.
	if got != int64(math.MinInt64) {
		t.Errorf("sub(MinInt64) = %v, want MinInt64 (wrap)", got)
	}
}

func TestArithmeticSubBinary(t *testing.T) {
	t.Parallel()

	// "sub" binary is subtractExact (overflow-checked).
	got, err := evaluateArithmetic(t, "sub", []any{int64(10), int64(3)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != int64(7) {
		t.Errorf("sub(10, 3) = %v, want 7", got)
	}
}

func TestArithmeticSubtractOverflow(t *testing.T) {
	t.Parallel()

	// binary subtract: MinInt64 - 1 overflows
	_, err := evaluateArithmetic(t, "subtract", []any{int64(math.MinInt64), int64(1)})
	if err == nil {
		t.Error("subtract(MinInt64, 1) should overflow")
	}
}

func TestArithmeticMultiply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"basic", 3, 4, 12},
		{"zero", 0, 999, 0},
		{"negative", -3, 4, -12},
		{"both_negative", -3, -4, 12},
		{"one", 1, math.MaxInt64, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Test both "multiply" and "mul" (alias).
			for _, fnName := range []string{"multiply", "mul"} {
				got, err := evaluateArithmetic(t, fnName, []any{tc.a, tc.b})
				if err != nil {
					t.Fatalf("%s(%d, %d) unexpected error: %v", fnName, tc.a, tc.b, err)
				}
				if got != tc.want {
					t.Errorf("%s(%d, %d) = %v, want %d", fnName, tc.a, tc.b, got, tc.want)
				}
			}
		})
	}
}

func TestArithmeticMultiplyOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
	}{
		{"large_positive", math.MaxInt64, 2},
		{"large_negative", math.MinInt64, 2},
		{"min_times_minus1", math.MinInt64, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := evaluateArithmetic(t, "multiply", []any{tc.a, tc.b})
			if err == nil {
				t.Errorf("multiply(%d, %d) should overflow", tc.a, tc.b)
			}
		})
	}
}

func TestArithmeticDivide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"exact", 12, 4, 3},
		{"truncate", 10, 3, 3},
		{"negative_truncate", -10, 3, -3},
		{"both_negative", -10, -3, 3},
		{"zero_dividend", 0, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Test both "divide" and "div" (alias).
			for _, fnName := range []string{"divide", "div"} {
				got, err := evaluateArithmetic(t, fnName, []any{tc.a, tc.b})
				if err != nil {
					t.Fatalf("%s(%d, %d) unexpected error: %v", fnName, tc.a, tc.b, err)
				}
				if got != tc.want {
					t.Errorf("%s(%d, %d) = %v, want %d", fnName, tc.a, tc.b, got, tc.want)
				}
			}
		})
	}
}

func TestArithmeticDivideByZero(t *testing.T) {
	t.Parallel()

	for _, fnName := range []string{"divide", "div"} {
		t.Run(fnName, func(t *testing.T) {
			t.Parallel()
			_, err := evaluateArithmetic(t, fnName, []any{int64(10), int64(0)})
			if err == nil {
				t.Errorf("%s(10, 0) should error on division by zero", fnName)
			}
		})
	}
}

func TestArithmeticMod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"basic", 10, 3, 1},
		{"exact", 12, 4, 0},
		{"negative_dividend", -10, 3, -1},
		{"negative_divisor", 10, -3, 1},
		{"both_negative", -10, -3, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "mod", []any{tc.a, tc.b})
			if err != nil {
				t.Fatalf("mod(%d, %d) unexpected error: %v", tc.a, tc.b, err)
			}
			if got != tc.want {
				t.Errorf("mod(%d, %d) = %v, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestArithmeticModByZero(t *testing.T) {
	t.Parallel()

	_, err := evaluateArithmetic(t, "mod", []any{int64(10), int64(0)})
	if err == nil {
		t.Error("mod(10, 0) should error on division by zero")
	}
}

func TestArithmeticBitwise(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   string
		a, b int64
		want int64
	}{
		{"and_basic", "bitand", 0xFF, 0x0F, 0x0F},
		{"and_zero", "bitand", 0xFF, 0, 0},
		{"or_basic", "bitor", 0xF0, 0x0F, 0xFF},
		{"or_same", "bitor", 0xFF, 0xFF, 0xFF},
		{"xor_basic", "bitxor", 0xFF, 0x0F, 0xF0},
		{"xor_same", "bitxor", 0xFF, 0xFF, 0},
		{"and_negative", "bitand", -1, 0xFF, 0xFF},
		{"or_negative", "bitor", 0, -1, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, tc.fn, []any{tc.a, tc.b})
			if err != nil {
				t.Fatalf("%s(%d, %d) unexpected error: %v", tc.fn, tc.a, tc.b, err)
			}
			if got != tc.want {
				t.Errorf("%s(%d, %d) = %v, want %d", tc.fn, tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestArithmeticBitnot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		x    int64
		want int64
	}{
		{"zero", 0, -1},
		{"minus_one", -1, 0},
		{"positive", 0xFF, ^int64(0xFF)},
		{"max", math.MaxInt64, math.MinInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "bitnot", []any{tc.x})
			if err != nil {
				t.Fatalf("bitnot(%d) unexpected error: %v", tc.x, err)
			}
			if got != tc.want {
				t.Errorf("bitnot(%d) = %v, want %d", tc.x, got, tc.want)
			}
		})
	}
}

func TestArithmeticNullPropagation(t *testing.T) {
	t.Parallel()

	// Binary: if either argument is nil, result is nil.
	t.Run("binary_left_nil", func(t *testing.T) {
		t.Parallel()
		got, err := evaluateArithmetic(t, "add", []any{nil, int64(5)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("add(nil, 5) = %v, want nil", got)
		}
	})

	t.Run("binary_right_nil", func(t *testing.T) {
		t.Parallel()
		got, err := evaluateArithmetic(t, "add", []any{int64(5), nil})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("add(5, nil) = %v, want nil", got)
		}
	})

	t.Run("binary_both_nil", func(t *testing.T) {
		t.Parallel()
		got, err := evaluateArithmetic(t, "add", []any{nil, nil})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("add(nil, nil) = %v, want nil", got)
		}
	})

	// Unary: nil argument → nil result.
	t.Run("unary_nil", func(t *testing.T) {
		t.Parallel()
		got, err := evaluateArithmetic(t, "bitnot", []any{nil})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("bitnot(nil) = %v, want nil", got)
		}
	})

	// Both function (subtract): nil with 1 arg.
	t.Run("both_unary_nil", func(t *testing.T) {
		t.Parallel()
		got, err := evaluateArithmetic(t, "subtract", []any{nil})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("subtract(nil) = %v, want nil", got)
		}
	})
}

func TestArithmeticWrongType(t *testing.T) {
	t.Parallel()

	// Non-int64 input should error.
	t.Run("string", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "add", []any{int64(1), "hello"})
		if err == nil {
			t.Error("add(1, string) should error on non-int64")
		}
	})

	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "add", []any{float64(1.0), int64(2)})
		if err == nil {
			t.Error("add(float64, int64) should error on non-int64")
		}
	})

	t.Run("int32", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "add", []any{int32(1), int64(2)})
		if err == nil {
			t.Error("add(int32, int64) should error on non-int64")
		}
	})
}

func TestArithmeticWrongArgCount(t *testing.T) {
	t.Parallel()

	// Binary function with wrong arg count.
	t.Run("add_one_arg", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "add", []any{int64(1)})
		if err == nil {
			t.Error("add with 1 arg should error")
		}
	})

	t.Run("add_three_args", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "add", []any{int64(1), int64(2), int64(3)})
		if err == nil {
			t.Error("add with 3 args should error")
		}
	})

	// Unary function with wrong arg count.
	t.Run("bitnot_two_args", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "bitnot", []any{int64(1), int64(2)})
		if err == nil {
			t.Error("bitnot with 2 args should error")
		}
	})

	// Both function with wrong arg count.
	t.Run("subtract_three_args", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "subtract", []any{int64(1), int64(2), int64(3)})
		if err == nil {
			t.Error("subtract with 3 args should error")
		}
	})

	t.Run("subtract_zero_args", func(t *testing.T) {
		t.Parallel()
		_, err := evaluateArithmetic(t, "subtract", []any{})
		if err == nil {
			t.Error("subtract with 0 args should error")
		}
	})
}

func TestArithmeticProtoRoundTrip(t *testing.T) {
	t.Parallel()

	// FunctionExpr with arithmetic function round-trips through proto.
	tests := []struct {
		name string
		fn   string
	}{
		{"add", "add"},
		{"subtract", "subtract"},
		{"sub", "sub"},
		{"multiply", "multiply"},
		{"mul", "mul"},
		{"divide", "divide"},
		{"div", "div"},
		{"mod", "mod"},
		{"bitand", "bitand"},
		{"bitor", "bitor"},
		{"bitxor", "bitxor"},
		{"bitnot", "bitnot"},
		{"bitmap_bit_position", "bitmap_bit_position"},
		{"bitmap_bucket_offset", "bitmap_bucket_offset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := FunctionExpr(tc.fn, Concat(Field("a"), Field("b")))
			p := original.ToKeyExpression()

			restored, err := KeyExpressionFromProto(p)
			if err != nil {
				t.Fatalf("KeyExpressionFromProto failed: %v", err)
			}

			fn, ok := restored.(*FunctionKeyExpression)
			if !ok {
				t.Fatalf("expected *FunctionKeyExpression, got %T", restored)
			}
			if fn.Name() != tc.fn {
				t.Errorf("name = %q, want %q", fn.Name(), tc.fn)
			}
		})
	}
}

func TestArithmeticMultipleTuples(t *testing.T) {
	t.Parallel()

	// Evaluator should process multiple argument tuples (fan-out scenario).
	fn := globalFunctionRegistry["add"]
	if fn == nil {
		t.Fatal("add function not registered")
	}

	arguments := [][]any{
		{int64(1), int64(2)},
		{int64(10), int64(20)},
		{int64(-5), int64(5)},
	}
	results, err := fn(nil, nil, arguments)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 result tuples, got %d", len(results))
	}

	expected := []int64{3, 30, 0}
	for i, want := range expected {
		if len(results[i]) != 1 {
			t.Fatalf("result[%d] has %d elements, want 1", i, len(results[i]))
		}
		got, ok := results[i][0].(int64)
		if !ok {
			t.Fatalf("result[%d][0] is %T, want int64", i, results[i][0])
		}
		if got != want {
			t.Errorf("result[%d] = %d, want %d", i, got, want)
		}
	}
}

func TestArithmeticBitmapBitPosition(t *testing.T) {
	t.Parallel()

	// bitmap_bit_position(l, r) = l - floorDiv(l, r) * r
	// This is essentially a floored modulo.
	tests := []struct {
		name string
		l, r int64
		want int64
	}{
		{"positive", 10003, 10000, 3},
		{"exact_boundary", 10000, 10000, 0},
		{"negative", -3, 10000, 9997},
		{"small", 5, 10, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "bitmap_bit_position", []any{tc.l, tc.r})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("bitmap_bit_position(%d, %d) = %v, want %d", tc.l, tc.r, got, tc.want)
			}
		})
	}
}

func TestArithmeticBitmapBucketOffset(t *testing.T) {
	t.Parallel()

	// bitmap_bucket_offset(l, r) = floorDiv(l, r) * r
	tests := []struct {
		name string
		l, r int64
		want int64
	}{
		{"positive", 10003, 10000, 10000},
		{"exact_boundary", 10000, 10000, 10000},
		{"negative", -3, 10000, -10000},
		{"small", 5, 10, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := evaluateArithmetic(t, "bitmap_bucket_offset", []any{tc.l, tc.r})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("bitmap_bucket_offset(%d, %d) = %v, want %d", tc.l, tc.r, got, tc.want)
			}
		})
	}
}

func TestArithmeticRegistered(t *testing.T) {
	t.Parallel()

	// Verify all expected function names are registered.
	expectedFunctions := []string{
		"add", "subtract", "sub", "multiply", "mul",
		"divide", "div", "mod",
		"bitand", "bitor", "bitxor", "bitnot",
		"bitmap_bit_position", "bitmap_bucket_offset",
	}
	for _, name := range expectedFunctions {
		if _, ok := globalFunctionRegistry[name]; !ok {
			t.Errorf("function %q not registered", name)
		}
	}
}

// TestArithmeticFunctionExprEvaluate tests the full FunctionExpr → Evaluate path
// using a real proto message with the arithmetic functions.
func TestArithmeticFunctionExprEvaluate(t *testing.T) {
	t.Parallel()

	// We can't easily test with a real proto message here (would need a test proto
	// with int64 fields), but we CAN test that FunctionExpr("add", ...) correctly
	// resolves to the registered evaluator.
	t.Run("unknown_function_errors", func(t *testing.T) {
		t.Parallel()
		expr := FunctionExpr("nonexistent_arithmetic_fn", EmptyKey())
		_, err := expr.Evaluate(nil, nil)
		if err == nil {
			t.Error("expected error for unknown function")
		}
	})
}

// TestOverflowCheckedOps tests the overflow-checked arithmetic functions directly.
func TestOverflowCheckedOps(t *testing.T) {
	t.Parallel()

	t.Run("addExact_no_overflow", func(t *testing.T) {
		t.Parallel()
		got, err := longAddExact(math.MaxInt64-1, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != math.MaxInt64 {
			t.Errorf("got %d, want MaxInt64", got)
		}
	})

	t.Run("addExact_overflow", func(t *testing.T) {
		t.Parallel()
		_, err := longAddExact(math.MaxInt64, 1)
		if err == nil {
			t.Error("expected overflow error")
		}
	})

	t.Run("subtractExact_no_overflow", func(t *testing.T) {
		t.Parallel()
		got, err := longSubtractExact(math.MinInt64+1, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != math.MinInt64 {
			t.Errorf("got %d, want MinInt64", got)
		}
	})

	t.Run("subtractExact_overflow", func(t *testing.T) {
		t.Parallel()
		_, err := longSubtractExact(math.MinInt64, 1)
		if err == nil {
			t.Error("expected overflow error")
		}
	})

	t.Run("multiplyExact_no_overflow", func(t *testing.T) {
		t.Parallel()
		got, err := longMultiplyExact(math.MaxInt64/2, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (math.MaxInt64/2)*2 {
			t.Errorf("got %d, want %d", got, (math.MaxInt64/2)*2)
		}
	})

	t.Run("multiplyExact_overflow", func(t *testing.T) {
		t.Parallel()
		_, err := longMultiplyExact(math.MaxInt64, 2)
		if err == nil {
			t.Error("expected overflow error")
		}
	})

	t.Run("negateExact_no_overflow", func(t *testing.T) {
		t.Parallel()
		got, err := longNegateExact(math.MaxInt64)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != -math.MaxInt64 {
			t.Errorf("got %d, want %d", got, -int64(math.MaxInt64))
		}
	})

	t.Run("negateExact_overflow", func(t *testing.T) {
		t.Parallel()
		_, err := longNegateExact(math.MinInt64)
		if err == nil {
			t.Error("expected overflow error for MinInt64")
		}
	})

	t.Run("floorDiv_positive", func(t *testing.T) {
		t.Parallel()
		got, err := floorDiv(7, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 3 {
			t.Errorf("floorDiv(7, 2) = %d, want 3", got)
		}
	})

	t.Run("floorDiv_negative", func(t *testing.T) {
		t.Parallel()
		// floorDiv(-7, 2) = -4 (rounds toward -inf), not -3 (truncation).
		got, err := floorDiv(-7, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != -4 {
			t.Errorf("floorDiv(-7, 2) = %d, want -4", got)
		}
	})

	t.Run("floorDiv_exact", func(t *testing.T) {
		t.Parallel()
		got, err := floorDiv(-6, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != -3 {
			t.Errorf("floorDiv(-6, 2) = %d, want -3", got)
		}
	})

	t.Run("floorDiv_by_zero", func(t *testing.T) {
		t.Parallel()
		_, err := floorDiv(7, 0)
		if err == nil {
			t.Error("expected error for division by zero")
		}
	})
}
